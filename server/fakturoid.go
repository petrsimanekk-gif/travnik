package main

// Fakturoid: "🧾 Vystavit fakturu" on a deal makes a deposit (zálohová) or a final
// invoice in Fakturoid, keeps its PDF in the deal's history, writes the e-mail for the
// client into the Drafts folder for the deal's owner to check and send, and every
// 15 minutes asks Fakturoid which invoices are paid (a paid deposit moves the deal on).
//
// API v3, OAuth client credentials: https://www.fakturoid.cz/api/v3

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

type fakCfg struct {
	Slug       string `json:"slug"`
	ClientID   string `json:"clientId"`
	Secret     string `json:"secret,omitempty"`
	Email      string `json:"email"`      // Fakturoid wants a contact e-mail in the User-Agent
	VatPayer   bool   `json:"vatPayer"`   // off: invoices without VAT
	DueDeposit int    `json:"dueDeposit"` // days
	DueFinal   int    `json:"dueFinal"`
	Box        string `json:"box"` // mailbox the e-mail drafts go to ("" = first shared one)
}

func fakBase() string {
	if u := os.Getenv("STINKO_FAKTUROID_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://app.fakturoid.cz/api/v3"
}

// ---------- settings (kept on the server only, like mailbox passwords) ----------

func ensureSecretsSchema(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("secrets"); err == nil {
		return nil
	}
	c := core.NewBaseCollection("secrets")
	c.Fields.Add(
		&core.TextField{Name: "key", Required: true, Max: 100},
		&core.JSONField{Name: "data", MaxSize: 1 << 20},
	)
	c.AddIndex("idx_secrets_key", true, "`key`", "")
	return app.Save(c)
}

func loadFakCfg(app core.App) (*fakCfg, *core.Record) {
	cfg := &fakCfg{DueDeposit: 7, DueFinal: 14}
	r, err := app.FindFirstRecordByData("secrets", "key", "fakturoid")
	if err != nil {
		return cfg, nil
	}
	raw, _ := json.Marshal(r.Get("data"))
	_ = json.Unmarshal(raw, cfg)
	return cfg, r
}

func (c *fakCfg) ready() bool { return c.Slug != "" && c.ClientID != "" && c.Secret != "" }

// ---------- API client ----------

var (
	fakTokMu  sync.Mutex
	fakTok    string
	fakTokFor string
	fakTokExp time.Time
	fakHTTP   = &http.Client{Timeout: 30 * time.Second}
)

type fakErr struct {
	status int
	msg    string
}

func (e fakErr) Error() string { return e.msg }

func fakToken(c *fakCfg, fresh bool) (string, error) {
	fakTokMu.Lock()
	defer fakTokMu.Unlock()
	who := c.ClientID + ":" + c.Secret
	if !fresh && fakTok != "" && fakTokFor == who && time.Now().Before(fakTokExp) {
		return fakTok, nil
	}
	req, _ := http.NewRequest("POST", fakBase()+"/oauth/token", strings.NewReader(`{"grant_type":"client_credentials"}`))
	req.SetBasicAuth(c.ClientID, c.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fakUA(c))
	res, err := fakHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("Fakturoid není dostupný: %w", err)
	}
	defer res.Body.Close()
	var j struct {
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&j)
	if res.StatusCode != 200 || j.Token == "" {
		return "", fakErr{res.StatusCode, "Fakturoid odmítl přihlášení – zkontrolujte Client ID a Client Secret."}
	}
	fakTok, fakTokFor, fakTokExp = j.Token, who, time.Now().Add(time.Duration(max(j.Expires, 120)-60)*time.Second)
	return fakTok, nil
}

func fakUA(c *fakCfg) string {
	mail := c.Email
	if mail == "" {
		mail = "crm@primashade.cz"
	}
	return "Prima Shade CRM (" + mail + ")"
}

// fakDo calls the account's API; out (if not nil) gets the JSON answer.
func fakDo(c *fakCfg, method, path string, body any, out any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	for try := 0; try < 2; try++ {
		tok, err := fakToken(c, try > 0)
		if err != nil {
			return 0, nil, err
		}
		req, _ := http.NewRequest(method, fakBase()+"/accounts/"+url.PathEscape(c.Slug)+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", fakUA(c))
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := fakHTTP.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("Fakturoid není dostupný: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 20<<20))
		res.Body.Close()
		if res.StatusCode == 401 && try == 0 {
			continue // the token expired early: once more with a new one
		}
		if res.StatusCode >= 400 {
			return res.StatusCode, data, fakErr{res.StatusCode, "Fakturoid: " + fakErrText(res.StatusCode, data)}
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return res.StatusCode, data, err
			}
		}
		return res.StatusCode, data, nil
	}
	return 0, nil, fakErr{401, "Fakturoid odmítl přihlášení."}
}

// fakErrText turns {"errors":{"field":["msg"]}} into a readable line.
func fakErrText(status int, data []byte) string {
	var j struct {
		Errors map[string][]string `json:"errors"`
		Error  string              `json:"error"`
	}
	_ = json.Unmarshal(data, &j)
	var parts []string
	for k, v := range j.Errors {
		parts = append(parts, k+": "+strings.Join(v, ", "))
	}
	if j.Error != "" {
		parts = append(parts, j.Error)
	}
	if len(parts) == 0 {
		switch status {
		case 401, 403:
			return "přístup odepřen (zkontrolujte přihlašovací údaje a název účtu)"
		case 404:
			return "nenalezeno (zkontrolujte název účtu ve Fakturoidu)"
		case 429:
			return "příliš mnoho požadavků, zkuste to za chvíli"
		}
		return fmt.Sprintf("chyba %d", status)
	}
	return strings.Join(parts, "; ")
}

// ---------- endpoints ----------

func bindFakRoutes(se *core.ServeEvent) {
	r := se.Router
	r.GET("/api/stinko/fak/cfg", handleFakCfg)
	r.POST("/api/stinko/fak/cfg", handleFakCfgSave)
	r.POST("/api/stinko/fak/invoice", handleFakInvoice)
	r.POST("/api/stinko/fak/check", handleFakCheck)
}

func adminAccount(e *core.RequestEvent) (map[string]any, error) {
	a, err := authAccount(e)
	if err != nil {
		return nil, err
	}
	if a["role"] != "admin" {
		return nil, e.ForbiddenError("permission_denied", nil)
	}
	return a, nil
}

func handleFakCfg(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	c, _ := loadFakCfg(e.App)
	out := map[string]any{"ready": c.ready(), "vatPayer": c.VatPayer, "dueDeposit": c.DueDeposit, "dueFinal": c.DueFinal}
	if a["role"] == "admin" {
		out["slug"], out["clientId"], out["email"], out["box"], out["hasSecret"] = c.Slug, c.ClientID, c.Email, c.Box, c.Secret != ""
	}
	return e.JSON(http.StatusOK, out)
}

func handleFakCfgSave(e *core.RequestEvent) error {
	if _, err := adminAccount(e); err != nil {
		return err
	}
	var b fakCfg
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	old, rec := loadFakCfg(e.App)
	b.Slug = strings.Trim(strings.TrimSpace(b.Slug), "/")
	if i := strings.Index(b.Slug, "fakturoid.cz/"); i >= 0 { // a pasted address: app.fakturoid.cz/<slug>/...
		b.Slug = strings.Split(b.Slug[i+len("fakturoid.cz/"):], "/")[0]
	}
	b.ClientID, b.Email = strings.TrimSpace(b.ClientID), strings.TrimSpace(b.Email)
	if strings.TrimSpace(b.Secret) == "" {
		b.Secret = old.Secret
	}
	b.Secret = strings.TrimSpace(b.Secret)
	if b.DueDeposit <= 0 {
		b.DueDeposit = 7
	}
	if b.DueFinal <= 0 {
		b.DueFinal = 14
	}
	// check the account before saving
	var acc struct {
		Name    string `json:"name"`
		VatMode string `json:"vat_mode"`
	}
	if _, _, err := fakDo(&b, "GET", "/account.json", nil, &acc); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	if rec == nil {
		coll, err := e.App.FindCachedCollectionByNameOrId("secrets")
		if err != nil {
			return err
		}
		rec = core.NewRecord(coll)
		rec.Set("key", "fakturoid")
	}
	rec.Set("data", b)
	if err := e.App.Save(rec); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"ok": true, "account": acc.Name, "vatMode": acc.VatMode})
}

type fakLine struct {
	Name  string  `json:"name"`
	Qty   float64 `json:"qty"`
	Unit  string  `json:"unit"`
	Price float64 `json:"price"` // Kč per unit, without VAT for a VAT payer
	Vat   float64 `json:"vat"`
}

type fakInvReq struct {
	DealID string    `json:"dealId"`
	Kind   string    `json:"kind"` // deposit | final
	Label  string    `json:"label"`
	Lines  []fakLine `json:"lines"`
	Due    int       `json:"due"`
	Note   string    `json:"note"`
	Mail   struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		HTML    string `json:"html"`
	} `json:"mail"`
}

func canEditDeals(a map[string]any) bool {
	if a["role"] == "admin" {
		return true
	}
	perms, _ := a["perms"].(map[string]any)
	return perms["deals"] == "edit"
}

func handleFakInvoice(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	if !canEditDeals(a) {
		return e.ForbiddenError("permission_denied", nil)
	}
	var b fakInvReq
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	if b.Kind != "deposit" && b.Kind != "final" {
		return e.BadRequestError("Neznámý typ faktury.", nil)
	}
	if len(b.Lines) == 0 || len(b.Lines) > 100 {
		return e.BadRequestError("Faktura nemá žádné položky.", nil)
	}
	c, _ := loadFakCfg(e.App)
	if !c.ready() {
		return e.BadRequestError("Fakturoid není nastavený (Nastavení → Fakturoid).", nil)
	}
	drec, err := findDoc(e.App, "deals", b.DealID)
	if err != nil {
		return e.NotFoundError("Obchodní případ nenalezen.", nil)
	}
	deal := docData(drec)
	if !scopeFor(e.App, e.Auth.GetString("acc")).visible("deals", b.DealID, deal) {
		return e.ForbiddenError("permission_denied", nil)
	}
	crec, err := findDoc(e.App, "clients", str(deal, "clientId"))
	if err != nil {
		return e.BadRequestError("Obchodní případ nemá klienta – fakturu není komu vystavit.", nil)
	}
	client := docData(crec)

	subjectID, err := fakSubject(c, e.App, str(deal, "clientId"), client)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	inv, err := fakCreateInvoice(c, subjectID, deal, &b)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	kindName := map[string]string{"deposit": "Zálohová faktura", "final": "Faktura"}[b.Kind]
	pdfName := strings.ReplaceAll(kindName, " ", "-") + "-" + inv.Number + ".pdf"
	pdfKey := ""
	if pdf, err := fakPDF(c, inv.ID); err == nil {
		pdfKey, _ = storeBlob(e.App, pdfName, "application/pdf", pdf)
	} else {
		e.App.Logger().Warn("fakturoid pdf", "invoice", inv.ID, "err", err)
	}
	by := str(a, "name")
	now := time.Now().UnixMilli()
	entry := map[string]any{"fakId": inv.ID, "number": inv.Number, "kind": b.Kind, "label": b.Label, "total": inv.Total, "vs": inv.VS, "due": inv.Due,
		"status": inv.Status, "url": inv.HTMLURL, "pdfId": pdfKey, "pdfName": pdfName, "at": now, "by": by}

	// the e-mail for the client waits in Koncepty for the owner
	draftID, mailErr := "", ""
	to := strings.TrimSpace(str(client, "email"))
	if to == "" {
		mailErr = "klient nemá e-mail"
	} else if box := fakMailBox(e.App, c); box == "" {
		mailErr = "není připojená žádná společná e-mailová schránka"
	} else if pdfKey == "" {
		mailErr = "PDF faktury se nepodařilo stáhnout"
	} else {
		v := map[string]string{"faktura": inv.Number, "castka": czk(inv.Total), "splatnost": czDate(inv.Due), "vs": inv.VS, "typ_faktury": strings.ToLower(kindName)}
		subj, body, html := fillAuto(b.Mail.Subject, v), fillAuto(b.Mail.Body, v), ""
		if strings.TrimSpace(b.Mail.HTML) != "" {
			html = fillAutoHTML(b.Mail.HTML, v)
			body = richToText(html)
		}
		if subj == "" {
			subj = kindName + " " + inv.Number
		}
		ctx, _ := json.Marshal(map[string]string{"dealId": b.DealID, "clientId": str(deal, "clientId")})
		draftID, err = saveDraft(e.App, &sendReq{Box: box, To: to, Subject: subj, Text: body, HTML: html, Att: []string{pdfKey}, AttNames: []string{pdfName}, SigID: "__box", Ctx: ctx})
		if err != nil {
			mailErr = err.Error()
		}
	}
	entry["draft"] = draftID

	// the deal: the invoice, a history entry with the PDF, a deposit moves it to "ZF vystavena"
	deal = docData(drec)
	invs, _ := deal["invoices"].([]any)
	deal["invoices"] = append(invs, entry)
	text := fmt.Sprintf("🧾 %s %s vystavena (%s)", kindName, inv.Number, czk(inv.Total))
	evs := []any{map[string]any{"at": now, "by": by, "kind": "file", "text": text, "pdfId": pdfKey, "pdfName": pdfName}}
	if b.Kind == "deposit" {
		if ev := movePhase(e.App, deal, "ZF vystavena", "Fakturoid"); ev != nil {
			evs = append(evs, ev)
		}
	}
	old, _ := deal["events"].([]any)
	deal["events"] = trimEvents(append(old, evs...))
	if err := putDoc(e.App, "deals", b.DealID, deal); err != nil {
		return err
	}
	owner := str(deal, "owner")
	if owner == "" {
		owner = by
	}
	note := text + " – e-mail pro klienta čeká v Konceptech ke schválení"
	if mailErr != "" {
		note = text + " – e-mail se nepřipravil: " + mailErr
	}
	_ = putDoc(e.App, "notifs", randUUID(), map[string]any{"to": owner, "text": note, "at": now, "read": false, "dealId": b.DealID, "leadId": "", "by": "Fakturoid"})
	return e.JSON(http.StatusOK, map[string]any{"invoice": entry, "draft": draftID, "mailError": mailErr})
}

func trimEvents(l []any) []any {
	if len(l) > 100 {
		return l[len(l)-100:]
	}
	return l
}

func czk(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	whole, frac, _ := strings.Cut(s, ".")
	neg := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	out := b.String()
	if frac != "00" {
		out += "," + frac
	}
	if neg {
		out = "-" + out
	}
	return out + " Kč"
}

func czDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return fmt.Sprintf("%d. %d. %d", t.Day(), int(t.Month()), t.Year())
}

// movePhase moves the deal to the named phase if it is still before it; gives the history entry.
func movePhase(app core.App, deal map[string]any, name, why string) map[string]any {
	cfg := crmConfig(app)
	phases, _ := cfg["phases"].([]any)
	cur, target := -1, -1
	curName, targetID := "", ""
	for i, x := range phases {
		p, _ := x.(map[string]any)
		if str(p, "id") == str(deal, "phaseId") {
			cur, curName = i, str(p, "name")
			if str(p, "kind") != "open" {
				return nil
			}
		}
		if strings.EqualFold(strings.TrimSpace(str(p, "name")), name) {
			target, targetID = i, str(p, "id")
		}
	}
	if target < 0 || cur >= target {
		return nil
	}
	now := time.Now().UnixMilli()
	deal["phaseId"], deal["phaseSince"] = targetID, now
	hist, _ := deal["phaseHistory"].([]any)
	hist = append(hist, map[string]any{"phaseId": targetID, "at": now})
	if len(hist) > 60 {
		hist = hist[len(hist)-60:]
	}
	deal["phaseHistory"] = hist
	if curName == "" {
		curName = "?"
	}
	return map[string]any{"at": now, "by": why, "kind": "phase", "text": "Stav: " + curName + " → " + name + " (automatizace " + why + ")"}
}

// fakSubject finds the client in Fakturoid (remembered id, IČO, e-mail) or makes it.
func fakSubject(c *fakCfg, app core.App, clientID string, cl map[string]any) (int64, error) {
	if id, ok := cl["fakturoidId"].(float64); ok && id > 0 {
		if st, _, err := fakDo(c, "GET", fmt.Sprintf("/subjects/%d.json", int64(id)), nil, nil); err == nil && st == 200 {
			return int64(id), nil
		}
	}
	var found []struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
		RegNo string `json:"registration_no"`
	}
	for _, q := range []string{str(cl, "ico"), str(cl, "email")} {
		if strings.TrimSpace(q) == "" {
			continue
		}
		if _, _, err := fakDo(c, "GET", "/subjects/search.json?query="+url.QueryEscape(strings.TrimSpace(q)), nil, &found); err != nil {
			return 0, err
		}
		for _, f := range found {
			if (str(cl, "ico") != "" && f.RegNo == strings.TrimSpace(str(cl, "ico"))) || strings.EqualFold(f.Email, strings.TrimSpace(str(cl, "email"))) {
				rememberSubject(app, clientID, f.ID)
				return f.ID, nil
			}
		}
	}
	name := strings.TrimSpace(str(cl, "name"))
	if name == "" {
		return 0, fmt.Errorf("klient nemá jméno")
	}
	body := map[string]any{"name": name, "email": str(cl, "email"), "phone": str(cl, "phone"), "street": str(cl, "street"), "city": str(cl, "city"), "zip": str(cl, "zip"),
		"country": "CZ", "registration_no": str(cl, "ico"), "vat_no": str(cl, "dic")}
	var made struct {
		ID int64 `json:"id"`
	}
	if _, _, err := fakDo(c, "POST", "/subjects.json", body, &made); err != nil {
		return 0, err
	}
	rememberSubject(app, clientID, made.ID)
	return made.ID, nil
}

func rememberSubject(app core.App, clientID string, id int64) {
	if r, err := findDoc(app, "clients", clientID); err == nil {
		d := docData(r)
		d["fakturoidId"] = id
		_ = putDoc(app, "clients", clientID, d)
	}
}

type fakInvoice struct {
	ID      int64   `json:"id"`
	Number  string  `json:"number"`
	VS      string  `json:"variable_symbol"`
	Total   float64 `json:"-"`
	TotalS  any     `json:"total"`
	Status  string  `json:"status"`
	Due     string  `json:"due_on"`
	PaidOn  string  `json:"paid_on"`
	HTMLURL string  `json:"html_url"`
}

func (i *fakInvoice) fix() {
	switch v := i.TotalS.(type) {
	case string:
		fmt.Sscan(v, &i.Total)
	case float64:
		i.Total = v
	}
}

func fakCreateInvoice(c *fakCfg, subjectID int64, deal map[string]any, b *fakInvReq) (*fakInvoice, error) {
	due := b.Due
	if due <= 0 {
		due = c.DueFinal
		if b.Kind == "deposit" {
			due = c.DueDeposit
		}
	}
	var lines []map[string]any
	for _, l := range b.Lines {
		if strings.TrimSpace(l.Name) == "" || l.Qty == 0 {
			continue
		}
		ln := map[string]any{"name": clip(l.Name, 250), "quantity": l.Qty, "unit_name": clip(l.Unit, 20), "unit_price": math.Round(l.Price*100) / 100}
		if c.VatPayer {
			ln["vat_rate"] = l.Vat
		} else {
			ln["vat_rate"] = 0
		}
		lines = append(lines, ln)
	}
	docType := "invoice"
	if b.Kind == "deposit" {
		docType = "proforma"
	}
	body := map[string]any{"subject_id": subjectID, "document_type": docType, "due": due, "lines": lines, "order_number": str(deal, "code"), "note": clip(b.Note, 2000)}
	var inv fakInvoice
	if _, _, err := fakDo(c, "POST", "/invoices.json", body, &inv); err != nil {
		return nil, err
	}
	inv.fix()
	return &inv, nil
}

// fakPDF downloads the invoice PDF; Fakturoid makes it a moment after the invoice (204 meanwhile).
func fakPDF(c *fakCfg, id int64) ([]byte, error) {
	for i := 0; i < 15; i++ {
		st, data, err := fakDo(c, "GET", fmt.Sprintf("/invoices/%d/download.pdf", id), nil, nil)
		if err != nil {
			return nil, err
		}
		if st == 200 && len(data) > 0 {
			return data, nil
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("PDF zatím není připravené")
}

// storeBlob keeps a file the server made (served at /_blob/{key} to logged-in users).
func storeBlob(app core.App, name, ctype string, data []byte) (string, error) {
	coll, err := app.FindCachedCollectionByNameOrId("blobs")
	if err != nil {
		return "", err
	}
	f, err := filesystem.NewFileFromBytes(data, name)
	if err != nil {
		return "", err
	}
	key := randHex(16)
	rec := core.NewRecord(coll)
	rec.Set("key", key)
	rec.Set("name", name)
	rec.Set("type", ctype)
	rec.Set("file", f)
	if err := app.Save(rec); err != nil {
		return "", err
	}
	return key, nil
}

// fakMailBox: the mailbox set in Nastavení, or the first shared one.
func fakMailBox(app core.App, c *fakCfg) string {
	if c.Box != "" {
		if _, _, _, err := loadBox(app, c.Box); err == nil {
			return c.Box
		}
	}
	box, _ := autoBox(app, "")
	return box
}

// ---------- paid or not: asked every 15 minutes (and on "Zkontrolovat úhrady") ----------

func startFakLoop(app core.App) {
	go func() {
		time.Sleep(time.Minute)
		for {
			fakCheckAll(app)
			time.Sleep(15 * time.Minute)
		}
	}()
}

var fakCheckMu sync.Mutex

func fakCheckAll(app core.App) int {
	fakCheckMu.Lock()
	defer fakCheckMu.Unlock()
	c, _ := loadFakCfg(app)
	if !c.ready() {
		return 0
	}
	recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "deals"})
	changed := 0
	for _, r := range recs {
		d := docData(r)
		invs, _ := d["invoices"].([]any)
		dirty := false
		var evs []any
		for _, x := range invs {
			inv, _ := x.(map[string]any)
			if inv == nil || inv["status"] == "paid" || inv["status"] == "cancelled" {
				continue
			}
			id, _ := inv["fakId"].(float64)
			if id <= 0 {
				continue
			}
			var cur fakInvoice
			st, _, err := fakDo(c, "GET", fmt.Sprintf("/invoices/%d.json", int64(id)), nil, &cur)
			if st == 404 {
				inv["status"], dirty = "cancelled", true
				continue
			}
			if err != nil {
				app.Logger().Warn("fakturoid check", "invoice", int64(id), "err", err)
				return changed // Fakturoid unreachable or a bad setting: try again next time
			}
			if cur.Status == "" || cur.Status == inv["status"] {
				continue
			}
			inv["status"], dirty = cur.Status, true
			if cur.Status != "paid" {
				continue
			}
			now := time.Now().UnixMilli()
			inv["paidAt"] = now
			kind := "Faktura"
			if inv["kind"] == "deposit" {
				kind = "Zálohová faktura"
			}
			text := fmt.Sprintf("✓ %s %v uhrazena (%s)", kind, inv["number"], czk(toF(inv["total"])))
			evs = append(evs, map[string]any{"at": now, "by": "Fakturoid", "kind": "edit", "text": text})
			if inv["kind"] == "deposit" {
				if ev := movePhase(app, d, "ZF uhrazena", "Fakturoid"); ev != nil {
					evs = append(evs, ev)
				}
			}
			_ = putDoc(app, "notifs", randUUID(), map[string]any{"to": str(d, "owner"), "text": "💰 " + text + " – " + str(d, "code") + " " + str(d, "name"), "at": now, "read": false, "dealId": r.GetString("key"), "leadId": "", "by": "Fakturoid"})
		}
		if dirty {
			old, _ := d["events"].([]any)
			d["events"] = trimEvents(append(old, evs...))
			_ = putDoc(app, "deals", r.GetString("key"), d)
			changed++
		}
	}
	return changed
}

func toF(v any) float64 {
	f, _ := v.(float64)
	return f
}

func handleFakCheck(e *core.RequestEvent) error {
	if _, err := authAccount(e); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"changed": fakCheckAll(e.App)})
}
