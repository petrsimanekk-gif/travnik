package main

// Payments from the bank's e-mail notifications. ČSOB (and Moneta) can send an
// e-mail for every posted payment; when such a mail arrives in a mailbox the CRM
// reads, it becomes a movement in Účetnictví → Banka (acc_bank) and is paired by
// the variable symbol: an issued invoice is marked paid (and its deal moves on),
// a received one is marked paid. The accounting people get a notification.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type bankMove struct {
	Date, Account, Counter, Name, VS, KS, SS, Msg, Bank string
	Amount                                         float64
}

var (
	bmLineRe = regexp.MustCompile(`^\s*([^:\n]{2,40}?)\s*:\s*(.*?)\s*$`)
	bmDneRe  = regexp.MustCompile(`(?i)\bdne\s+(\d{1,2})\.\s*(\d{1,2})\.\s*(\d{4})`)
	bmDateRe = regexp.MustCompile(`(\d{1,2})\.\s*(\d{1,2})\.\s*(\d{4})`)
	bmAcctRe = regexp.MustCompile(`(?:\d{1,6}-)?\d{2,10}(?:/\d{4})?`)
	bmOnRe   = regexp.MustCompile(`(?i)na\s+účtu\s+((?:\d{1,6}-)?\d{2,10})`)
	bmAmtRe  = regexp.MustCompile(`([+\-−]?)\s*(\d[\d\s.]*(?:,\d{1,2})?)`)
)

func bmLabels(text string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		m := bmLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(m[1]))
		if _, dup := out[k]; !dup {
			out[k] = strings.TrimSpace(m[2])
		}
	}
	return out
}

func bmPick(l map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := l[k]; v != "" {
			return v
		}
	}
	return ""
}

// bmAmount reads "+1 234,50 CZK", "-99,00 Kč", "1.234,50".
func bmAmount(v string) (float64, bool) {
	v = strings.NewReplacer(" ", " ", " ", " ").Replace(v)
	m := bmAmtRe.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n := strings.ReplaceAll(m[2], " ", "")
	if strings.Contains(n, ",") {
		n = strings.ReplaceAll(n, ".", "")
		n = strings.Replace(n, ",", ".", 1)
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil {
		return 0, false
	}
	if m[1] == "-" || m[1] == "−" {
		f = -f
	}
	return f, true
}

func bmDate(d, mo, y string) string {
	di, _ := strconv.Atoi(d)
	mi, _ := strconv.Atoi(mo)
	return fmt.Sprintf("%s-%02d-%02d", y, mi, di)
}

// parseBankMail recognises a payment notification of ČSOB or Moneta; nil for any other mail.
func parseBankMail(p *parsedMail) *bankMove {
	text := p.Text
	if strings.TrimSpace(text) == "" {
		text = htmlToText(p.HTML)
	}
	text = strings.NewReplacer("\r", "", " ", " ").Replace(text)
	low := strings.ToLower(text + " " + p.Subject + " " + p.From)
	bank := ""
	switch {
	case strings.Contains(low, "čsob") || strings.Contains(low, "csob"):
		bank = "ČSOB"
	case strings.Contains(low, "moneta"):
		bank = "Moneta"
	default:
		return nil
	}
	// a reply or forward is not a notification of its own
	if s := strings.ToLower(strings.TrimSpace(p.Subject)); strings.HasPrefix(s, "re:") || strings.HasPrefix(s, "fw") {
		return nil
	}
	l := bmLabels(text)
	amtText := bmPick(l, "částka", "částka transakce", "částka platby", "objem")
	amt, ok := bmAmount(amtText)
	if !ok || amt == 0 {
		return nil
	}
	if !strings.ContainsAny(amtText, "+-−") && (strings.Contains(low, "odchozí") || strings.Contains(low, "odepsán")) {
		amt = -amt
	}
	t := &bankMove{Amount: amt, Bank: bank}
	if m := bmDneRe.FindStringSubmatch(text); m != nil {
		t.Date = bmDate(m[1], m[2], m[3])
	} else if m := bmDateRe.FindStringSubmatch(bmPick(l, "datum zaúčtování", "datum", "datum splatnosti", "datum transakce")); m != nil {
		t.Date = bmDate(m[1], m[2], m[3])
	} else if !p.Date.IsZero() {
		t.Date = p.Date.Local().Format("2006-01-02")
	} else {
		t.Date = time.Now().Format("2006-01-02")
	}
	if m := bmOnRe.FindStringSubmatch(text); m != nil {
		t.Account = m[1]
	} else {
		t.Account = bmAcctRe.FindString(bmPick(l, "účet", "číslo účtu", "na účet"))
	}
	t.Counter = bmAcctRe.FindString(bmPick(l, "účet protistrany", "protiúčet", "číslo protiúčtu", "z účtu", "účet plátce", "účet příjemce"))
	t.Name = bmPick(l, "název protistrany", "plátce", "příjemce", "název účtu protistrany", "jméno protistrany")
	t.VS = strings.TrimLeft(digitsRe.ReplaceAllString(bmPick(l, "variabilní symbol", "vs"), ""), "0")
	t.KS = strings.TrimLeft(digitsRe.ReplaceAllString(bmPick(l, "konstantní symbol", "ks"), ""), "0")
	t.SS = strings.TrimLeft(digitsRe.ReplaceAllString(bmPick(l, "specifický symbol", "ss"), ""), "0")
	t.Msg = bmPick(l, "zpráva pro příjemce", "zpráva", "poznámka", "popis")
	return t
}

func baseAcct(s string) string {
	s = strings.SplitN(strings.TrimSpace(s), "/", 2)[0]
	parts := strings.SplitN(s, "-", 2)
	if len(parts) == 2 {
		return strings.TrimLeft(parts[0], "0") + "-" + strings.TrimLeft(parts[1], "0")
	}
	return strings.TrimLeft(s, "0")
}

// bankFromMail stores the payment and pairs it; false when the mail is not a bank notification.
func bankFromMail(app core.App, p *parsedMail) bool {
	t := parseBankMail(p)
	if t == nil {
		return false
	}
	cfg := accCfg(app)
	if len(cfg) == 0 {
		return false // Účetnictví is not set up: leave it as an ordinary e-mail
	}
	acctID, acctNum := "", t.Account
	if accts, _ := cfg["accounts"].([]any); len(accts) > 0 {
		for _, x := range accts {
			if a, _ := x.(map[string]any); a != nil && baseAcct(str(a, "number")) == baseAcct(t.Account) {
				acctID, acctNum = str(a, "id"), str(a, "number")
			}
		}
	}
	// the same payment always gives the same key: a forwarded copy or a second sync is ignored
	h := sha1.Sum([]byte(strings.Join([]string{t.Date, baseAcct(t.Account), strconv.FormatFloat(t.Amount, 'f', 2, 64), t.VS, t.Counter, t.Name}, "|")))
	id := "m_" + hex.EncodeToString(h[:10])
	if _, err := findDoc(app, "acc_bank", id); err == nil {
		return true
	}
	rec := map[string]any{"account": baseAcct(t.Account), "acct": acctID, "acctNumber": acctNum, "date": t.Date, "amount": t.Amount, "vs": t.VS, "ks": t.KS, "ss": t.SS,
		"counter": t.Counter, "name": t.Name, "msg": t.Msg, "match": nil, "importedAt": time.Now().UnixMilli(), "by": t.Bank + " (e-mail)", "source": "mail"}
	if err := putDoc(app, "acc_bank", id, rec); err != nil {
		app.Logger().Warn("bank mail", "err", err)
		return true
	}
	what := bankAutoMatch(app, id, rec)
	who := t.Name
	if who == "" {
		who = t.Counter
	}
	var text string
	if t.Amount > 0 {
		text = fmt.Sprintf("💰 Přišla platba %s od %s", czk(t.Amount), who)
	} else {
		text = fmt.Sprintf("💸 Odešla platba %s – %s", czk(-t.Amount), who)
	}
	if t.VS != "" {
		text += " (VS " + t.VS + ")"
	}
	if what != "" {
		text += " – spárováno: " + what
	} else {
		text += " – nespárováno, spárujte v Účetnictví → Banka"
	}
	accNotify(app, text)
	return true
}

// bankAutoMatch pairs a movement by its variable symbol (and the amount for supplier invoices).
func bankAutoMatch(app core.App, id string, t map[string]any) string {
	vs := strings.TrimLeft(str(t, "vs"), "0")
	amt := toF(t["amount"])
	if vs == "" {
		return ""
	}
	vsEq := func(x string) bool { return strings.TrimLeft(x, "0") == vs }
	what := ""
	if amt > 0 {
		recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "acc_out"})
		best, bestExact := "", false
		for _, r := range recs {
			d := docData(r)
			if st := str(d, "status"); st == "paid" || st == "cancelled" || !vsEq(str(d, "vs")) {
				continue
			}
			pays, _ := d["payments"].([]any)
			exact := abs(toF(d["total"])-accPaidSum(pays)-amt) < 0.01
			if best == "" || (exact && !bestExact) {
				best, bestExact = r.GetString("key"), exact
			}
		}
		if best != "" {
			if _, _, err := accMarkPaid(app, best, str(t, "date"), amt, "bank", id, "Banka", false); err == nil {
				t["match"] = map[string]any{"col": "acc_out", "id": best}
				if r, err := findDoc(app, "acc_out", best); err == nil {
					what = "faktura " + str(docData(r), "number")
				}
			}
		}
	} else {
		recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "acc_in"})
		for _, r := range recs {
			d := docData(r)
			if d["paid"] == true || str(d, "status") == "todo" || !vsEq(str(d, "vs")) || abs(toF(d["total"])+amt) >= 0.01 {
				continue
			}
			d["paid"], d["paidAt"], d["paidHow"], d["bankId"] = true, str(t, "date"), "bank", id
			if putDoc(app, "acc_in", r.GetString("key"), d) == nil {
				t["match"] = map[string]any{"col": "acc_in", "id": r.GetString("key")}
				sup, _ := d["supplier"].(map[string]any)
				what = "doklad " + str(sup, "name") + " " + str(d, "number")
			}
			break
		}
	}
	if what != "" {
		_ = putDoc(app, "acc_bank", id, t)
	}
	return what
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// accNotify: a CRM notification for everyone who sees Účetnictví.
func accNotify(app core.App, text string) {
	recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "accounts"})
	now := time.Now().UnixMilli()
	for _, r := range recs {
		a := docData(r)
		if a["active"] == false || accPerm(a) == "none" {
			continue
		}
		_ = putDoc(app, "notifs", randUUID(), map[string]any{"to": str(a, "name"), "text": text, "at": now, "read": false, "dealId": "", "leadId": "", "by": "Banka", "acc": "banka"})
	}
}
