package main

// Účetnictví: issued invoices (acc_out), received documents (acc_in), bank
// movements (acc_bank) and the company's invoicing settings (acc_cfg). All of
// it is readable only by admins and accounts with the "accounting" right; the
// enrich hook in main.go blanks these records for everyone else.
//
// The page writes these records through /api/stinko/set like any other. Only
// what must be atomic or touches other sections runs here: numbering a new
// invoice, its PDF and e-mail draft, payment and storno (they also update the
// deal the invoice belongs to, which an accountant may not be allowed to edit).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

func isAccCol(col string) bool { return strings.HasPrefix(col, "acc_") }

func accPerm(a map[string]any) string {
	if a == nil {
		return "none"
	}
	if a["role"] == "admin" {
		return "edit"
	}
	perms, _ := a["perms"].(map[string]any)
	if p, _ := perms["accounting"].(string); p == "view" || p == "edit" {
		return p
	}
	return "none"
}

var accMu sync.Mutex

func bindAccRoutes(se *core.ServeEvent) {
	r := se.Router
	r.POST("/api/stinko/acc/issue", handleAccIssue)
	r.POST("/api/stinko/acc/attach", handleAccAttach)
	r.POST("/api/stinko/acc/paid", handleAccPaid)
	r.POST("/api/stinko/acc/storno", handleAccStorno)
	r.GET("/api/stinko/acc/ares", handleAres)
}

func accEditor(e *core.RequestEvent) (map[string]any, error) {
	a, err := authAccount(e)
	if err != nil {
		return nil, err
	}
	if accPerm(a) != "edit" {
		return nil, e.ForbiddenError("permission_denied", nil)
	}
	return a, nil
}

var digitsRe = regexp.MustCompile(`\D`)

// nextInvNumber: {prefix}{year}{seq padded to 3}, e.g. 2026001 or Z2026001.
func nextInvNumber(app core.App, prefix string, year int) string {
	head := fmt.Sprintf("%s%d", prefix, year)
	max := 0
	recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "acc_out"})
	for _, r := range recs {
		n := str(docData(r), "number")
		if !strings.HasPrefix(n, head) {
			continue
		}
		if v, err := strconv.Atoi(n[len(head):]); err == nil && v > max {
			max = v
		}
	}
	return fmt.Sprintf("%s%03d", head, max+1)
}

func accCfg(app core.App) map[string]any {
	if r, err := findDoc(app, "acc_cfg", "main"); err == nil {
		return docData(r)
	}
	return map[string]any{}
}

var accKindName = map[string]string{"invoice": "Faktura", "deposit": "Zálohová faktura", "credit": "Opravný daňový doklad"}

func handleAccIssue(e *core.RequestEvent) error {
	a, err := accEditor(e)
	if err != nil {
		return err
	}
	var b struct {
		ID  string         `json:"id"`
		Inv map[string]any `json:"inv"`
	}
	if err := e.BindBody(&b); err != nil || b.ID == "" || b.Inv == nil {
		return e.BadRequestError("", err)
	}
	inv := b.Inv
	kind := str(inv, "kind")
	if accKindName[kind] == "" {
		return e.BadRequestError("Neznámý typ faktury.", nil)
	}
	if lines, _ := inv["lines"].([]any); len(lines) == 0 || len(lines) > 200 {
		return e.BadRequestError("Faktura nemá žádné položky.", nil)
	}
	accMu.Lock()
	defer accMu.Unlock()
	if _, err := findDoc(e.App, "acc_out", b.ID); err == nil {
		return e.BadRequestError("Faktura už je vystavená.", nil)
	}
	issued := str(inv, "issued")
	year := time.Now().Year()
	if t, err := time.Parse("2006-01-02", issued); err == nil {
		year = t.Year()
	}
	cfg := accCfg(e.App)
	prefix := str(cfg, "prefixInvoice")
	if kind == "deposit" {
		prefix = str(cfg, "prefixDeposit")
		if _, set := cfg["prefixDeposit"]; !set {
			prefix = "Z"
		}
	} else if kind == "credit" {
		prefix = str(cfg, "prefixCredit")
		if _, set := cfg["prefixCredit"]; !set {
			prefix = "D"
		}
	}
	number := nextInvNumber(e.App, prefix, year)
	inv["number"] = number
	// the variable symbol is the number's digits; a deposit gets a leading 1 so that
	// Z2026001 (VS 12026001) never collides with invoice 2026001 (VS 2026001)
	inv["vs"] = map[string]string{"deposit": "1", "credit": "2"}[kind] + digitsRe.ReplaceAllString(number, "")
	now := time.Now().UnixMilli()
	by := str(a, "name")
	inv["status"] = "issued"
	inv["at"], inv["by"] = now, by
	inv["events"] = []any{map[string]any{"at": now, "by": by, "text": accKindName[kind] + " " + number + " vystavena"}}
	if err := putDoc(e.App, "acc_out", b.ID, inv); err != nil {
		return err
	}

	// the deal it belongs to: the same entry the Fakturoid flow writes, so the deal page shows it
	if dealID := str(inv, "dealId"); dealID != "" {
		if drec, err := findDoc(e.App, "deals", dealID); err == nil {
			deal := docData(drec)
			entry := map[string]any{"accId": b.ID, "number": number, "kind": map[bool]string{true: "deposit", false: "final"}[kind == "deposit"], "label": str(inv, "label"),
				"total": toF(inv["total"]), "vs": inv["vs"], "due": str(inv, "due"), "status": "open", "at": now, "by": by}
			invs, _ := deal["invoices"].([]any)
			deal["invoices"] = append(invs, entry)
			evs := []any{map[string]any{"at": now, "by": by, "kind": "edit", "text": fmt.Sprintf("🧾 %s %s vystavena (%s)", accKindName[kind], number, czk(toF(inv["total"])))}}
			if kind == "deposit" {
				if ev := movePhase(e.App, deal, "ZF vystavena", "Účetnictví"); ev != nil {
					evs = append(evs, ev)
				}
			}
			old, _ := deal["events"].([]any)
			deal["events"] = trimEvents(append(old, evs...))
			_ = putDoc(e.App, "deals", dealID, deal)
		}
	}
	return e.JSON(http.StatusOK, map[string]any{"id": b.ID, "number": number, "vs": inv["vs"]})
}

// updateDealInv changes the deal's copy of an invoice (status, PDF) and returns the deal to save.
func updateDealInv(app core.App, dealID, accID string, set func(entry map[string]any)) map[string]any {
	drec, err := findDoc(app, "deals", dealID)
	if err != nil {
		return nil
	}
	deal := docData(drec)
	invs, _ := deal["invoices"].([]any)
	found := false
	for _, x := range invs {
		if m, _ := x.(map[string]any); m != nil && str(m, "accId") == accID {
			set(m)
			found = true
		}
	}
	if !found {
		return nil
	}
	return deal
}

func handleAccAttach(e *core.RequestEvent) error {
	a, err := accEditor(e)
	if err != nil {
		return err
	}
	var b struct {
		ID      string `json:"id"`
		PdfID   string `json:"pdfId"`
		PdfName string `json:"pdfName"`
		Mail    struct {
			Box, To, Subject, Body, HTML string
		} `json:"mail"`
	}
	if err := e.BindBody(&b); err != nil || b.ID == "" {
		return e.BadRequestError("", err)
	}
	rec, err := findDoc(e.App, "acc_out", b.ID)
	if err != nil {
		return e.NotFoundError("Faktura nenalezena.", nil)
	}
	inv := docData(rec)
	if b.PdfID != "" {
		inv["pdfId"], inv["pdfName"] = b.PdfID, b.PdfName
	}
	draftID, mailErr := "", ""
	if strings.TrimSpace(b.Mail.To) != "" {
		box := b.Mail.Box
		if box == "" {
			box, _ = autoBox(e.App, "")
		}
		pdf := str(inv, "pdfId")
		if box == "" {
			mailErr = "není připojená žádná e-mailová schránka"
		} else if pdf == "" {
			mailErr = "faktura nemá PDF"
		} else {
			ctx, _ := json.Marshal(map[string]string{"dealId": str(inv, "dealId"), "clientId": str(inv, "clientId")})
			v := map[string]string{"faktura": str(inv, "number"), "castka": czk(toF(inv["total"])), "splatnost": czDate(str(inv, "due")), "vs": str(inv, "vs"), "typ_faktury": strings.ToLower(accKindName[str(inv, "kind")])}
			body, html := fillAuto(b.Mail.Body, v), ""
			if strings.TrimSpace(b.Mail.HTML) != "" {
				html = fillAutoHTML(b.Mail.HTML, v)
				body = richToText(html)
			}
			subj := fillAuto(b.Mail.Subject, v)
			if subj == "" {
				subj = accKindName[str(inv, "kind")] + " " + str(inv, "number")
			}
			draftID, err = saveDraft(e.App, &sendReq{Box: box, To: b.Mail.To, Subject: subj, Text: body, HTML: html, Att: []string{pdf}, AttNames: []string{str(inv, "pdfName")}, SigID: "__box", Ctx: ctx})
			if err != nil {
				mailErr = err.Error()
			} else {
				inv["draft"] = draftID
			}
		}
	}
	if err := putDoc(e.App, "acc_out", b.ID, inv); err != nil {
		return err
	}
	if dealID := str(inv, "dealId"); dealID != "" {
		if deal := updateDealInv(e.App, dealID, b.ID, func(m map[string]any) {
			m["pdfId"], m["pdfName"] = inv["pdfId"], inv["pdfName"]
			if draftID != "" {
				m["draft"] = draftID
			}
		}); deal != nil {
			if b.PdfID != "" {
				old, _ := deal["events"].([]any)
				deal["events"] = trimEvents(append(old, map[string]any{"at": time.Now().UnixMilli(), "by": str(a, "name"), "kind": "file",
					"text": "📄 " + accKindName[str(inv, "kind")] + " " + str(inv, "number"), "pdfId": b.PdfID, "pdfName": b.PdfName}))
			}
			_ = putDoc(e.App, "deals", dealID, deal)
		}
	}
	return e.JSON(http.StatusOK, map[string]any{"draft": draftID, "mailError": mailErr})
}

func handleAccPaid(e *core.RequestEvent) error {
	a, err := accEditor(e)
	if err != nil {
		return err
	}
	var b struct {
		ID     string  `json:"id"`
		Date   string  `json:"date"`
		Amount float64 `json:"amount"` // Kč; 0 = the whole invoice
		How    string  `json:"how"`
		BankID string  `json:"bankId"`
		Undo   bool    `json:"undo"`
	}
	if err := e.BindBody(&b); err != nil || b.ID == "" {
		return e.BadRequestError("", err)
	}
	status, paid, err := accMarkPaid(e.App, b.ID, b.Date, b.Amount, b.How, b.BankID, str(a, "name"), b.Undo)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"status": status, "paid": paid})
}

// accMarkPaid records a payment of an issued invoice (or removes all of them with undo)
// and mirrors the paid state into the deal: history, "ZF uhrazena", a note for the owner.
func accMarkPaid(app core.App, id, date string, amount float64, how, bankID, by string, undo bool) (string, float64, error) {
	accMu.Lock()
	defer accMu.Unlock()
	rec, err := findDoc(app, "acc_out", id)
	if err != nil {
		return "", 0, userErr{"Faktura nenalezena."}
	}
	inv := docData(rec)
	if str(inv, "status") == "cancelled" {
		return "", 0, userErr{"Faktura je stornovaná."}
	}
	now := time.Now().UnixMilli()
	total := toF(inv["total"])
	pays, _ := inv["payments"].([]any)
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if undo {
		pays = nil
	} else {
		if amount == 0 {
			amount = total - accPaidSum(pays)
		}
		pays = append(pays, map[string]any{"date": date, "amount": amount, "how": how, "bankId": bankID, "at": now, "by": by})
	}
	inv["payments"] = pays
	paid := accPaidSum(pays)
	wasPaid := str(inv, "status") == "paid"
	status := "issued"
	if total > 0 && paid >= total-0.005 {
		status = "paid"
		inv["paidAt"] = date
	} else {
		delete(inv, "paidAt")
	}
	inv["status"] = status
	evs, _ := inv["events"].([]any)
	text := "Úhrada zrušena"
	if !undo {
		text = "Uhrazeno " + czk(amount) + " (" + czDate(date) + ")"
	}
	inv["events"] = append(evs, map[string]any{"at": now, "by": by, "text": text})
	if err := putDoc(app, "acc_out", id, inv); err != nil {
		return "", 0, err
	}
	if dealID := str(inv, "dealId"); dealID != "" && (status == "paid") != wasPaid {
		if deal := updateDealInv(app, dealID, id, func(m map[string]any) {
			if status == "paid" {
				m["status"], m["paidAt"] = "paid", now
			} else {
				m["status"] = "open"
				delete(m, "paidAt")
			}
		}); deal != nil {
			kind := accKindName[str(inv, "kind")]
			if status == "paid" {
				t := fmt.Sprintf("✓ %s %s uhrazena (%s)", kind, str(inv, "number"), czk(total))
				evs := []any{map[string]any{"at": now, "by": by, "kind": "edit", "text": t}}
				if str(inv, "kind") == "deposit" {
					if ev := movePhase(app, deal, "ZF uhrazena", "Účetnictví"); ev != nil {
						evs = append(evs, ev)
					}
				}
				old, _ := deal["events"].([]any)
				deal["events"] = trimEvents(append(old, evs...))
				if owner := str(deal, "owner"); owner != "" && owner != by {
					_ = putDoc(app, "notifs", randUUID(), map[string]any{"to": owner, "text": "💰 " + t + " – " + str(deal, "code") + " " + str(deal, "name"), "at": now, "read": false, "dealId": dealID, "leadId": "", "by": by})
				}
			}
			_ = putDoc(app, "deals", dealID, deal)
		}
	}
	return status, paid, nil
}

func accPaidSum(pays []any) float64 {
	s := 0.0
	for _, p := range pays {
		if m, _ := p.(map[string]any); m != nil {
			s += toF(m["amount"])
		}
	}
	return s
}

func handleAccStorno(e *core.RequestEvent) error {
	a, err := accEditor(e)
	if err != nil {
		return err
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := e.BindBody(&b); err != nil || b.ID == "" {
		return e.BadRequestError("", err)
	}
	rec, err := findDoc(e.App, "acc_out", b.ID)
	if err != nil {
		return e.NotFoundError("Faktura nenalezena.", nil)
	}
	inv := docData(rec)
	if str(inv, "status") == "paid" {
		return e.BadRequestError("Uhrazenou fakturu nelze stornovat. Nejdřív zrušte úhradu.", nil)
	}
	now := time.Now().UnixMilli()
	inv["status"] = "cancelled"
	evs, _ := inv["events"].([]any)
	inv["events"] = append(evs, map[string]any{"at": now, "by": str(a, "name"), "text": "Stornováno"})
	if err := putDoc(e.App, "acc_out", b.ID, inv); err != nil {
		return err
	}
	if dealID := str(inv, "dealId"); dealID != "" {
		if deal := updateDealInv(e.App, dealID, b.ID, func(m map[string]any) { m["status"] = "cancelled" }); deal != nil {
			old, _ := deal["events"].([]any)
			deal["events"] = trimEvents(append(old, map[string]any{"at": now, "by": str(a, "name"), "kind": "edit", "text": "🧾 " + accKindName[str(inv, "kind")] + " " + str(inv, "number") + " stornována"}))
			_ = putDoc(e.App, "deals", dealID, deal)
		}
	}
	return e.JSON(http.StatusOK, map[string]any{"status": "cancelled"})
}

// handleAres looks a company up in the public register (ARES) by IČO, so that
// invoices get the exact name, DIČ and address.
func handleAres(e *core.RequestEvent) error {
	if _, err := authAccount(e); err != nil {
		return err
	}
	ico := digitsRe.ReplaceAllString(e.Request.URL.Query().Get("ico"), "")
	if len(ico) < 6 || len(ico) > 8 {
		return e.BadRequestError("IČO má 8 číslic.", nil)
	}
	ico = strings.Repeat("0", 8-len(ico)) + ico
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get("https://ares.gov.cz/ekonomicke-subjekty-v-be/rest/ekonomicke-subjekty/" + ico)
	if err != nil {
		return e.BadRequestError("ARES teď neodpovídá, zkuste to za chvíli.", nil)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return e.NotFoundError("Firma s tímto IČO v ARES není.", nil)
	}
	var r struct {
		Ico           string `json:"ico"`
		ObchodniJmeno string `json:"obchodniJmeno"`
		Dic           string `json:"dic"`
		Sidlo         struct {
			NazevObce        string `json:"nazevObce"`
			NazevCastiObce   string `json:"nazevCastiObce"`
			NazevUlice       string `json:"nazevUlice"`
			CisloDomovni     int    `json:"cisloDomovni"`
			CisloOrientacni  int    `json:"cisloOrientacni"`
			CisloOrientacniP string `json:"cisloOrientacniPismeno"`
			Psc              int    `json:"psc"`
			TextovaAdresa    string `json:"textovaAdresa"`
		} `json:"sidlo"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&r) != nil {
		return e.BadRequestError("ARES vrátil chybu, zkuste to za chvíli.", nil)
	}
	s := r.Sidlo
	street := s.NazevUlice
	if street == "" {
		street = s.NazevCastiObce
		if street == "" {
			street = s.NazevObce
		}
	}
	if s.CisloDomovni > 0 {
		street += " " + strconv.Itoa(s.CisloDomovni)
		if s.CisloOrientacni > 0 {
			street += "/" + strconv.Itoa(s.CisloOrientacni) + s.CisloOrientacniP
		}
	}
	zip := ""
	if s.Psc > 0 {
		z := fmt.Sprintf("%05d", s.Psc)
		zip = z[:3] + " " + z[3:]
	}
	return e.JSON(http.StatusOK, map[string]any{"ico": r.Ico, "name": r.ObchodniJmeno, "dic": r.Dic, "street": strings.TrimSpace(street), "city": s.NazevObce, "zip": zip, "address": s.TextovaAdresa})
}
