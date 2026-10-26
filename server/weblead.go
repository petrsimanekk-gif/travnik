package main

// Public web enquiry form (/poptavka) → a new lead in the CRM.

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

var prague, _ = time.LoadLocation("Europe/Prague")

func crmConfig(app core.App) map[string]any {
	if rec, err := findDoc(app, "config", "main"); err == nil {
		return docData(rec)
	}
	return map[string]any{}
}

func strList(v any) []string {
	var out []string
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// handleLeadForm gives the form its choice list (deal types from the CRM settings).
func handleLeadForm(e *core.RequestEvent) error {
	types := strList(crmConfig(e.App)["types"])
	if len(types) == 0 {
		types = []string{"Nová pergola – rodinný dům", "Nová pergola – firma / gastro", "Doplňky ke stávající pergole", "Servis / reklamace pergoly"}
	}
	return e.JSON(http.StatusOK, map[string]any{"types": types})
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func handleWebLead(e *core.RequestEvent) error {
	var b struct {
		Name, Phone, Email, City, Zip, Street, Subject, Message, Web string
		Consent                                                      bool
		Started                                                      int64
	}
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("Neplatný formulář.", err)
	}
	// bots: honeypot field filled or the form sent within 3 s of loading → pretend success
	if b.Web != "" || (b.Started > 0 && time.Now().UnixMilli()-b.Started < 3000) {
		return e.JSON(http.StatusOK, map[string]any{"ok": true})
	}
	name, phone, email := clip(b.Name, 120), clip(b.Phone, 40), clip(b.Email, 160)
	if name == "" || (phone == "" && email == "") {
		return e.BadRequestError("Vyplňte jméno a telefon nebo e-mail.", nil)
	}
	if email != "" && !emailRe.MatchString(email) {
		return e.BadRequestError("E-mail nemá správný tvar.", nil)
	}
	if !b.Consent {
		return e.BadRequestError("Potvrďte souhlas se zpracováním údajů.", nil)
	}
	now := time.Now().In(prague)
	var code string
	err := e.App.RunInTransaction(func(tx core.App) error {
		cfg := crmConfig(tx)
		prefix, _ := cfg["leadPrefix"].(string)
		if prefix == "" {
			prefix = "L"
		}
		pre := fmt.Sprintf("%s-%s-", prefix, now.Format("06"))
		recs, err := tx.FindAllRecords("docs", dbx.HashExp{"col": "leads"})
		if err != nil {
			return err
		}
		max := 0
		for _, r := range recs {
			if c, _ := docData(r)["code"].(string); strings.HasPrefix(c, pre) {
				if n, err := strconv.Atoi(c[len(pre):]); err == nil && n > max {
					max = n
				}
			}
		}
		code = fmt.Sprintf("%s%04d", pre, max+1)
		state := "Nový"
		if st := strList(cfg["leadStates"]); len(st) > 0 {
			state = st[0]
		}
		ms := now.UnixMilli()
		id := randUUID()
		note := clip(b.Message, 4000)
		lead := map[string]any{
			"code": code, "name": name, "phone": phone, "email": email,
			"subject": clip(b.Subject, 200), "state": state, "owner": "", "source": "Web",
			"street": clip(b.Street, 200), "city": clip(b.City, 120), "zip": clip(b.Zip, 12),
			"receivedAt": now.Format("2006-01-02"), "note": note, "createdAt": ms,
			"gdpr":        "Souhlas z webového formuláře " + now.Format("2.1.2006 15:04"),
			"events":      []any{map[string]any{"at": ms, "by": "Web", "text": "Lead přijat z webového formuláře", "kind": "edit"}},
			"autoPending": true, // the first signed-in CRM page runs the "new lead" automations
		}
		if err := putDoc(tx, "leads", id, lead); err != nil {
			return err
		}
		for _, u := range strList(cfg["users"]) {
			n := map[string]any{"to": u, "text": "Nový lead z webu: " + name + " (" + code + ")", "at": ms, "read": false, "dealId": "", "leadId": id, "by": "Web"}
			if err := putDoc(tx, "notifs", randUUID(), n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	e.App.Logger().Info("web lead", "code", code)
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func randUUID() string {
	h := randHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}
