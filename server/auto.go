package main

// Automation e-mails go out from a company mailbox without anyone clicking:
//
//   - a new lead (web form, e-mail → lead) gets its "lead_created" e-mails from the
//     server right away; the lead carries autoMailDone so the page does not send them again,
//   - every other automation runs in the page and asks /api/stinko/mail/auto to send.

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

func autoRules(app core.App) []map[string]any {
	var out []map[string]any
	if l, ok := crmConfig(app)["automations"].([]any); ok {
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func ruleActions(r map[string]any) []map[string]any {
	var out []map[string]any
	if l, ok := r["actions"].([]any); ok {
		for _, x := range l {
			m, _ := x.(map[string]any)
			out = append(out, m) // nil keeps the index in step with the page
		}
	}
	return out
}

// autoBox: the mailbox chosen in the action, else the first enabled shared one (by name).
func autoBox(app core.App, want string) (string, *mailCfg) {
	if want != "" {
		if _, cfg, _, err := loadBox(app, want); err == nil && cfg.Enabled {
			return want, cfg
		}
	}
	type bx struct {
		key string
		cfg *mailCfg
	}
	var all []bx
	recs, _ := app.FindAllRecords("mailboxes")
	for _, r := range recs {
		if _, cfg, _, err := loadBox(app, r.GetString("key")); err == nil && cfg.Enabled && cfg.Owner == "" {
			all = append(all, bx{r.GetString("key"), cfg})
		}
	}
	if len(all) == 0 {
		return "", nil
	}
	sort.Slice(all, func(i, j int) bool { return strings.ToLower(all[i].cfg.Name) < strings.ToLower(all[j].cfg.Name) })
	return all[0].key, all[0].cfg
}

var autoVarRe = regexp.MustCompile(`\{(\w+)\}`)

func fillAuto(t string, v map[string]string) string {
	return autoVarRe.ReplaceAllStringFunc(t, func(m string) string { return v[m[1:len(m)-1]] })
}

func leadVars(l map[string]any) map[string]string {
	name := str(l, "name")
	return map[string]string{"klient": name, "lead": name, "jmeno": name, "stav": str(l, "state"), "vlastnik": str(l, "owner"), "obchodnik": str(l, "owner"),
		"predmet": str(l, "subject"), "osloveni": salutation(name, str(l, "salutation"))}
}

// actionText: the action's own subject/body, or those of the e-mail template it points to
// (so editing the template changes what the automation sends).
func actionText(app core.App, a map[string]any) (string, string) {
	if id := str(a, "tpl"); id != "" {
		if l, ok := crmConfig(app)["mailTpls"].([]any); ok {
			for _, x := range l {
				if t, ok := x.(map[string]any); ok && str(t, "id") == id {
					return str(t, "subject"), str(t, "body")
				}
			}
		}
	}
	return str(a, "subject"), str(a, "body")
}

// sendAuto sends one automation e-mail and records it in Komunikace.
func sendAuto(app core.App, rule string, act map[string]any, to, subject, body string, link map[string]string) error {
	box, cfg := autoBox(app, str(act, "box"))
	cid := randUUID()
	comm := map[string]any{"kind": "email", "to": to, "subject": subject, "body": body, "status": "odesílá se", "at": time.Now().UnixMilli(), "by": "Automatizace",
		"leadId": link["leadId"], "dealId": link["dealId"], "clientId": link["clientId"], "auto": rule}
	if box == "" {
		comm["status"], comm["error"] = "neodesláno", "Není připojená žádná společná e-mailová schránka."
		_ = putDoc(app, "comms", cid, comm)
		return userErr{comm["error"].(string)}
	}
	if err := putDoc(app, "comms", cid, comm); err != nil {
		return err
	}
	sig, logo := "", ""
	if str(act, "sig") == "box" {
		sig, logo = cfg.Signature, cfg.SigLogo
	}
	_, err := doSend(app, map[string]any{"name": "Automatizace „" + rule + "“", "role": "admin"}, &sendReq{Box: box, To: to, Subject: subject, Text: body, SigText: sig, SigLogo: logo, CommID: cid})
	if err != nil {
		if d, e2 := findDoc(app, "comms", cid); e2 == nil {
			cm := docData(d)
			cm["status"], cm["error"] = "chyba", clip(err.Error(), 500)
			_ = putDoc(app, "comms", cid, cm)
		}
	}
	return err
}

// leadAutoKeys lists the lead_created e-mail actions the server will send ("ruleId:index").
func leadAutoKeys(app core.App) []string {
	if box, _ := autoBox(app, ""); box == "" {
		// no shared mailbox: the page keeps its old behaviour for these e-mails
		has := false
		for _, r := range autoRules(app) {
			for _, a := range ruleActions(r) {
				if a != nil && str(a, "box") != "" {
					has = true
				}
			}
		}
		if !has {
			return nil
		}
	}
	var keys []string
	for _, r := range autoRules(app) {
		if r["on"] != true || str(r, "trigger") != "lead_created" || str(r, "param") != "" || str(r, "condType") != "" {
			continue
		}
		for i, a := range ruleActions(r) {
			if a != nil && str(a, "kind") == "email" {
				keys = append(keys, fmt.Sprintf("%s:%d", str(r, "id"), i))
			}
		}
	}
	return keys
}

// sendLeadAutoMails runs after a lead was stored; keys came from leadAutoKeys.
func sendLeadAutoMails(app core.App, leadID string, keys []string) {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var events []any
	rec, err := findDoc(app, "leads", leadID)
	if err != nil {
		return
	}
	l := docData(rec)
	v := leadVars(l)
	to := strings.TrimSpace(str(l, "email"))
	// the web form is public: it must not become a way to send our e-mails to anyone, many times
	if to != "" && str(l, "source") == "Web" {
		if why := webMailLimit(app, to); why != "" {
			events = append(events, map[string]any{"at": time.Now().UnixMilli(), "by": "Automatizace", "kind": "edit", "text": "✉ Automatický e-mail neodeslán: " + why})
			to = ""
		}
	}
	for _, r := range autoRules(app) {
		for i, a := range ruleActions(r) {
			if a == nil || !want[fmt.Sprintf("%s:%d", str(r, "id"), i)] {
				continue
			}
			name := str(r, "name")
			ms := time.Now().UnixMilli()
			if to == "" {
				if len(events) == 0 {
					events = append(events, map[string]any{"at": ms, "by": "Automatizace", "kind": "edit", "text": "✉ E-mail neodeslán – lead nemá e-mail (automatizace „" + name + "“)"})
				}
				continue
			}
			subj, body := actionText(app, a)
			subject := fillAuto(subj, v)
			if err := sendAuto(app, name, a, to, subject, fillAuto(body, v), map[string]string{"leadId": leadID}); err != nil {
				app.Logger().Warn("auto mail", "lead", leadID, "err", err)
				events = append(events, map[string]any{"at": ms, "by": "Automatizace", "kind": "edit", "text": "✉ E-mail „" + subject + "“ se nepodařilo odeslat: " + clip(err.Error(), 200)})
			} else {
				events = append(events, map[string]any{"at": ms, "by": "Automatizace", "kind": "edit", "text": "✉ E-mail „" + subject + "“ odeslán na " + to + " (automatizace „" + name + "“)"})
			}
		}
	}
	if len(events) == 0 {
		return
	}
	if rec, err := findDoc(app, "leads", leadID); err == nil {
		l := docData(rec)
		old, _ := l["events"].([]any)
		l["events"] = append(old, events...)
		_ = putDoc(app, "leads", leadID, l)
	}
}

const (
	webMailPerHour  = 20             // automatic e-mails to web-form leads, all addresses together
	webMailSameAddr = 24 * time.Hour // one per address in this window
)

// webMailLimit says why an automatic e-mail to a web-form lead must not go out ("" = may).
func webMailLimit(app core.App, to string) string {
	recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "comms"})
	now := time.Now().UnixMilli()
	hour := 0
	for _, r := range recs {
		c := docData(r)
		if str(c, "by") != "Automatizace" || str(c, "status") == "neodesláno" {
			continue
		}
		at, _ := c["at"].(float64)
		if now-int64(at) < time.Hour.Milliseconds() {
			hour++
		}
		if strings.EqualFold(str(c, "to"), to) && now-int64(at) < webMailSameAddr.Milliseconds() {
			return "na tuto adresu už dnes automatický e-mail odešel (ochrana proti zneužití formuláře)"
		}
	}
	if hour >= webMailPerHour {
		return "za poslední hodinu odešlo příliš mnoho automatických e-mailů (ochrana proti zneužití formuláře)"
	}
	return ""
}

// handleMailAuto sends an e-mail of an automation the page has just run.
func handleMailAuto(e *core.RequestEvent) error {
	if _, err := authAccount(e); err != nil {
		return err
	}
	var b struct {
		RuleID   string `json:"ruleId"`
		Idx      int    `json:"idx"`
		To       string `json:"to"`
		Subject  string `json:"subject"`
		Body     string `json:"body"`
		LeadID   string `json:"leadId"`
		DealID   string `json:"dealId"`
		OfferID  string `json:"offerId"`
		ClientID string `json:"clientId"`
	}
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	// only an e-mail action an admin set up, and only to the lead's or client's own address
	var rule, act map[string]any
	for _, r := range autoRules(e.App) {
		if str(r, "id") == b.RuleID && r["on"] == true {
			if acts := ruleActions(r); b.Idx >= 0 && b.Idx < len(acts) && acts[b.Idx] != nil && str(acts[b.Idx], "kind") == "email" {
				rule, act = r, acts[b.Idx]
			}
		}
	}
	if rule == nil {
		return e.ForbiddenError("Automatizace s tímto e-mailem neexistuje.", nil)
	}
	to := strings.ToLower(strings.TrimSpace(b.To))
	allowed := map[string]bool{}
	addDoc := func(col, id, field string) map[string]any {
		if id == "" {
			return nil
		}
		if d, err := findDoc(e.App, col, id); err == nil {
			m := docData(d)
			if field != "" {
				allowed[strings.ToLower(strings.TrimSpace(str(m, field)))] = true
			}
			return m
		}
		return nil
	}
	addDoc("leads", b.LeadID, "email")
	clientID := b.ClientID
	if d := addDoc("deals", b.DealID, ""); d != nil && clientID == "" {
		clientID = str(d, "clientId")
	}
	if o := addDoc("offers", b.OfferID, ""); o != nil && clientID == "" {
		clientID = str(o, "clientId")
	}
	addDoc("clients", clientID, "email")
	delete(allowed, "")
	if to == "" || !allowed[to] {
		return e.ForbiddenError("Příjemce není e-mail leadu ani klienta.", nil)
	}
	err := sendAuto(e.App, str(rule, "name"), act, b.To, clip(b.Subject, 500), b.Body, map[string]string{"leadId": b.LeadID, "dealId": b.DealID, "clientId": clientID})
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

// ---------- oslovení (same rules as salutation() in the page) ----------

var (
	femFirst  = wordSet("eva jana hana anna marie lenka kateřina lucie petra martina veronika tereza michaela monika zuzana jitka alena markéta ivana barbora kristýna simona pavla věra dana helena ludmila jaroslava andrea nikola klára eliška adéla natálie karolína denisa gabriela šárka romana renata olga zdeňka iveta irena milena blanka radka vladimíra dagmar ilona libuše miroslava jiřina hedvika magdaléna aneta silvie sabina vendula daniela kamila stanislava drahomíra božena marcela soňa sandra julie viktorie ema emma sofie ester johana valerie žaneta iva kristina")
	maleFirst = wordSet("jan petr pavel jiří josef tomáš martin jaroslav miroslav zdeněk václav michal františek jakub milan karel lukáš david ondřej vladimír ladislav roman stanislav marek radek daniel antonín vojtěch filip adam matěj dominik patrik aleš libor jindřich ivan vít richard robert oldřich vlastimil luboš dušan kamil igor rudolf bohumil štěpán šimon matyáš kryštof hynek přemysl radim viktor otakar jaromír bořivoj zbyněk ivo lubomír emil leoš denis erik oskar sebastián tadeáš vilém eduard bohuslav jonáš samuel alexandr dalibor radovan bohdan lubor miloš lumír vratislav ctibor kvido arnošt evžen zdenek jiri tomas frantisek vaclav lukas ondrej")
	titleRe   = regexp.MustCompile(`(?i)^(ing|mgr|bc|mudr|judr|phdr|rndr|paeddr|mvdr|doc|prof|ph\.?d|csc|mba|dis|arch|ing\.arch)\.?$`)
	companyRe = regexp.MustCompile(`(?i)\b(s\.?\s?r\.?\s?o|a\.?\s?s|spol|v\.?\s?o\.?\s?s|k\.?\s?s|z\.?\s?s|o\.?\s?p\.?\s?s|z\.?\s?ú|družstvo|group|holding|ltd|gmbh|inc)\b\.?|\d`)
	ownRe     = regexp.MustCompile(`(?i)^(dobrý|vážen|mil[ýá]|ahoj|zdravím|hezký)`)
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

func capFirst(w string) string {
	r := []rune(w)
	if len(r) == 0 {
		return w
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func vocMale(s string) string {
	r := []rune(s)
	w := []rune(strings.ToLower(s))
	n := len(w)
	keep := func(cut int, end string) string { return string(r[:n-cut]) + end }
	last := func(k int) rune {
		if n-k < 0 {
			return 0
		}
		return w[n-k]
	}
	vowel := func(c rune) bool { return strings.ContainsRune("aeiouyáéíóúůý", c) }
	switch {
	case n == 0:
		return s
	case strings.ContainsRune("ýíoeiuyů", last(1)):
		return s
	case last(1) == 'a':
		return keep(1, "o")
	case n >= 3 && last(2) == 'ě' && last(1) == 'k' && strings.ContainsRune("ndt", last(3)):
		return keep(3, map[rune]string{'n': "ň", 'd': "ď", 't': "ť"}[last(3)]+"ku")
	case n >= 3 && last(2) == 'e' && last(1) == 'k' && !vowel(last(3)):
		return keep(2, "ku")
	case n > 4 && last(2) == 'e' && last(1) == 'c':
		return keep(2, "če")
	case strings.HasSuffix(string(w), "ch") || strings.ContainsRune("khg", last(1)):
		return s + "u"
	case strings.ContainsRune("šžčřjďťňcsxz", last(1)):
		return s + "i"
	case last(1) == 'r' && n >= 2 && !vowel(last(2)):
		return keep(1, "ře")
	case strings.HasSuffix(string(w), "havel") || strings.HasSuffix(string(w), "pavel") || strings.HasSuffix(string(w), "karel"):
		return keep(2, "le")
	case strings.ContainsRune("bdfglmnptv", last(1)) || (last(1) == 'r' && n >= 2 && strings.ContainsRune("aeiouáéíóú", last(2))):
		return s + "e"
	}
	return s
}

func salutation(name, own string) string {
	if o := strings.TrimRight(strings.TrimSpace(own), ","); o != "" {
		if ownRe.MatchString(o) {
			return o + ","
		}
		return "Dobrý den, " + o + ","
	}
	raw := strings.TrimSpace(name)
	if raw == "" || companyRe.MatchString(raw) {
		return "Dobrý den,"
	}
	var parts []string
	for _, p := range strings.Fields(strings.ReplaceAll(raw, ",", " ")) {
		if !titleRe.MatchString(strings.TrimSuffix(p, ".")) && !titleRe.MatchString(p) {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 || len(parts) > 3 {
		return "Dobrý den,"
	}
	first, last := parts[0], parts[len(parts)-1]
	a, b := strings.ToLower(first), strings.ToLower(last)
	if (femFirst[b] || maleFirst[b]) && !(femFirst[a] || maleFirst[a]) {
		first, last = last, first
	}
	f, l := strings.ToLower(first), strings.ToLower(last)
	if femFirst[f] || (strings.HasSuffix(l, "ová") && !maleFirst[f]) {
		return "Dobrý den, paní " + capFirst(last) + ","
	}
	if maleFirst[f] && !strings.HasSuffix(l, "ová") {
		return "Dobrý den, pane " + capFirst(vocMale(last)) + ","
	}
	return "Dobrý den,"
}

// used by createLead: which keys to store and send
func leadAutoStart(app core.App, id string, keys []string) {
	if len(keys) > 0 {
		go sendLeadAutoMails(app, id, keys)
	}
}
