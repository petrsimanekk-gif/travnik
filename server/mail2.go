package main

// Pošta, part 2: who may use which mailbox, and e-mails scheduled for later.
//
//	mailqueue – a scheduled e-mail: the compose request as sent by the page,
//	            sent by the background loop at sendAt under its author's rights.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

const queuePoll = 20 * time.Second

// boxAllowed: an empty access list means everyone with the Pošta permission.
func boxAllowed(a map[string]any, cfg *mailCfg) bool {
	if a == nil || cfg == nil {
		return false
	}
	if a["role"] == "admin" || len(cfg.Access) == 0 {
		return true
	}
	id, _ := a["id"].(string)
	for _, x := range cfg.Access {
		if x == id {
			return true
		}
	}
	return false
}

func allowedBoxes(app core.App, a map[string]any) []string {
	var out []string
	recs, _ := app.FindAllRecords("mailboxes")
	for _, r := range recs {
		if _, cfg, _, err := loadBox(app, r.GetString("key")); err == nil && boxAllowed(a, cfg) {
			out = append(out, r.GetString("key"))
		}
	}
	return out
}

func boxKeyAllowed(app core.App, a map[string]any, key string) bool {
	_, cfg, _, err := loadBox(app, key)
	return err == nil && boxAllowed(a, cfg)
}

// ---------- scheduled sending ----------

func ensureQueueSchema(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("mailqueue"); err == nil {
		return nil
	}
	c := core.NewBaseCollection("mailqueue")
	c.Fields.Add(
		&core.TextField{Name: "box", Required: true, Max: 100},
		&core.TextField{Name: "acc", Max: 300},
		&core.DateField{Name: "sendAt"},
		&core.JSONField{Name: "req", MaxSize: 5 << 20},
		&core.TextField{Name: "status", Max: 20}, // wait | hold (being edited) | sending | error
		&core.TextField{Name: "error", Max: 2000},
	)
	c.AddIndex("idx_mailqueue_status", false, "`status`,`sendAt`", "")
	return app.Save(c)
}

func queueMail(app core.App, a map[string]any, b *sendReq) (string, error) {
	coll, err := app.FindCachedCollectionByNameOrId("mailqueue")
	if err != nil {
		return "", err
	}
	rec := core.NewRecord(coll)
	rec.Set("box", b.Box)
	rec.Set("acc", a["id"])
	rec.Set("sendAt", time.UnixMilli(b.SendAt).UTC())
	rec.Set("req", b)
	rec.Set("status", "wait")
	if err := app.Save(rec); err != nil {
		return "", err
	}
	publishMailState(app, b.Box)
	return rec.Id, nil
}

func queueReq(r *core.Record) *sendReq {
	b := &sendReq{}
	if raw, ok := r.Get("req").(types.JSONRaw); ok {
		_ = json.Unmarshal(raw, b)
	}
	return b
}

func startQueueLoop(app core.App) {
	// a send cut short by a restart may or may not have gone out: never repeat it on its own
	if recs, err := app.FindAllRecords("mailqueue", dbx.HashExp{"status": "sending"}); err == nil {
		for _, r := range recs {
			r.Set("status", "error")
			r.Set("error", "Odesílání přerušil restart serveru. Zkontrolujte Odeslané a případně odešlete znovu.")
			_ = app.Save(r)
		}
	}
	go func() {
		for {
			processQueue(app)
			time.Sleep(queuePoll)
		}
	}()
}

var queueKick = make(chan struct{}, 1)

func processQueue(app core.App) {
	select {
	case queueKick <- struct{}{}:
	default:
		return // already running
	}
	defer func() { <-queueKick }()
	recs, err := app.FindRecordsByFilter("mailqueue", "status = 'wait' && sendAt <= {:now}", "sendAt", 50, 0, dbx.Params{"now": types.NowDateTime()})
	if err != nil {
		return
	}
	for _, r := range recs {
		r.Set("status", "sending")
		if err := app.Save(r); err != nil {
			continue
		}
		b := queueReq(r)
		a, _ := loadAccount(app, r.GetString("acc"))
		_, cfg, _, cerr := loadBox(app, b.Box)
		switch {
		case !isActive(a) || mailPerm(a) != "edit" || cerr != nil || !boxAllowed(a, cfg):
			err = errors.New("Autor už nemá oprávnění odesílat z této schránky.")
		default:
			_, err = doSend(app, a, b)
		}
		if err != nil {
			r.Set("status", "error")
			r.Set("error", clip(err.Error(), 2000))
			_ = app.Save(r)
			if name, _ := a["name"].(string); name != "" {
				_ = putDoc(app, "notifs", randUUID(), map[string]any{"to": name, "text": "Naplánovaný e-mail „" + clip(b.Subject, 80) + "“ se nepodařilo odeslat: " + clip(err.Error(), 200),
					"at": time.Now().UnixMilli(), "read": false, "dealId": "", "leadId": "", "by": "Pošta", "mail": b.Box})
			}
			publishMailState(app, b.Box)
			continue
		}
		_ = app.Delete(r)
		publishMailState(app, b.Box)
	}
}

func queueItem(app core.App, r *core.Record) map[string]any {
	b := queueReq(r)
	by := ""
	if a, _ := loadAccount(app, r.GetString("acc")); a != nil {
		by, _ = a["name"].(string)
	}
	return map[string]any{"id": r.Id, "box": r.GetString("box"), "to": b.To, "cc": b.Cc, "subject": b.Subject, "snippet": clip(spaceRe.ReplaceAllString(b.Text, " "), 240),
		"sendAt": r.GetDateTime("sendAt").Time().UnixMilli(), "status": r.GetString("status"), "error": r.GetString("error"), "by": by, "acc": r.GetString("acc"), "att": len(b.Att)}
}

func handleMailQueue(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	boxes := allowedBoxes(e.App, a)
	if b := e.Request.URL.Query().Get("box"); b != "" {
		if !boxKeyAllowed(e.App, a, b) {
			return e.ForbiddenError("permission_denied", nil)
		}
		boxes = []string{b}
	}
	out := []map[string]any{}
	if len(boxes) > 0 {
		recs, _ := e.App.FindAllRecords("mailqueue", dbx.In("box", toAny(boxes)...))
		for _, r := range recs {
			out = append(out, queueItem(e.App, r))
		}
	}
	return e.JSON(http.StatusOK, map[string]any{"items": out})
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, x := range l {
		out[i] = x
	}
	return out
}

// queuedRecord finds a scheduled e-mail the caller may change (author or admin).
func queuedRecord(e *core.RequestEvent) (*core.Record, map[string]any, error) {
	a, err := mailAccount(e, "edit")
	if err != nil {
		return nil, nil, err
	}
	var b struct{ ID string }
	if err := e.BindBody(&b); err != nil {
		return nil, nil, e.BadRequestError("", err)
	}
	r, err := e.App.FindRecordById("mailqueue", b.ID)
	if err != nil {
		return nil, nil, e.NotFoundError("E-mail už ve frontě není (možná právě odešel).", nil)
	}
	if a["role"] != "admin" && r.GetString("acc") != a["id"] {
		return nil, nil, e.ForbiddenError("Naplánovaný e-mail může změnit jen jeho autor nebo admin.", nil)
	}
	if r.GetString("status") == "sending" {
		return nil, nil, e.BadRequestError("E-mail se právě odesílá.", nil)
	}
	return r, a, nil
}

func handleMailUnqueue(e *core.RequestEvent) error {
	r, _, err := queuedRecord(e)
	if err != nil {
		return err
	}
	b := queueReq(r)
	if err := e.App.Delete(r); err != nil {
		return err
	}
	publishMailState(e.App, r.GetString("box"))
	// the page reopens it in the compose dialog when "Upravit" was clicked
	return e.JSON(http.StatusOK, b)
}

func handleMailSendNow(e *core.RequestEvent) error {
	r, _, err := queuedRecord(e)
	if err != nil {
		return err
	}
	r.Set("sendAt", types.NowDateTime())
	r.Set("status", "wait")
	r.Set("error", "")
	if err := e.App.Save(r); err != nil {
		return err
	}
	go processQueue(e.App)
	return e.NoContent(http.StatusNoContent)
}

func queueCount(app core.App, box string) int64 {
	n, _ := app.CountRecords("mailqueue", dbx.HashExp{"box": box})
	return n
}

func boxesFilter(boxes []string) (string, map[string]any) {
	var or []string
	p := map[string]any{}
	for i, b := range boxes {
		k := "bx" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		or, p[k] = append(or, "box = {:"+k+"}"), b
	}
	return "(" + strings.Join(or, " || ") + ")", p
}

// handleMailHold keeps a scheduled e-mail from going out while it is edited.
func handleMailHold(e *core.RequestEvent) error {
	r, _, err := queuedRecord(e)
	if err != nil {
		return err
	}
	r.Set("status", "hold")
	if err := e.App.Save(r); err != nil {
		return err
	}
	publishMailState(e.App, r.GetString("box"))
	out := map[string]any{"id": r.Id, "sendAt": r.GetDateTime("sendAt").Time().UnixMilli(), "req": queueReq(r)}
	return e.JSON(http.StatusOK, out)
}

// handleMailRelease puts it back when the edit is cancelled.
func handleMailRelease(e *core.RequestEvent) error {
	r, _, err := queuedRecord(e)
	if err != nil {
		return err
	}
	r.Set("status", "wait")
	r.Set("error", "")
	if err := e.App.Save(r); err != nil {
		return err
	}
	publishMailState(e.App, r.GetString("box"))
	return e.NoContent(http.StatusNoContent)
}
