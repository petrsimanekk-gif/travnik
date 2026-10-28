package main

// Drafts and deleting: a draft is kept in the mailbox's Drafts folder (so the webmail
// shows it too); deleting moves a message to Trash, and deleting in Trash removes it.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// draftInfo is what a draft needs besides its text to be sent as written.
type draftInfo struct {
	SigID   string          `json:"sigId,omitempty"`
	Offer   string          `json:"offer,omitempty"`
	ReplyTo string          `json:"replyTo,omitempty"`
	CommID  string          `json:"commId,omitempty"`
	Ctx     json.RawMessage `json:"ctx,omitempty"`
}

func draftMeta(b *sendReq) string {
	ctx := b.Ctx
	if len(ctx) > 600 {
		ctx = nil
	}
	j, _ := json.Marshal(draftInfo{SigID: b.SigID, Offer: b.Offer, ReplyTo: b.ReplyTo, CommID: b.CommID, Ctx: ctx})
	return base64.StdEncoding.EncodeToString(j)
}

// readDraftMeta finds the X-CRM-Draft header in a stored draft.
func readDraftMeta(raw []byte) map[string]any {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		end = len(raw)
	}
	for _, l := range strings.Split(string(raw[:end]), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "X-CRM-Draft") {
			j, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
			if err != nil {
				return nil
			}
			var m map[string]any
			_ = json.Unmarshal(j, &m)
			return m
		}
	}
	return nil
}

// mailEditor: the account may send from (and so change) this mailbox.
func mailEditor(e *core.RequestEvent, box string) (map[string]any, *mailCfg, *mailState, error) {
	a, err := mailAccount(e, "view")
	if err != nil {
		return nil, nil, nil, err
	}
	_, cfg, st, err := loadBox(e.App, box)
	if err != nil || !boxAllowed(a, cfg) || (mailPerm(a) != "edit" && !boxOwned(a, cfg)) {
		return nil, nil, nil, e.ForbiddenError("permission_denied", nil)
	}
	return a, cfg, st, nil
}

// removeMails deletes messages of one mailbox: into Trash, or for good when they are in
// Trash already (or forever is set, or the mailbox has no Trash).
func removeMails(app core.App, box string, recs []*core.Record, forever bool) error {
	_, cfg, st, err := loadBox(app, box)
	if err != nil {
		return err
	}
	byFolder := map[string][]*core.Record{}
	for _, r := range recs {
		if r.GetString("box") == box {
			byFolder[r.GetString("folder")] = append(byFolder[r.GetString("folder")], r)
		}
	}
	l := mailLock(box)
	l.Lock()
	defer l.Unlock()
	ic, err := imapDial(cfg)
	if err != nil {
		return err
	}
	defer ic.Close()
	trash := ""
	if f := st.Folders["trash"]; f != nil {
		trash = f.Name
	}
	for folder, list := range byFolder {
		var uids imap.UIDSet
		for _, r := range list {
			if u := r.GetInt("uid"); u > 0 {
				uids.AddNum(imap.UID(u))
			}
		}
		if len(uids) > 0 {
			fs := st.Folders[folder]
			if fs == nil {
				return errors.New("složka není načtená")
			}
			if _, err := ic.Select(fs.Name, nil).Wait(); err != nil {
				return err
			}
			if folder != "trash" && trash != "" && !forever {
				if _, err := ic.Move(uids, trash).Wait(); err != nil {
					return err
				}
			} else {
				if err := ic.Store(uids, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
					return err
				}
				if ic.Caps().Has(imap.CapUIDPlus) || ic.Caps().Has(imap.CapIMAP4rev2) {
					err = ic.UIDExpunge(uids).Close()
				} else {
					err = ic.Expunge().Close()
				}
				if err != nil {
					return err
				}
			}
		}
		for _, r := range list {
			_ = app.Delete(r)
		}
	}
	_ = ic.Logout().Wait()
	return nil
}

func handleMailDel(e *core.RequestEvent) error {
	var b struct {
		IDs     []string `json:"ids"`
		Forever bool     `json:"forever"`
	}
	if err := e.BindBody(&b); err != nil || len(b.IDs) == 0 || len(b.IDs) > 200 {
		return e.BadRequestError("", err)
	}
	byBox := map[string][]*core.Record{}
	for _, id := range b.IDs {
		r, err := e.App.FindRecordById("mails", id)
		if err != nil {
			continue
		}
		byBox[r.GetString("box")] = append(byBox[r.GetString("box")], r)
	}
	for box := range byBox {
		if _, _, _, err := mailEditor(e, box); err != nil {
			return err
		}
	}
	for box, recs := range byBox {
		if err := removeMails(e.App, box, recs, b.Forever); err != nil {
			return e.BadRequestError("Smazání se nepovedlo: "+err.Error(), nil)
		}
		publishMailState(e.App, box)
		go func(box string) { _ = syncBox(e.App, box) }(box)
	}
	return e.NoContent(http.StatusNoContent)
}

// handleMailDraft keeps the compose dialog's content in the Drafts folder.
func handleMailDraft(e *core.RequestEvent) error {
	var b sendReq
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	if _, _, _, err := mailEditor(e, b.Box); err != nil {
		return err
	}
	id, err := saveDraft(e.App, &b)
	if err != nil {
		var ue userErr
		if errors.As(err, &ue) {
			return e.BadRequestError(ue.msg, nil)
		}
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"id": id})
}

// saveDraft appends the e-mail to the mailbox's Drafts folder (made if missing),
// removes the draft it replaces and gives the id of the stored copy.
func saveDraft(app core.App, b *sendReq) (string, error) {
	_, cfg, st, err := loadBox(app, b.Box)
	if err != nil {
		return "", userErr{err.Error()}
	}
	if _, err := parseAddrs(b.To); err != nil {
		return "", userErr{err.Error()}
	}
	if _, err := parseAddrs(b.Cc); err != nil {
		return "", userErr{err.Error()}
	}
	m, err := composeMail(app, cfg, b, true)
	if err != nil {
		return "", err
	}
	name := ""
	if f := st.Folders["drafts"]; f != nil {
		name = f.Name
	}
	l := mailLock(b.Box)
	l.Lock()
	ic, err := imapDial(cfg)
	if err != nil {
		l.Unlock()
		return "", userErr{err.Error()}
	}
	if name == "" {
		name = folderNames(ic)["drafts"]
	}
	if name == "" { // no Drafts folder yet: make one
		name = "Drafts"
		_ = ic.Create(name, nil).Wait()
	}
	ac := ic.Append(name, int64(len(m.raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagDraft, imap.FlagSeen}, Time: time.Now()})
	_, werr := ac.Write(m.raw)
	if werr == nil {
		werr = ac.Close()
	}
	if werr == nil {
		_, werr = ac.Wait()
	}
	_ = ic.Logout().Wait()
	ic.Close()
	l.Unlock()
	if werr != nil {
		return "", userErr{"Koncept se nepodařilo uložit: " + werr.Error()}
	}
	// the previous version of this draft goes away
	if b.Draft != "" {
		if r, err := app.FindRecordById("mails", b.Draft); err == nil && r.GetString("box") == b.Box && r.GetString("folder") == "drafts" {
			_ = removeMails(app, b.Box, []*core.Record{r}, true)
		}
	}
	_ = syncBox(app, b.Box)
	id := ""
	if r, err := app.FindFirstRecordByFilter("mails", "box = {:b} && folder = 'drafts' && msgid = {:m}", dbx.Params{"b": b.Box, "m": m.msgID}); err == nil {
		id = r.Id
	}
	publishMailState(app, b.Box)
	return id, nil
}

// dropSentDraft removes the draft an e-mail was written from, once it has gone out.
func dropSentDraft(app core.App, b *sendReq) {
	if b.Draft == "" {
		return
	}
	r, err := app.FindRecordById("mails", b.Draft)
	if err != nil || r.GetString("box") != b.Box || r.GetString("folder") != "drafts" {
		return
	}
	if err := removeMails(app, b.Box, []*core.Record{r}, true); err != nil {
		app.Logger().Warn("draft remove", "box", b.Box, "err", err)
	}
	publishMailState(app, b.Box)
}
