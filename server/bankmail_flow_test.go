package main

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func testApp(t *testing.T) core.App {
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := ensureSchema(app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

func TestBankMailPaysInvoice(t *testing.T) {
	app := testApp(t)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(putDoc(app, "config", "main", map[string]any{"phases": []any{
		map[string]any{"id": "p1", "name": "Nový", "kind": "open"}, map[string]any{"id": "p2", "name": "ZF vystavena", "kind": "open"}, map[string]any{"id": "p3", "name": "ZF uhrazena", "kind": "open"}}}))
	must(putDoc(app, "acc_cfg", "main", map[string]any{"company": map[string]any{"name": "Prima Shade s.r.o."}, "accounts": []any{map[string]any{"id": "a1", "name": "ČSOB", "number": "378833629/0300"}}}))
	must(putDoc(app, "accounts", "u1", map[string]any{"name": "Petr", "role": "admin", "active": true}))
	must(putDoc(app, "accounts", "u2", map[string]any{"name": "Obchodník", "role": "sales", "active": true, "perms": map[string]any{"deals": "edit"}}))
	must(putDoc(app, "deals", "d1", map[string]any{"code": "OP-1", "name": "Pergola", "owner": "Obchodník", "phaseId": "p2", "invoices": []any{map[string]any{"accId": "i1", "number": "Z2026001", "kind": "deposit", "status": "open"}}}))
	must(putDoc(app, "acc_out", "i1", map[string]any{"kind": "deposit", "number": "Z2026001", "vs": "123", "total": 2.0, "status": "issued", "dealId": "d1"}))

	if !bankFromMail(app, &parsedMail{Subject: "ČSOB CEB", Text: csobSample}) {
		t.Fatal("notification not taken")
	}
	// second sync of the same mail: nothing new
	bankFromMail(app, &parsedMail{Subject: "ČSOB CEB", Text: csobSample})
	recs, _ := app.FindRecordsByFilter("docs", "col = 'acc_bank'", "", 0, 0)
	if len(recs) != 1 {
		t.Fatalf("bank movements: %d", len(recs))
	}
	b := docData(recs[0])
	if b["acct"] != "a1" || b["amount"] != 2.0 || b["match"] == nil {
		t.Fatalf("movement: %+v", b)
	}
	inv, _ := findDoc(app, "acc_out", "i1")
	if d := docData(inv); d["status"] != "paid" {
		t.Fatalf("invoice: %+v", d)
	}
	drec, _ := findDoc(app, "deals", "d1")
	d := docData(drec)
	if d["phaseId"] != "p3" {
		t.Fatalf("deal phase %v", d["phaseId"])
	}
	if e := d["invoices"].([]any)[0].(map[string]any); e["status"] != "paid" {
		t.Fatalf("deal invoice %+v", e)
	}
	notes, _ := app.FindRecordsByFilter("docs", "col = 'notifs'", "", 0, 0)
	got := map[string]string{}
	for _, n := range notes {
		x := docData(n)
		got[str(x, "to")] += str(x, "text") + " | "
	}
	if got["Petr"] == "" || got["Obchodník"] == "" {
		t.Fatalf("notifications: %v", got)
	}
	t.Log(got)
}
