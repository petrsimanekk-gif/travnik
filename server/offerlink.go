package main

// Online offer: an offer e-mail can carry a card (photo, what the pergola has, a
// "Zobrazit nabídku" button) that opens /n/{token}, a page with the whole offer.
// Each visit is recorded — when, how long, whether the PDF was downloaded, from what
// device — and the offer's owner gets a CRM notification when the client opens it.
// The page shows a snapshot of the offer taken when the e-mail was prepared.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

func ensureOfferSchema(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("offershares"); err != nil {
		c := core.NewBaseCollection("offershares")
		c.Fields.Add(
			&core.TextField{Name: "token", Required: true, Max: 64},
			&core.TextField{Name: "offer", Required: true, Max: 300},
			&core.JSONField{Name: "data", MaxSize: 1 << 20},
			&core.TextField{Name: "pdf", Max: 300},
			&core.TextField{Name: "origin", Max: 300},
			&core.TextField{Name: "by", Max: 300},
			&core.NumberField{Name: "at", OnlyInt: true},
		)
		c.AddIndex("idx_offershares_token", true, "`token`", "")
		c.AddIndex("idx_offershares_offer", false, "`offer`", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCollectionByNameOrId("offerviews"); err != nil {
		c := core.NewBaseCollection("offerviews")
		c.Fields.Add(
			&core.TextField{Name: "token", Required: true, Max: 64},
			&core.TextField{Name: "offer", Max: 300},
			&core.TextField{Name: "vid", Max: 64}, // visitor (one browser), kept in the page's localStorage
			&core.NumberField{Name: "started", OnlyInt: true},
			&core.NumberField{Name: "last", OnlyInt: true},
			&core.NumberField{Name: "secs", OnlyInt: true},
			&core.BoolField{Name: "pdf"},
			&core.TextField{Name: "device", Max: 40},
			&core.TextField{Name: "seen", Max: 300}, // sections scrolled to
		)
		c.AddIndex("idx_offerviews_token", false, "`token`,`started`", "")
		c.AddIndex("idx_offerviews_offer", false, "`offer`", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	return nil
}

func bindOfferRoutes(se *core.ServeEvent) {
	r := se.Router
	r.POST("/api/stinko/offer/share", handleOfferShare)
	r.GET("/api/stinko/offer/views", handleOfferViews)
	r.GET("/api/stinko/o/{token}", handleOfferPublic)
	r.POST("/api/stinko/o/{token}/hit", handleOfferHit)
	r.GET("/api/stinko/o/{token}/pdf", handleOfferPdf)
	r.GET("/n/{token}", func(e *core.RequestEvent) error {
		e.Response.Header().Set("X-Robots-Tag", "noindex")
		e.Response.Header().Set("Cache-Control", "no-cache")
		return e.FileFS(os.DirFS(publicDir()), "nabidka.html")
	})
}

func offerPerm(a map[string]any) string {
	if a["role"] == "admin" {
		return "edit"
	}
	perms, _ := a["perms"].(map[string]any)
	if p, _ := perms["offers"].(string); p == "view" || p == "edit" {
		return p
	}
	return "none"
}

// offerFor: the offer doc, if this account may see it.
func offerFor(e *core.RequestEvent, a map[string]any, id string) (map[string]any, error) {
	if offerPerm(a) == "none" {
		return nil, e.ForbiddenError("permission_denied", nil)
	}
	d, err := findDoc(e.App, "offers", id)
	if err != nil {
		return nil, e.NotFoundError("Nabídka nenalezena.", nil)
	}
	o := docData(d)
	accID := ""
	if e.Auth != nil {
		accID = e.Auth.GetString("acc")
	}
	if !scopeFor(e.App, accID).visible("offers", id, o) {
		return nil, e.ForbiddenError("permission_denied", nil)
	}
	return o, nil
}

func shareURL(r *core.Record) string {
	return strings.TrimRight(r.GetString("origin"), "/") + "/n/" + r.GetString("token")
}

// handleOfferShare creates (or refreshes) the offer's online page; one link per offer.
func handleOfferShare(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	var b struct {
		Offer  string         `json:"offer"`
		Data   map[string]any `json:"data"`
		Pdf    string         `json:"pdf"`
		Origin string         `json:"origin"`
	}
	if err := e.BindBody(&b); err != nil || b.Offer == "" || b.Data == nil {
		return e.BadRequestError("", err)
	}
	if _, err := offerFor(e, a, b.Offer); err != nil {
		return err
	}
	origin := strings.TrimRight(b.Origin, "/")
	if !strings.HasPrefix(origin, "https://") && !strings.HasPrefix(origin, "http://") {
		origin = "https://" + e.Request.Host
	}
	rec, err := e.App.FindFirstRecordByData("offershares", "offer", b.Offer)
	if err != nil {
		coll, err := e.App.FindCachedCollectionByNameOrId("offershares")
		if err != nil {
			return err
		}
		rec = core.NewRecord(coll)
		rec.Set("offer", b.Offer)
		rec.Set("token", randHex(12))
	}
	rec.Set("data", b.Data)
	rec.Set("pdf", b.Pdf)
	rec.Set("origin", origin)
	rec.Set("by", str(a, "name"))
	rec.Set("at", time.Now().UnixMilli())
	if err := e.App.Save(rec); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"token": rec.GetString("token"), "url": shareURL(rec)})
}

// handleOfferViews lists the client's visits of an offer's page, for the offer detail.
func handleOfferViews(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	id := e.Request.URL.Query().Get("offer")
	if _, err := offerFor(e, a, id); err != nil {
		return err
	}
	out := map[string]any{"url": "", "views": []any{}}
	if rec, err := e.App.FindFirstRecordByData("offershares", "offer", id); err == nil {
		out["url"], out["sharedAt"] = shareURL(rec), rec.GetInt("at")
	}
	recs, _ := e.App.FindRecordsByFilter("offerviews", "offer = {:o}", "-started", 200, 0, dbx.Params{"o": id})
	views := []any{}
	for _, r := range recs {
		views = append(views, map[string]any{"started": r.GetInt("started"), "last": r.GetInt("last"), "secs": r.GetInt("secs"), "pdf": r.GetBool("pdf"),
			"device": r.GetString("device"), "vid": r.GetString("vid"), "seen": r.GetString("seen")})
	}
	out["views"] = views
	return e.JSON(http.StatusOK, out)
}

func shareByToken(app core.App, token string) (*core.Record, error) {
	if len(token) < 16 || len(token) > 64 {
		return nil, fmt.Errorf("bad token")
	}
	return app.FindFirstRecordByData("offershares", "token", token)
}

// staffVisit: someone logged into the CRM in this browser (their visits do not count).
func staffVisit(e *core.RequestEvent) bool {
	c, err := e.Request.Cookie(cookieName)
	if err != nil {
		return false
	}
	u, err := e.App.FindAuthRecordByToken(c.Value, core.TokenTypeAuth)
	return err == nil && u.Collection().Name == "users"
}

func handleOfferPublic(e *core.RequestEvent) error {
	rec, err := shareByToken(e.App, e.Request.PathValue("token"))
	if err != nil {
		return e.NotFoundError("Nabídka nenalezena.", nil)
	}
	var data map[string]any
	if raw, ok := rec.Get("data").(types.JSONRaw); ok {
		_ = json.Unmarshal(raw, &data)
	}
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, map[string]any{"data": data, "pdf": rec.GetString("pdf") != "", "staff": staffVisit(e)})
}

const offerViewsPerHour = 120 // per offer; a runaway page or a bot cannot fill the database

func handleOfferHit(e *core.RequestEvent) error {
	token := e.Request.PathValue("token")
	share, err := shareByToken(e.App, token)
	if err != nil {
		return e.NotFoundError("", nil)
	}
	if staffVisit(e) {
		return e.JSON(http.StatusOK, map[string]any{"staff": true})
	}
	var b struct {
		View   string `json:"view"`
		Vid    string `json:"vid"`
		Secs   int64  `json:"secs"`
		Pdf    bool   `json:"pdf"`
		Device string `json:"device"`
		Seen   string `json:"seen"`
	}
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	now := time.Now().UnixMilli()
	if b.View != "" {
		r, err := e.App.FindRecordById("offerviews", b.View)
		if err != nil || r.GetString("token") != token {
			return e.NotFoundError("", nil)
		}
		// time on the page can not exceed the time since the visit started
		if max := (now-int64(r.GetInt("started")))/1000 + 30; b.Secs > max {
			b.Secs = max
		}
		if b.Secs > int64(r.GetInt("secs")) {
			r.Set("secs", b.Secs)
		}
		if b.Pdf {
			r.Set("pdf", true)
		}
		if b.Seen != "" {
			r.Set("seen", clip(b.Seen, 300))
		}
		r.Set("last", now)
		if err := e.App.Save(r); err != nil {
			return err
		}
		return e.JSON(http.StatusOK, map[string]any{"view": r.Id})
	}
	if n, _ := e.App.CountRecords("offerviews", dbx.NewExp("token = {:t} AND started > {:s}", dbx.Params{"t": token, "s": now - 3600e3})); n >= offerViewsPerHour {
		return e.JSON(http.StatusOK, map[string]any{"view": ""})
	}
	coll, err := e.App.FindCachedCollectionByNameOrId("offerviews")
	if err != nil {
		return err
	}
	// a visit after a quiet half hour is news for the owner; reloads and quick returns are not
	recent, _ := e.App.CountRecords("offerviews", dbx.NewExp("token = {:t} AND last > {:s}", dbx.Params{"t": token, "s": now - 30*60e3}))
	first, _ := e.App.CountRecords("offerviews", dbx.HashExp{"token": token})
	r := core.NewRecord(coll)
	r.Set("token", token)
	r.Set("offer", share.GetString("offer"))
	r.Set("vid", clip(b.Vid, 64))
	r.Set("started", now)
	r.Set("last", now)
	r.Set("pdf", b.Pdf)
	r.Set("device", clip(b.Device, 40))
	if err := e.App.Save(r); err != nil {
		return err
	}
	if recent == 0 {
		go notifyOfferView(e.App, share.GetString("offer"), share.GetString("by"), first == 0)
	}
	return e.JSON(http.StatusOK, map[string]any{"view": r.Id})
}

// notifyOfferView tells the offer's owner the client is looking at it right now.
func notifyOfferView(app core.App, offerID, by string, first bool) {
	d, err := findDoc(app, "offers", offerID)
	if err != nil {
		return
	}
	o := docData(d)
	who := ""
	if c, err := findDoc(app, "clients", str(o, "clientId")); err == nil {
		who = str(docData(c), "name")
	}
	if who == "" {
		who = "Klient"
	}
	to := str(o, "owner")
	if to == "" {
		to = by
	}
	ms := time.Now().UnixMilli()
	text := "👀 " + who + " si právě prohlíží nabídku " + str(o, "code")
	if first {
		text = "👀 " + who + " poprvé otevřel(a) nabídku " + str(o, "code")
	}
	_ = putDoc(app, "notifs", randUUID(), map[string]any{"to": to, "text": text, "at": ms, "read": false, "dealId": "", "leadId": "", "offerId": offerID, "by": "Online nabídka"})
	if first {
		ev, _ := o["events"].([]any)
		o["events"] = append(ev, map[string]any{"at": ms, "by": "Online nabídka", "kind": "edit", "text": "👀 Klient poprvé otevřel online nabídku"})
		_ = putDoc(app, "offers", offerID, o)
	}
}

func handleOfferPdf(e *core.RequestEvent) error {
	share, err := shareByToken(e.App, e.Request.PathValue("token"))
	if err != nil || share.GetString("pdf") == "" {
		return e.NotFoundError("", nil)
	}
	return sendBlob(e, share.GetString("pdf"))
}

// ---------- the card in the e-mail ----------

func anyStrings(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// offerLayout wraps an offer e-mail: the company logo, a heading, the text in a larger
// font, the offer card with its button at the bottom and the signature. Tables and inline
// styles only, which is what mail apps show reliably.
func offerLayout(body, card, sig string) (string, *outAtt) {
	var logo *outAtt
	top := ""
	if data, err := os.ReadFile(publicDir() + "/logo-email.png"); err == nil {
		logo = &outAtt{name: "logo.png", ctype: "image/png", data: data, cid: "logo-" + randHex(6) + "@crm"}
		top = `<tr><td align="center" style="padding:26px 0 22px;border-bottom:1px solid #e6e9ec"><img src="cid:` + logo.cid + `" width="200" height="45" alt="Prima Shade" style="display:block;border:0;width:200px;height:auto"></td></tr>`
	}
	body = strings.Replace(body, "font-size:14px;line-height:1.45", "font-size:16px;line-height:1.6;color:#1f2b33", 1)
	var b strings.Builder
	b.WriteString(`<div style="margin:0;padding:0;background:#ffffff"><table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td align="center" style="padding:0 14px">`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;font-family:Arial,Helvetica,sans-serif;color:#1f2b33">`)
	b.WriteString(top)
	b.WriteString(`<tr><td style="padding:34px 0 0"><h1 style="margin:0 0 22px;font-family:Arial,Helvetica,sans-serif;font-size:30px;line-height:1.25;font-weight:bold;color:#1f2b33">Vaše cenová nabídka je připravená</h1>`)
	b.WriteString(body)
	b.WriteString(card)
	if sig != "" {
		b.WriteString(`<div style="font-size:15px;line-height:1.55">` + sig + `</div>`)
	}
	b.WriteString(`<div style="height:30px"></div></td></tr></table></td></tr></table></div>`)
	return b.String(), logo
}

// offerCard gives the HTML card (table layout and inline styles: what mail apps show
// reliably), its plain-text line and the photo as an inline image.
func offerCard(app core.App, token string) (htm, text string, img *outAtt, err error) {
	share, err := shareByToken(app, token)
	if err != nil {
		return "", "", nil, userErr{"Odkaz na online nabídku nenalezen."}
	}
	var d map[string]any
	if raw, ok := share.Get("data").(types.JSONRaw); ok {
		_ = json.Unmarshal(raw, &d)
	}
	url := shareURL(share)
	esc := html.EscapeString
	var b bytes.Buffer
	b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%" style="max-width:600px;margin:28px 0 8px;border:1px solid #e3e7ea;border-radius:12px;border-collapse:separate;font-family:Arial,Helvetica,sans-serif;background:#ffffff">`)
	if photo, e := os.ReadFile(publicDir() + "/offer/mail-hero.jpg"); e == nil {
		img = &outAtt{name: "pergola.jpg", ctype: "image/jpeg", data: photo, cid: "offer-" + randHex(6) + "@crm"}
		b.WriteString(`<tr><td style="padding:0"><a href="` + esc(url) + `" target="_blank"><img src="cid:` + img.cid + `" width="600" height="400" alt="Pergola Prima Shade" style="display:block;width:100%;max-width:600px;height:auto;border:0;border-radius:12px 12px 0 0"></a></td></tr>`)
	}
	b.WriteString(`<tr><td style="padding:20px 22px 22px">`)
	b.WriteString(`<div style="font-size:12px;letter-spacing:.08em;text-transform:uppercase;color:#8a6a3a">Cenová nabídka ` + esc(str(d, "code")) + `</div>`)
	b.WriteString(`<div style="font-size:21px;line-height:1.3;font-weight:bold;color:#1f2b33;margin:4px 0 12px">` + esc(str(d, "title")) + `</div>`)
	if ps, ok := d["params"].([]any); ok && len(ps) > 0 {
		b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="font-size:14px;color:#33414b;margin-bottom:10px">`)
		for i, p := range ps {
			kv := anyStrings(p)
			if len(kv) != 2 || i >= 6 {
				continue
			}
			b.WriteString(`<tr><td style="padding:3px 16px 3px 0;color:#6c7a85;white-space:nowrap">` + esc(kv[0]) + `</td><td style="padding:3px 0;font-weight:bold">` + esc(kv[1]) + `</td></tr>`)
		}
		b.WriteString(`</table>`)
	}
	if hl := anyStrings(d["highlights"]); len(hl) > 0 {
		b.WriteString(`<div style="font-size:14px;line-height:1.7;color:#33414b;margin-bottom:6px">`)
		for i, h := range hl {
			if i >= 6 {
				break
			}
			b.WriteString(`<span style="color:#e0922a;font-weight:bold">✓</span>&nbsp;` + esc(h) + `<br>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%" style="margin-top:22px"><tr><td align="center" style="border-radius:8px;background:#f0a23b"><a href="` + esc(url) + `" target="_blank" style="display:block;padding:17px 24px;font-family:Arial,Helvetica,sans-serif;font-size:18px;font-weight:bold;color:#2b1a04;text-decoration:none;border-radius:8px;text-align:center">Zobrazit nabídku</a></td></tr></table>`)
	if v := str(d, "validTo"); v != "" {
		b.WriteString(`<div style="font-size:13px;color:#6c7a85;margin-top:12px;text-align:center">Cena, kompletní rozpis a fotky jsou v nabídce · platnost do ` + esc(v) + `</div>`)
	}
	b.WriteString(`</td></tr></table>`)
	return b.String(), "Zobrazit nabídku " + str(d, "code") + ": " + url, img, nil
}
