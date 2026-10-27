package main

// Pošta: company mailboxes read over IMAP and sent through SMTP.
//
//	mailboxes – one record per mailbox: cfg (hosts, login, password) + state
//	            (UIDVALIDITY / last UID per folder). Never exposed by the API.
//	mails     – one record per message (headers for the list + the raw RFC 822
//	            source as a protected file). Read only through /api/stinko/mail/*.
//
// A background loop polls every enabled mailbox; after a change the docs
// record mailstate/{box} is updated so open CRM pages refresh over realtime.

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomsg "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // windows-1250, iso-8859-2, …
	"github.com/emersion/go-message/mail"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"
)

const (
	mailPoll       = 2 * time.Minute
	mailFirstInbox = 500 // messages taken on the first sync of a folder
	mailFirstSent  = 300
	mailMaxRaw     = 40 << 20 // bigger messages keep only their header
	mailPageSize   = 50
)

type mailCfg struct {
	Name      string   `json:"name"`
	Email     string   `json:"email"`
	FromName  string   `json:"fromName"`
	User      string   `json:"user"`
	Password  string   `json:"password,omitempty"`
	ImapHost  string   `json:"imapHost"`
	ImapPort  int      `json:"imapPort"`
	SmtpHost  string   `json:"smtpHost"`
	SmtpPort  int      `json:"smtpPort"`
	Security  string   `json:"security"` // ssl (default) | starttls | none (local tests only)
	Enabled   bool     `json:"enabled"`
	Signature string   `json:"signature"`
	SigLogo   string   `json:"sigLogo"` // blob key of the logo under the mailbox signature
	Access    []string `json:"access"`  // account ids; empty = everyone with Pošta
	Owner     string   `json:"owner"`   // account that connected its own mailbox (it may edit it)
}

type mailFolderState struct {
	Name string `json:"name"`
	UIDV uint32 `json:"uidv"`
	Last uint32 `json:"last"`
}

type mailState struct {
	Folders  map[string]*mailFolderState `json:"folders"`
	LastSync int64                       `json:"lastSync"`
	Error    string                      `json:"error"`
}

var (
	mailLocksMu sync.Mutex
	mailLocks   = map[string]*sync.Mutex{}
)

func mailLock(box string) *sync.Mutex {
	mailLocksMu.Lock()
	defer mailLocksMu.Unlock()
	if mailLocks[box] == nil {
		mailLocks[box] = &sync.Mutex{}
	}
	return mailLocks[box]
}

// ---------- schema ----------

func ensureMailSchema(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("mailboxes"); err != nil {
		c := core.NewBaseCollection("mailboxes")
		c.Fields.Add(
			&core.TextField{Name: "key", Required: true, Max: 100},
			&core.JSONField{Name: "cfg", MaxSize: 1 << 20},
			&core.JSONField{Name: "state", MaxSize: 1 << 20},
		)
		c.AddIndex("idx_mailboxes_key", true, "`key`", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCollectionByNameOrId("mails"); err != nil {
		c := core.NewBaseCollection("mails")
		c.Fields.Add(
			&core.TextField{Name: "box", Required: true, Max: 100},
			&core.TextField{Name: "folder", Required: true, Max: 20},
			&core.NumberField{Name: "uid", OnlyInt: true},
			&core.TextField{Name: "msgid", Max: 1000},
			&core.TextField{Name: "subject", Max: 2000},
			&core.TextField{Name: "from", Max: 500},
			&core.TextField{Name: "fromName", Max: 500},
			&core.JSONField{Name: "to", MaxSize: 1 << 20},
			&core.JSONField{Name: "cc", MaxSize: 1 << 20},
			&core.TextField{Name: "addrs", Max: 50000},
			&core.DateField{Name: "date"},
			&core.TextField{Name: "snippet", Max: 1000},
			&core.BoolField{Name: "seen"},
			&core.JSONField{Name: "att", MaxSize: 1 << 20},
			&core.NumberField{Name: "size", OnlyInt: true},
			&core.TextField{Name: "leadId", Max: 300},
			&core.FileField{Name: "raw", MaxSelect: 1, MaxSize: mailMaxRaw + (1 << 20), Protected: true},
		)
		c.AddIndex("idx_mails_uid", false, "`box`,`folder`,`uid`", "")
		c.AddIndex("idx_mails_date", false, "`box`,`folder`,`date`", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	return ensureQueueSchema(app)
}

func loadBox(app core.App, key string) (*core.Record, *mailCfg, *mailState, error) {
	rec, err := app.FindFirstRecordByData("mailboxes", "key", key)
	if err != nil {
		return nil, nil, nil, errors.New("Schránka neexistuje.")
	}
	cfg, st := &mailCfg{}, &mailState{}
	if raw, ok := rec.Get("cfg").(types.JSONRaw); ok && len(raw) > 0 {
		_ = json.Unmarshal(raw, cfg)
	}
	if raw, ok := rec.Get("state").(types.JSONRaw); ok && len(raw) > 0 {
		_ = json.Unmarshal(raw, st)
	}
	if st.Folders == nil {
		st.Folders = map[string]*mailFolderState{}
	}
	return rec, cfg, st, nil
}

func saveState(app core.App, key string, st *mailState) {
	// the record is re-read: the admin may have saved new settings during a sync
	rec, err := app.FindFirstRecordByData("mailboxes", "key", key)
	if err != nil {
		return
	}
	rec.Set("state", st)
	if err := app.Save(rec); err != nil {
		app.Logger().Warn("mail state", "box", key, "err", err)
	}
}

// ---------- connections ----------

var mailDialer = &net.Dialer{Timeout: 20 * time.Second}

func imapDial(cfg *mailCfg) (*imapclient.Client, error) {
	addr := net.JoinHostPort(cfg.ImapHost, strconv.Itoa(cfg.ImapPort))
	opt := &imapclient.Options{Dialer: mailDialer}
	var (
		c   *imapclient.Client
		err error
	)
	switch cfg.Security {
	case "starttls":
		c, err = imapclient.DialStartTLS(addr, opt)
	case "none":
		c, err = imapclient.DialInsecure(addr, opt)
	default:
		c, err = imapclient.DialTLS(addr, opt)
	}
	if err != nil {
		return nil, fmt.Errorf("IMAP %s: %w", addr, err)
	}
	if err := c.Login(cfg.User, cfg.Password).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("IMAP přihlášení se nepovedlo (jméno nebo heslo?): %w", err)
	}
	return c, nil
}

func smtpDial(cfg *mailCfg) (*smtp.Client, error) {
	addr := net.JoinHostPort(cfg.SmtpHost, strconv.Itoa(cfg.SmtpPort))
	var (
		c   *smtp.Client
		err error
	)
	if cfg.Security == "ssl" || cfg.Security == "" {
		conn, derr := tls.DialWithDialer(mailDialer, "tcp", addr, &tls.Config{ServerName: cfg.SmtpHost})
		if derr != nil {
			return nil, fmt.Errorf("SMTP %s: %w", addr, derr)
		}
		c, err = smtp.NewClient(conn, cfg.SmtpHost)
	} else {
		conn, derr := mailDialer.Dial("tcp", addr)
		if derr != nil {
			return nil, fmt.Errorf("SMTP %s: %w", addr, derr)
		}
		c, err = smtp.NewClient(conn, cfg.SmtpHost)
		if err == nil && cfg.Security == "starttls" {
			err = c.StartTLS(&tls.Config{ServerName: cfg.SmtpHost})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP %s: %w", addr, err)
	}
	if ok, _ := c.Extension("AUTH"); ok {
		auth := smtp.PlainAuth("", cfg.User, cfg.Password, cfg.SmtpHost)
		if cfg.Security == "none" {
			auth = plainAnyAuth{cfg.User, cfg.Password}
		}
		if err := c.Auth(auth); err != nil {
			c.Close()
			return nil, fmt.Errorf("SMTP přihlášení se nepovedlo: %w", err)
		}
	}
	return c, nil
}

// plainAnyAuth is PLAIN without net/smtp's TLS check, for a local test server only.
type plainAnyAuth struct{ user, pw string }

func (a plainAnyAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pw), nil
}
func (a plainAnyAuth) Next([]byte, bool) ([]byte, error) { return nil, nil }

// folderNames finds the inbox and the sent folder (special-use flag, else by name).
func folderNames(c *imapclient.Client) map[string]string {
	out := map[string]string{"inbox": "INBOX"}
	list, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		list, _ = c.List("", "*", nil).Collect()
	}
	for _, l := range list {
		for _, a := range l.Attrs {
			if a == imap.MailboxAttrSent {
				out["sent"] = l.Mailbox
			}
		}
	}
	if out["sent"] == "" {
		for _, l := range list {
			n := strings.ToLower(l.Mailbox)
			if i := strings.LastIndexAny(n, "/."); i >= 0 {
				n = n[i+1:]
			}
			if n == "sent" || n == "sent items" || n == "sent messages" || strings.HasPrefix(n, "odeslan") {
				out["sent"] = l.Mailbox
				break
			}
		}
	}
	return out
}

// ---------- parsing ----------

type mailAtt struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int    `json:"size"`
	CID  string `json:"cid,omitempty"`
	data []byte
}

type parsedMail struct {
	Subject, From, FromName, MsgID string
	InReplyTo                      string
	References                     []string
	To, Cc                         []map[string]string
	Date                           time.Time
	Text, HTML                     string
	Atts                           []mailAtt
}

func addrList(h *mail.Header, key string) []map[string]string {
	out := []map[string]string{}
	l, _ := h.AddressList(key)
	for _, a := range l {
		out = append(out, map[string]string{"name": a.Name, "addr": strings.ToLower(a.Address)})
	}
	return out
}

func parseMail(raw []byte) *parsedMail {
	p := &parsedMail{}
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		p.Text = string(raw)
		return p
	}
	h := &mr.Header
	p.Subject, _ = h.Subject()
	if from := addrList(h, "From"); len(from) > 0 {
		p.From, p.FromName = from[0]["addr"], from[0]["name"]
	}
	p.To, p.Cc = addrList(h, "To"), addrList(h, "Cc")
	p.Date, _ = h.Date()
	p.MsgID, _ = h.MessageID()
	if l, _ := h.MsgIDList("In-Reply-To"); len(l) > 0 {
		p.InReplyTo = l[0]
	}
	p.References, _ = h.MsgIDList("References")
	if err != nil && !strings.Contains(err.Error(), "charset") {
		return p
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(io.LimitReader(part.Body, mailMaxRaw))
		switch ph := part.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := ph.ContentType()
			cid := strings.Trim(ph.Get("Content-Id"), "<> ")
			switch {
			case ct == "text/plain" && p.Text == "" && cid == "":
				p.Text = string(b)
			case ct == "text/html" && p.HTML == "" && cid == "":
				p.HTML = string(b)
			case strings.HasPrefix(ct, "text/") && cid == "":
				// further text parts (rare) are shown as attachments
				p.Atts = append(p.Atts, mailAtt{Name: "text." + strings.TrimPrefix(ct, "text/"), Type: ct, Size: len(b), data: b})
			default:
				_, params, _ := ph.ContentType()
				name := params["name"]
				if name == "" {
					name = "soubor"
				}
				p.Atts = append(p.Atts, mailAtt{Name: name, Type: ct, Size: len(b), CID: cid, data: b})
			}
		case *mail.AttachmentHeader:
			ct, params, _ := ph.ContentType()
			name, _ := ph.Filename()
			if name == "" {
				name = params["name"]
			}
			if name == "" {
				name = "příloha"
			}
			p.Atts = append(p.Atts, mailAtt{Name: name, Type: ct, Size: len(b), CID: strings.Trim(ph.Get("Content-Id"), "<> "), data: b})
		}
	}
	return p
}

var (
	tagRe   = regexp.MustCompile(`(?is)<(script|style|head)[^>]*>.*?</(script|style|head)>|<[^>]+>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

func htmlToText(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(html.UnescapeString(tagRe.ReplaceAllString(s, " ")), " "))
}

func snippetOf(p *parsedMail) string {
	t := p.Text
	if strings.TrimSpace(t) == "" {
		t = htmlToText(p.HTML)
	}
	// skip quoted lines of a reply
	var lines []string
	for _, l := range strings.Split(t, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), ">") {
			lines = append(lines, l)
		}
	}
	return clip(spaceRe.ReplaceAllString(strings.Join(lines, " "), " "), 240)
}

func addrsField(p *parsedMail) string {
	set := map[string]bool{p.From: true}
	for _, l := range [][]map[string]string{p.To, p.Cc} {
		for _, a := range l {
			set[a["addr"]] = true
		}
	}
	// " a@x b@y ": the spaces let the list filter match whole addresses only
	var b strings.Builder
	b.WriteString(" ")
	for a := range set {
		if a != "" && b.Len()+len(a) < 49000 {
			b.WriteString(a + " ")
		}
	}
	return b.String()
}

func fillMailRecord(rec *core.Record, p *parsedMail, raw []byte, size int64, when time.Time) error {
	date := p.Date
	if date.IsZero() || date.After(time.Now().Add(48*time.Hour)) {
		date = when
	}
	var att []map[string]any
	for _, a := range p.Atts {
		if a.CID == "" || !strings.Contains(p.HTML, "cid:"+a.CID) {
			att = append(att, map[string]any{"name": a.Name, "size": a.Size})
		}
	}
	rec.Set("msgid", clip(p.MsgID, 1000))
	rec.Set("subject", clip(p.Subject, 2000))
	rec.Set("from", clip(p.From, 500))
	rec.Set("fromName", clip(p.FromName, 500))
	rec.Set("to", p.To)
	rec.Set("cc", p.Cc)
	rec.Set("addrs", addrsField(p))
	rec.Set("date", date.UTC())
	rec.Set("snippet", snippetOf(p))
	rec.Set("att", att)
	rec.Set("size", size)
	if len(raw) > 0 {
		f, err := filesystem.NewFileFromBytes(raw, "mail.eml")
		if err != nil {
			return err
		}
		rec.Set("raw", f)
	}
	return nil
}

// ---------- sync ----------

func startMailLoop(app core.App) {
	startQueueLoop(app)
	go func() {
		time.Sleep(10 * time.Second)
		for {
			recs, _ := app.FindAllRecords("mailboxes")
			for _, r := range recs {
				_, cfg, _, err := loadBox(app, r.GetString("key"))
				if err == nil && cfg.Enabled {
					if err := syncBox(app, r.GetString("key")); err != nil {
						app.Logger().Warn("mail sync", "box", r.GetString("key"), "err", err)
					}
				}
			}
			time.Sleep(mailPoll)
		}
	}()
}

func syncBox(app core.App, key string) (err error) {
	l := mailLock(key)
	l.Lock()
	defer l.Unlock()
	_, cfg, st, err := loadBox(app, key)
	if err != nil {
		return err
	}
	changed := false
	defer func() {
		st.LastSync = time.Now().UnixMilli()
		prevErr := st.Error
		st.Error = ""
		if err != nil {
			st.Error = err.Error()
		}
		saveState(app, key, st)
		if changed || prevErr != st.Error {
			publishMailState(app, key)
		}
	}()
	c, err := imapDial(cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	names := folderNames(c)
	for _, folder := range []string{"inbox", "sent"} {
		name := names[folder]
		if name == "" {
			continue
		}
		ch, ferr := syncFolder(app, c, key, folder, name, st)
		changed = changed || ch
		if ferr != nil {
			return ferr
		}
	}
	_ = c.Logout().Wait()
	return nil
}

func syncFolder(app core.App, c *imapclient.Client, box, folder, name string, st *mailState) (bool, error) {
	fs := st.Folders[folder]
	if fs == nil {
		fs = &mailFolderState{}
		st.Folders[folder] = fs
	}
	sel, err := c.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return false, fmt.Errorf("složka %s: %w", name, err)
	}
	changed := false
	if fs.Name != name || fs.UIDV != sel.UIDValidity {
		// a different folder or renumbered UIDs: start this folder over
		if fs.UIDV != 0 || fs.Name != "" {
			recs, _ := app.FindAllRecords("mails", dbx.HashExp{"box": box, "folder": folder})
			for _, r := range recs {
				_ = app.Delete(r)
			}
			changed = true
		}
		fs.Name, fs.UIDV, fs.Last = name, sel.UIDValidity, 0
	}

	// flags of the messages we have, and which of them are gone from the server
	local, _ := app.FindAllRecords("mails", dbx.NewExp("box = {:b} AND folder = {:f} AND uid > 0", dbx.Params{"b": box, "f": folder}))
	if len(local) > 0 && sel.NumMessages > 0 && fs.Last > 0 {
		min := uint32(0)
		for _, r := range local {
			if u := uint32(r.GetInt("uid")); min == 0 || u < min {
				min = u
			}
		}
		var set imap.UIDSet
		set.AddRange(imap.UID(min), imap.UID(fs.Last))
		msgs, err := c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true}).Collect()
		if err != nil {
			return changed, err
		}
		seen := map[uint32]bool{}
		present := map[uint32]bool{}
		for _, m := range msgs {
			present[uint32(m.UID)] = true
			for _, f := range m.Flags {
				if f == imap.FlagSeen {
					seen[uint32(m.UID)] = true
				}
			}
		}
		for _, r := range local {
			u := uint32(r.GetInt("uid"))
			if u > fs.Last {
				continue
			}
			if !present[u] {
				_ = app.Delete(r)
				changed = true
			} else if r.GetBool("seen") != seen[u] {
				r.Set("seen", seen[u])
				_ = app.Save(r)
				changed = true
			}
		}
	} else if sel.NumMessages == 0 && len(local) > 0 {
		for _, r := range local {
			_ = app.Delete(r)
		}
		changed = true
	}
	if sel.NumMessages == 0 {
		return changed, nil
	}

	// new messages: everything after the last UID, or the latest N on the first sync
	var numSet imap.NumSet
	if fs.Last == 0 {
		n := uint32(mailFirstInbox)
		if folder == "sent" {
			n = mailFirstSent
		}
		from := uint32(1)
		if sel.NumMessages > n {
			from = sel.NumMessages - n + 1
		}
		var s imap.SeqSet
		s.AddRange(from, sel.NumMessages)
		numSet = s
	} else {
		var s imap.UIDSet
		s.AddRange(imap.UID(fs.Last+1), 0) // 0 = "*"
		numSet = s
	}
	heads, err := c.Fetch(numSet, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true}).Collect()
	if err != nil {
		return changed, err
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].UID < heads[j].UID })
	coll, err := app.FindCachedCollectionByNameOrId("mails")
	if err != nil {
		return changed, err
	}
	for i := 0; i < len(heads); i += 20 {
		batch := heads[i:min(i+20, len(heads))]
		var full, small imap.UIDSet
		meta := map[imap.UID]*imapclient.FetchMessageBuffer{}
		for _, h := range batch {
			if uint32(h.UID) <= fs.Last {
				continue
			}
			meta[h.UID] = h
			if h.RFC822Size > mailMaxRaw {
				small.AddNum(h.UID)
			} else {
				full.AddNum(h.UID)
			}
		}
		var got []*imapclient.FetchMessageBuffer
		for _, part := range []struct {
			set    imap.UIDSet
			header bool
		}{{full, false}, {small, true}} {
			if len(part.set) == 0 {
				continue
			}
			sec := &imap.FetchItemBodySection{Peek: true}
			if part.header {
				sec.Specifier = imap.PartSpecifierHeader
			}
			msgs, err := c.Fetch(part.set, &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}).Collect()
			if err != nil {
				return changed, err
			}
			got = append(got, msgs...)
		}
		sort.Slice(got, func(i, j int) bool { return got[i].UID < got[j].UID })
		for _, m := range got {
			h := meta[m.UID]
			if h == nil || len(m.BodySection) == 0 {
				continue
			}
			raw := m.BodySection[0].Bytes
			p := parseMail(raw)
			rec := core.NewRecord(coll)
			// a message this CRM sent is already stored without a UID: adopt it
			if folder == "sent" && p.MsgID != "" {
				if r, err := app.FindFirstRecordByFilter("mails", "box = {:b} && folder = 'sent' && uid = 0 && msgid = {:m}", dbx.Params{"b": box, "m": p.MsgID}); err == nil {
					rec = r
				}
			}
			rec.Set("box", box)
			rec.Set("folder", folder)
			rec.Set("uid", int(m.UID))
			seen := false
			for _, f := range h.Flags {
				seen = seen || f == imap.FlagSeen
			}
			rec.Set("seen", seen)
			keep := raw
			if h.RFC822Size > mailMaxRaw {
				keep = nil // header only: the message stays readable in the webmail
			}
			if err := fillMailRecord(rec, p, keep, h.RFC822Size, h.InternalDate); err != nil {
				return changed, err
			}
			if err := app.Save(rec); err != nil {
				app.Logger().Warn("mail save", "box", box, "uid", m.UID, "err", err)
			}
			if folder == "inbox" && !seen && fs.Last > 0 {
				notifyNewMail(app, box, p)
			}
			changed = true
		}
		for _, h := range batch {
			if uint32(h.UID) > fs.Last {
				fs.Last = uint32(h.UID)
			}
		}
		saveState(app, box, st)
	}
	return changed, nil
}

// publishMailState updates docs mailstate/{box}; its realtime event tells the
// open pages to reload the list and the unread badge.
func publishMailState(app core.App, box string) {
	n, _ := app.CountRecords("mails", dbx.HashExp{"box": box, "folder": "inbox", "seen": false})
	_, _, st, _ := loadBox(app, box)
	errText := ""
	if st != nil {
		errText = st.Error
	}
	_ = putDoc(app, "mailstate", box, map[string]any{"unread": n, "at": time.Now().UnixMilli(), "error": errText})
}

// notifyNewMail drops a CRM notification for the accounts that see Pošta.
func notifyNewMail(app core.App, box string, p *parsedMail) {
	who := p.FromName
	if who == "" {
		who = p.From
	}
	recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "accounts"})
	ms := time.Now().UnixMilli()
	_, cfg, _, _ := loadBox(app, box)
	for _, r := range recs {
		a := docData(r)
		a["id"] = r.GetString("key")
		if !isActive(a) || mailPerm(a) == "none" || !inPosta(a, cfg) {
			continue
		}
		name, _ := a["name"].(string)
		n := map[string]any{"to": name, "text": "Nový e-mail od " + who + ": " + clip(p.Subject, 120), "at": ms, "read": false, "dealId": "", "leadId": "", "by": "Pošta", "mail": box}
		_ = putDoc(app, "notifs", randUUID(), n)
	}
}

// ---------- API ----------

func mailPerm(a map[string]any) string {
	if a == nil {
		return "none"
	}
	if a["role"] == "admin" {
		return "edit"
	}
	perms, _ := a["perms"].(map[string]any)
	if p, _ := perms["mail"].(string); p == "view" || p == "edit" {
		return p
	}
	return "none"
}

func mailAccount(e *core.RequestEvent, need string) (map[string]any, error) {
	a, err := authAccount(e)
	if err != nil {
		return nil, err
	}
	p := mailPerm(a)
	if p == "none" || (need == "edit" && p != "edit") || (need == "admin" && a["role"] != "admin") {
		return nil, e.ForbiddenError("permission_denied", nil)
	}
	return a, nil
}

func bindMailRoutes(se *core.ServeEvent) {
	r := se.Router
	r.GET("/api/stinko/mail/boxes", handleMailBoxes)
	r.POST("/api/stinko/mail/box", handleMailBoxSave)
	r.POST("/api/stinko/mail/boxdel", handleMailBoxDel)
	r.POST("/api/stinko/mail/test", handleMailTest)
	r.POST("/api/stinko/mail/sync", handleMailSync)
	r.GET("/api/stinko/mail/list", handleMailList)
	r.GET("/api/stinko/mail/msg", handleMailMsg)
	r.GET("/api/stinko/mail/att", handleMailAtt)
	r.POST("/api/stinko/mail/seen", handleMailSeen)
	r.POST("/api/stinko/mail/send", handleMailSend)
	r.POST("/api/stinko/mail/tolead", handleMailToLead)
	r.GET("/api/stinko/mail/queue", handleMailQueue)
	r.POST("/api/stinko/mail/unqueue", handleMailUnqueue)
	r.POST("/api/stinko/mail/sendnow", handleMailSendNow)
	r.POST("/api/stinko/mail/hold", handleMailHold)
	r.POST("/api/stinko/mail/auto", handleMailAuto)
	r.POST("/api/stinko/mail/release", handleMailRelease)
}

func handleMailBoxes(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	recs, _ := e.App.FindAllRecords("mailboxes")
	out := []map[string]any{}
	for _, r := range recs {
		key := r.GetString("key")
		_, cfg, st, _ := loadBox(e.App, key)
		if !boxAllowed(a, cfg) {
			continue
		}
		unread, _ := e.App.CountRecords("mails", dbx.HashExp{"box": key, "folder": "inbox", "seen": false})
		m := map[string]any{"id": key, "name": cfg.Name, "email": cfg.Email, "fromName": cfg.FromName, "enabled": cfg.Enabled, "signature": cfg.Signature, "sigLogo": cfg.SigLogo,
			"lastSync": st.LastSync, "error": st.Error, "unread": unread, "hasSent": st.Folders["sent"] != nil, "queued": queueCount(e.App, key)}
		m["owner"], m["inPosta"] = cfg.Owner, inPosta(a, cfg)
		if boxOwned(a, cfg) {
			m["mine"] = true
			m["user"], m["imapHost"], m["imapPort"], m["smtpHost"], m["smtpPort"], m["security"] = cfg.User, cfg.ImapHost, cfg.ImapPort, cfg.SmtpHost, cfg.SmtpPort, cfg.Security
			m["hasPassword"] = cfg.Password != ""
			m["access"] = cfg.Access
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"]) })
	return e.JSON(http.StatusOK, map[string]any{"boxes": out})
}

// boxFromBody reads mailbox settings sent by the page; an empty password keeps the stored one.
// publicMailHost keeps a user's own mailbox to real mail servers: usual ports, public
// addresses (not this server or the local network) and an encrypted connection.
func publicMailHost(cfg *mailCfg) error {
	if os.Getenv("STINKO_MAIL_TEST_LOCAL") == "1" {
		return nil // local test runs against a fake mail server on 127.0.0.1
	}
	ports := map[int]bool{993: true, 143: true, 465: true, 587: true, 25: true, 2525: true}
	if !ports[cfg.ImapPort] || !ports[cfg.SmtpPort] {
		return errors.New("Nepovolený port. Použijte porty poštovního serveru (IMAP 993, SMTP 465 nebo 587).")
	}
	if cfg.Security == "none" {
		return errors.New("Spojení se schránkou musí být šifrované (SSL/TLS nebo STARTTLS).")
	}
	for _, h := range []string{cfg.ImapHost, cfg.SmtpHost} {
		ips, err := net.LookupIP(h)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("Server %s se nepodařilo najít.", h)
		}
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("Server %s není veřejný poštovní server.", h)
			}
		}
	}
	return nil
}

// boxOwned: admins manage every mailbox, a user only the one they connected.
func boxOwned(a map[string]any, cfg *mailCfg) bool {
	return a["role"] == "admin" || (cfg.Owner != "" && cfg.Owner == a["id"])
}

func boxFromBody(e *core.RequestEvent, a map[string]any) (string, *mailCfg, error) {
	var b struct {
		ID string `json:"id"`
		mailCfg
	}
	if err := e.BindBody(&b); err != nil {
		return "", nil, e.BadRequestError("", err)
	}
	cfg := b.mailCfg
	cfg.Email, cfg.User = strings.TrimSpace(strings.ToLower(cfg.Email)), strings.TrimSpace(cfg.User)
	cfg.ImapHost, cfg.SmtpHost = strings.TrimSpace(cfg.ImapHost), strings.TrimSpace(cfg.SmtpHost)
	if cfg.User == "" {
		cfg.User = cfg.Email
	}
	if !emailRe.MatchString(cfg.Email) || cfg.ImapHost == "" || cfg.SmtpHost == "" || cfg.ImapPort <= 0 || cfg.SmtpPort <= 0 {
		return "", nil, e.BadRequestError("Vyplňte e-mail, servery a porty.", nil)
	}
	if cfg.Security != "starttls" && cfg.Security != "none" {
		cfg.Security = "ssl"
	}
	if b.ID != "" {
		_, old, _, err := loadBox(e.App, b.ID)
		if err != nil {
			return "", nil, e.NotFoundError(err.Error(), nil)
		}
		if !boxOwned(a, old) {
			return "", nil, e.ForbiddenError("permission_denied", nil)
		}
		if cfg.Password == "" {
			cfg.Password = old.Password
		}
		cfg.Owner = old.Owner
	} else {
		cfg.Owner = ""
	}
	if a["role"] != "admin" {
		if err := publicMailHost(&cfg); err != nil {
			return "", nil, e.BadRequestError(err.Error(), nil)
		}
	}
	if a["role"] != "admin" {
		// a user's own mailbox: only they (and admins) see it
		id, _ := a["id"].(string)
		cfg.Owner, cfg.Access = id, []string{id}
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Email
	}
	return b.ID, &cfg, nil
}

func handleMailTest(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	_, cfg, err := boxFromBody(e, a)
	if err != nil {
		return err
	}
	c, err := imapDial(cfg)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	names := folderNames(c)
	c.Close()
	s, err := smtpDial(cfg)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	s.Close()
	return e.JSON(http.StatusOK, map[string]any{"ok": true, "sent": names["sent"]})
}

func handleMailBoxSave(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	id, cfg, err := boxFromBody(e, a)
	if err != nil {
		return err
	}
	var rec *core.Record
	if id != "" {
		rec, _, _, err = loadBox(e.App, id)
		if err != nil {
			return e.NotFoundError(err.Error(), nil)
		}
	} else {
		coll, err := e.App.FindCachedCollectionByNameOrId("mailboxes")
		if err != nil {
			return err
		}
		rec = core.NewRecord(coll)
		id = randHex(6)
		rec.Set("key", id)
		rec.Set("state", mailState{Folders: map[string]*mailFolderState{}})
	}
	rec.Set("cfg", cfg)
	if err := e.App.Save(rec); err != nil {
		return err
	}
	if cfg.Enabled {
		go func() {
			if err := syncBox(e.App, id); err != nil {
				e.App.Logger().Warn("mail sync", "box", id, "err", err)
			}
		}()
	}
	publishMailState(e.App, id)
	return e.JSON(http.StatusOK, map[string]any{"id": id})
}

func handleMailBoxDel(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	var b struct{ ID string }
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	l := mailLock(b.ID)
	l.Lock()
	defer l.Unlock()
	rec, cfg, _, err := loadBox(e.App, b.ID)
	if err != nil {
		return e.NotFoundError(err.Error(), nil)
	}
	if !boxOwned(a, cfg) {
		return e.ForbiddenError("permission_denied", nil)
	}
	mails, _ := e.App.FindAllRecords("mails", dbx.HashExp{"box": b.ID})
	for _, m := range mails {
		_ = e.App.Delete(m)
	}
	queued, _ := e.App.FindAllRecords("mailqueue", dbx.HashExp{"box": b.ID})
	for _, m := range queued {
		_ = e.App.Delete(m)
	}
	if err := e.App.Delete(rec); err != nil {
		return err
	}
	if d, err := findDoc(e.App, "mailstate", b.ID); err == nil {
		_ = e.App.Delete(d)
	}
	return e.NoContent(http.StatusNoContent)
}

func handleMailSync(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	var b struct{ Box string }
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	if !boxKeyAllowed(e.App, a, b.Box) {
		return e.ForbiddenError("permission_denied", nil)
	}
	if err := syncBox(e.App, b.Box); err != nil {
		return e.BadRequestError("Načtení pošty se nepovedlo: "+err.Error(), nil)
	}
	return e.NoContent(http.StatusNoContent)
}

func mailItem(r *core.Record) map[string]any {
	return map[string]any{"id": r.Id, "box": r.GetString("box"), "folder": r.GetString("folder"), "subject": r.GetString("subject"),
		"from": r.GetString("from"), "fromName": r.GetString("fromName"), "to": r.Get("to"), "cc": r.Get("cc"),
		"date": r.GetDateTime("date").Time().UnixMilli(), "snippet": r.GetString("snippet"), "seen": r.GetBool("seen"),
		"att": r.Get("att"), "size": r.GetInt("size"), "leadId": r.GetString("leadId"), "msgid": r.GetString("msgid")}
}

func handleMailList(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	q := e.Request.URL.Query()
	filter, params := []string{}, dbx.Params{}
	if b := q.Get("box"); b != "" {
		if !boxKeyAllowed(e.App, a, b) {
			return e.ForbiddenError("permission_denied", nil)
		}
		filter, params["b"] = append(filter, "box = {:b}"), b
	} else {
		boxes := allowedBoxes(e.App, a)
		if len(boxes) == 0 {
			return e.JSON(http.StatusOK, map[string]any{"items": []any{}, "more": false})
		}
		f, p := boxesFilter(boxes)
		filter = append(filter, f)
		for k, v := range p {
			params[k] = v
		}
	}
	if f := q.Get("folder"); f != "" {
		filter, params["f"] = append(filter, "folder = {:f}"), f
	}
	if a := strings.ToLower(strings.TrimSpace(q.Get("addr"))); a != "" {
		// several addresses (a client and its contacts) separated by commas
		var or []string
		for i, x := range strings.Split(a, ",") {
			if x = strings.TrimSpace(x); x != "" && i < 20 {
				k := "a" + strconv.Itoa(i)
				or, params[k] = append(or, "addrs ~ {:"+k+"}"), " "+x+" "
			}
		}
		if len(or) > 0 {
			filter = append(filter, "("+strings.Join(or, " || ")+")")
		}
	}
	if s := strings.TrimSpace(q.Get("q")); s != "" {
		filter, params["q"] = append(filter, "(subject ~ {:q} || fromName ~ {:q} || addrs ~ {:q} || snippet ~ {:q})"), s
	}
	if q.Get("unread") == "1" {
		filter = append(filter, "seen = false")
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if filter == nil {
		filter = []string{"id != ''"}
	}
	recs, err := e.App.FindRecordsByFilter("mails", strings.Join(filter, " && "), "-date", mailPageSize+1, page*mailPageSize, params)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for i, r := range recs {
		if i < mailPageSize {
			out = append(out, mailItem(r))
		}
	}
	return e.JSON(http.StatusOK, map[string]any{"items": out, "more": len(recs) > mailPageSize})
}

func readRaw(app core.App, r *core.Record) ([]byte, error) {
	if r.GetString("raw") == "" {
		return nil, nil
	}
	fsys, err := app.NewFilesystem()
	if err != nil {
		return nil, err
	}
	defer fsys.Close()
	br, err := fsys.GetReader(r.BaseFilesPath() + "/" + r.GetString("raw"))
	if err != nil {
		return nil, err
	}
	defer br.Close()
	return io.ReadAll(br)
}

func handleMailMsg(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	r, err := e.App.FindRecordById("mails", e.Request.URL.Query().Get("id"))
	if err != nil || !boxKeyAllowed(e.App, a, r.GetString("box")) {
		return e.NotFoundError("", nil)
	}
	raw, err := readRaw(e.App, r)
	if err != nil {
		return err
	}
	p := parseMail(raw)
	body := p.HTML
	// inline images (cid:) become data: URLs, the page shows the mail in a sandbox
	var atts []map[string]any
	for i, a := range p.Atts {
		if a.CID != "" && strings.Contains(body, "cid:"+a.CID) {
			if len(a.data) < 5<<20 {
				body = strings.ReplaceAll(body, "cid:"+a.CID, "data:"+a.Type+";base64,"+base64.StdEncoding.EncodeToString(a.data))
			}
			continue
		}
		atts = append(atts, map[string]any{"i": i, "name": a.Name, "type": a.Type, "size": a.Size})
	}
	out := mailItem(r)
	out["text"], out["html"], out["atts"] = p.Text, body, atts
	out["inReplyTo"], out["references"] = p.InReplyTo, p.References
	if raw == nil {
		out["text"] = "(Zpráva je příliš velká, v CRM je jen její hlavička. Otevřete ji ve webmailu.)"
	}
	if !r.GetBool("seen") && e.Request.URL.Query().Get("peek") != "1" {
		setSeen(e.App, r, true)
		out["seen"] = true
	}
	return e.JSON(http.StatusOK, out)
}

// handleMailAtt downloads an attachment; links carry the session cookie like /_blob.
func handleMailAtt(e *core.RequestEvent) error {
	c, err := e.Request.Cookie(cookieName)
	if err != nil {
		return e.UnauthorizedError("", nil)
	}
	u, err := e.App.FindAuthRecordByToken(c.Value, core.TokenTypeAuth)
	if err != nil || u.Collection().Name != "users" || !u.GetBool("active") {
		return e.UnauthorizedError("", nil)
	}
	a, _ := loadAccount(e.App, u.GetString("acc"))
	if !isActive(a) || mailPerm(a) == "none" {
		return e.ForbiddenError("", nil)
	}
	r, err := e.App.FindRecordById("mails", e.Request.URL.Query().Get("id"))
	if err != nil || !boxKeyAllowed(e.App, a, r.GetString("box")) {
		return e.NotFoundError("", nil)
	}
	raw, err := readRaw(e.App, r)
	if err != nil {
		return err
	}
	i, _ := strconv.Atoi(e.Request.URL.Query().Get("i"))
	p := parseMail(raw)
	if i < 0 || i >= len(p.Atts) {
		return e.NotFoundError("", nil)
	}
	att := p.Atts[i]
	ct := att.Type
	if ct == "" {
		ct = "application/octet-stream"
	}
	disp := "attachment"
	if ct == "application/pdf" || strings.HasPrefix(ct, "image/") && ct != "image/svg+xml" {
		disp = "inline"
	}
	e.Response.Header().Set("Content-Type", ct)
	e.Response.Header().Set("Content-Disposition", disp+"; filename*=UTF-8''"+urlPathEscape(att.Name))
	e.Response.Header().Set("Content-Security-Policy", "sandbox; frame-ancestors 'self'")
	if ct == "application/pdf" {
		e.Response.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
	}
	e.Response.Header().Set("Cache-Control", "private, max-age=3600")
	_, err = e.Response.Write(att.data)
	return err
}

func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// setSeen stores the flag locally at once and on the IMAP server in the background.
func setSeen(app core.App, r *core.Record, seen bool) {
	r.Set("seen", seen)
	_ = app.Save(r)
	box, folder, uid := r.GetString("box"), r.GetString("folder"), r.GetInt("uid")
	publishMailState(app, box)
	if uid <= 0 {
		return
	}
	go func() {
		_, cfg, st, err := loadBox(app, box)
		if err != nil || st.Folders[folder] == nil {
			return
		}
		c, err := imapDial(cfg)
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := c.Select(st.Folders[folder].Name, nil).Wait(); err != nil {
			return
		}
		op := imap.StoreFlagsAdd
		if !seen {
			op = imap.StoreFlagsDel
		}
		_ = c.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close()
		_ = c.Logout().Wait()
	}()
}

func handleMailSeen(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	var b struct {
		ID   string `json:"id"`
		Seen bool   `json:"seen"`
	}
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	r, err := e.App.FindRecordById("mails", b.ID)
	if err != nil || !boxKeyAllowed(e.App, a, r.GetString("box")) {
		return e.NotFoundError("", nil)
	}
	setSeen(e.App, r, b.Seen)
	return e.NoContent(http.StatusNoContent)
}

// ---------- sending ----------

func parseAddrs(s string) ([]*mail.Address, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ";", ","))
	if s == "" {
		return nil, nil
	}
	l, err := mail.ParseAddressList(s)
	if err != nil {
		return nil, fmt.Errorf("Neplatná adresa: %s", s)
	}
	return l, nil
}

type outAtt struct {
	name, ctype, cid string
	data             []byte
}

func blobAtt(app core.App, key string) (*outAtt, error) {
	rec, err := app.FindFirstRecordByData("blobs", "key", key)
	if err != nil {
		return nil, errors.New("Příloha nenalezena.")
	}
	fsys, err := app.NewFilesystem()
	if err != nil {
		return nil, err
	}
	defer fsys.Close()
	br, err := fsys.GetReader(rec.BaseFilesPath() + "/" + rec.GetString("file"))
	if err != nil {
		return nil, err
	}
	defer br.Close()
	b, err := io.ReadAll(br)
	if err != nil {
		return nil, err
	}
	name := rec.GetString("name")
	if name == "" {
		name = "příloha"
	}
	ct := rec.GetString("type")
	if ct == "" {
		ct = mime.TypeByExtension(name[strings.LastIndex(name, ".")+1:])
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return &outAtt{name: name, ctype: ct, data: b}, nil
}

// buildMail writes mixed{ related{ alternative{text, html}, inline images }, attachments }.
func buildMail(cfg *mailCfg, to, cc []*mail.Address, subject, text, htmlBody, inReplyTo string, refs []string, inline, atts []*outAtt) ([]byte, string, error) {
	var h mail.Header
	h.SetDate(time.Now())
	h.SetAddressList("From", []*mail.Address{{Name: cfg.FromName, Address: cfg.Email}})
	h.SetAddressList("To", to)
	if len(cc) > 0 {
		h.SetAddressList("Cc", cc)
	}
	h.SetSubject(subject)
	host := cfg.Email[strings.LastIndex(cfg.Email, "@")+1:]
	if err := h.GenerateMessageIDWithHostname(host); err != nil {
		return nil, "", err
	}
	id, _ := h.MessageID()
	if inReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{inReplyTo})
		h.SetMsgIDList("References", append(refs, inReplyTo))
	}
	h.Set("X-Mailer", "Prima Shade CRM")
	h.Set("MIME-Version", "1.0")
	h.SetContentType("multipart/mixed", nil)
	var buf bytes.Buffer
	mw, err := gomsg.CreateWriter(&buf, h.Header)
	if err != nil {
		return nil, "", err
	}
	part := func(parent *gomsg.Writer, ctype string, params map[string]string, set func(*gomsg.Header)) (*gomsg.Writer, error) {
		var ph gomsg.Header
		ph.SetContentType(ctype, params)
		if set != nil {
			set(&ph)
		}
		return parent.CreatePart(ph)
	}
	rel, err := part(mw, "multipart/related", nil, nil)
	if err != nil {
		return nil, "", err
	}
	alt, err := part(rel, "multipart/alternative", nil, nil)
	if err != nil {
		return nil, "", err
	}
	for _, x := range [][2]string{{"text/plain", text}, {"text/html", htmlBody}} {
		w, err := part(alt, x[0], map[string]string{"charset": "utf-8"}, func(ph *gomsg.Header) { ph.Set("Content-Transfer-Encoding", "quoted-printable") })
		if err != nil {
			return nil, "", err
		}
		_, _ = io.WriteString(w, x[1])
		w.Close()
	}
	alt.Close()
	for _, a := range inline {
		w, err := part(rel, a.ctype, nil, func(ph *gomsg.Header) {
			ph.Set("Content-Transfer-Encoding", "base64")
			ph.Set("Content-ID", "<"+a.cid+">")
			ph.SetContentDisposition("inline", map[string]string{"filename": a.name})
		})
		if err != nil {
			return nil, "", err
		}
		_, _ = w.Write(a.data)
		w.Close()
	}
	rel.Close()
	for _, a := range atts {
		w, err := part(mw, a.ctype, map[string]string{"name": a.name}, func(ph *gomsg.Header) {
			ph.Set("Content-Transfer-Encoding", "base64")
			ph.SetContentDisposition("attachment", map[string]string{"filename": a.name})
		})
		if err != nil {
			return nil, "", err
		}
		_, _ = w.Write(a.data)
		w.Close()
	}
	mw.Close()
	return buf.Bytes(), id, nil
}

// textToHTML gives the plain text a simple HTML twin so mail apps keep line breaks and links.
var linkRe = regexp.MustCompile(`https?://[^\s<>"]+`)

func textToHTML(s string) string {
	e := html.EscapeString(s)
	e = linkRe.ReplaceAllStringFunc(e, func(u string) string { return `<a href="` + u + `">` + u + `</a>` })
	var b strings.Builder
	b.WriteString(`<div style="font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.45">`)
	quote := 0
	for _, l := range strings.Split(e, "\n") {
		q := 0
		for strings.HasPrefix(l, "&gt;") {
			l, q = strings.TrimPrefix(strings.TrimPrefix(l, "&gt;"), " "), q+1
		}
		for ; quote < q; quote++ {
			b.WriteString(`<blockquote style="margin:0 0 0 6px;padding-left:10px;border-left:2px solid #ccc;color:#555">`)
		}
		for ; quote > q; quote-- {
			b.WriteString(`</blockquote>`)
		}
		b.WriteString(strings.TrimRight(l, "\r") + "<br>")
	}
	for ; quote > 0; quote-- {
		b.WriteString(`</blockquote>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// sendReq is what the compose dialog sends; a scheduled one is stored as is.
type sendReq struct {
	Box      string   `json:"box"`
	To       string   `json:"to"`
	Cc       string   `json:"cc"`
	Subject  string   `json:"subject"`
	Text     string   `json:"text"`
	HTML     string   `json:"html"`     // formatted text from the editor ("" = plain Text only)
	SigText  string   `json:"sigText"`  // signature, written under the text
	SigLogo  string   `json:"sigLogo"`  // blob key of the signature image
	SigID    string   `json:"sigId"`    // which of the author's signatures (kept for editing a scheduled e-mail)
	Quote    string   `json:"quote"`    // the answered / forwarded message, under the signature
	ReplyTo  string   `json:"replyTo"`  // id of the mail answered
	Fwd      string   `json:"fwd"`      // id of the mail forwarded (its attachments go along)
	Att      []string `json:"att"`      // uploaded blob keys
	AttNames []string `json:"attNames"` // their names, to show a scheduled e-mail
	CommID   string   `json:"commId"`   // comms record to mark as sent
	SendAt   int64    `json:"sendAt"`   // ms; later than now = scheduled
}

// userErr is shown to the user as is.
type userErr struct{ msg string }

func (u userErr) Error() string { return u.msg }

func uerr(format string, a ...any) error { return userErr{fmt.Sprintf(format, a...)} }

func handleMailSend(e *core.RequestEvent) error {
	a, err := mailAccount(e, "view")
	if err != nil {
		return err
	}
	var b sendReq
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	// sending needs Pošta "edit", except from a mailbox the user connected themselves
	if _, cfg, _, err := loadBox(e.App, b.Box); err != nil || !boxAllowed(a, cfg) || (mailPerm(a) != "edit" && !boxOwned(a, cfg)) {
		return e.ForbiddenError("permission_denied", nil)
	}
	if b.SendAt > time.Now().Add(30*time.Second).UnixMilli() {
		if err := checkSend(&b); err != nil {
			return e.BadRequestError(err.Error(), nil)
		}
		id, err := queueMail(e.App, a, &b)
		if err != nil {
			return err
		}
		return e.JSON(http.StatusOK, map[string]any{"queued": id})
	}
	rec, err := doSend(e.App, a, &b)
	if err != nil {
		var ue userErr
		if errors.As(err, &ue) {
			return e.BadRequestError(ue.msg, nil)
		}
		return err
	}
	return e.JSON(http.StatusOK, mailItem(rec))
}

func checkSend(b *sendReq) error {
	to, err := parseAddrs(b.To)
	if err != nil {
		return userErr{err.Error()}
	}
	if _, err := parseAddrs(b.Cc); err != nil {
		return userErr{err.Error()}
	}
	if len(to) == 0 {
		return uerr("Vyplňte příjemce.")
	}
	return nil
}

// sigHTML: the logo gets fixed width/height attributes (fitted into 200×80), which
// Outlook needs; it ignores max-width and would show the image at full size.
func sigHTML(text, cid string, logo []byte) string {
	if strings.TrimSpace(text) == "" && cid == "" {
		return ""
	}
	h := `<div style="margin-top:16px">` + textToHTML(text)
	if cid != "" {
		size := ""
		if c, _, err := image.DecodeConfig(bytes.NewReader(logo)); err == nil && c.Width > 0 && c.Height > 0 {
			w, hh := float64(c.Width), float64(c.Height)
			if f := math.Min(1, math.Min(200/w, 80/hh)); f < 1 {
				w, hh = w*f, hh*f
			}
			size = fmt.Sprintf(` width="%d" height="%d"`, int(math.Round(w)), int(math.Round(hh)))
		}
		h += `<img src="cid:` + cid + `" alt="Logo"` + size + ` style="display:block;max-width:200px;margin-top:8px;border:0">`
	}
	return h + `</div>`
}

// doSend builds the message, hands it to SMTP and keeps a copy in Sent.
func doSend(app core.App, a map[string]any, b *sendReq) (*core.Record, error) {
	_, cfg, st, err := loadBox(app, b.Box)
	if err != nil {
		return nil, userErr{err.Error()}
	}
	if err := checkSend(b); err != nil {
		return nil, err
	}
	to, _ := parseAddrs(b.To)
	cc, _ := parseAddrs(b.Cc)
	var atts, inline []*outAtt
	for _, k := range b.Att {
		x, err := blobAtt(app, k)
		if err != nil {
			return nil, userErr{err.Error()}
		}
		atts = append(atts, x)
	}
	logoCID, logoData := "", []byte(nil)
	if b.SigLogo != "" {
		if x, err := blobAtt(app, b.SigLogo); err == nil && strings.HasPrefix(x.ctype, "image/") {
			logoCID, logoData = "sig-"+randHex(6)+"@crm", x.data
			x.cid = logoCID
			inline = append(inline, x)
		}
	}
	inReplyTo, refs, leadID := "", []string(nil), ""
	if b.ReplyTo != "" {
		if r, err := app.FindRecordById("mails", b.ReplyTo); err == nil {
			inReplyTo, leadID = r.GetString("msgid"), r.GetString("leadId")
			if raw, _ := readRaw(app, r); raw != nil {
				refs = parseMail(raw).References
			}
		}
	}
	if b.Fwd != "" {
		if r, err := app.FindRecordById("mails", b.Fwd); err == nil {
			raw, _ := readRaw(app, r)
			for _, x := range parseMail(raw).Atts {
				atts = append(atts, &outAtt{name: x.Name, ctype: x.Type, data: x.data})
			}
		}
	}
	text, htm := strings.TrimRight(b.Text, " \n"), textToHTML(strings.TrimRight(b.Text, " \n"))
	if strings.TrimSpace(b.HTML) != "" {
		htm = richWrap(b.HTML)
		if strings.TrimSpace(text) == "" {
			text = richToText(b.HTML)
		}
	}
	if strings.TrimSpace(b.SigText) != "" || logoCID != "" {
		text += "\n\n-- \n" + b.SigText
		htm += sigHTML(b.SigText, logoCID, logoData)
	}
	if strings.TrimSpace(b.Quote) != "" {
		text += "\n\n" + b.Quote
		htm += `<div style="margin-top:16px">` + textToHTML(b.Quote) + `</div>`
	}
	raw, msgID, err := buildMail(cfg, to, cc, b.Subject, text, htm, inReplyTo, refs, inline, atts)
	if err != nil {
		return nil, err
	}
	s, err := smtpDial(cfg)
	if err != nil {
		return nil, userErr{err.Error()}
	}
	defer s.Close()
	if err := s.Mail(cfg.Email); err != nil {
		return nil, uerr("Server odesílatele odmítl: %v", err)
	}
	for _, x := range append(append([]*mail.Address{}, to...), cc...) {
		if err := s.Rcpt(x.Address); err != nil {
			return nil, uerr("Server odmítl adresu %s: %v", x.Address, err)
		}
	}
	w, err := s.Data()
	if err != nil {
		return nil, userErr{err.Error()}
	}
	if _, err := w.Write(raw); err != nil {
		return nil, userErr{err.Error()}
	}
	if err := w.Close(); err != nil {
		return nil, uerr("Odeslání se nepovedlo: %v", err)
	}
	_ = s.Quit()

	// the copy in Sent: stored here at once, appended on the server in the background
	coll, err := app.FindCachedCollectionByNameOrId("mails")
	if err != nil {
		return nil, err
	}
	rec := core.NewRecord(coll)
	rec.Set("box", b.Box)
	rec.Set("folder", "sent")
	rec.Set("uid", 0)
	rec.Set("seen", true)
	rec.Set("leadId", leadID)
	if err := fillMailRecord(rec, parseMail(raw), raw, int64(len(raw)), time.Now()); err != nil {
		return nil, err
	}
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	if b.ReplyTo != "" {
		if r, err := app.FindRecordById("mails", b.ReplyTo); err == nil && !r.GetBool("seen") {
			setSeen(app, r, true)
		}
	}
	if b.CommID != "" {
		if d, err := findDoc(app, "comms", b.CommID); err == nil {
			cm := docData(d)
			cm["status"], cm["sentAt"], cm["mailId"] = "odesláno", time.Now().UnixMilli(), rec.Id
			_ = putDoc(app, "comms", b.CommID, cm)
		}
	}
	box := b.Box
	sentName := ""
	if f := st.Folders["sent"]; f != nil {
		sentName = f.Name
	}
	go func() {
		c, err := imapDial(cfg)
		if err != nil {
			return
		}
		defer c.Close()
		if sentName == "" {
			sentName = folderNames(c)["sent"]
		}
		if sentName == "" {
			return
		}
		ac := c.Append(sentName, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Time: time.Now()})
		if _, err := ac.Write(raw); err == nil {
			_ = ac.Close()
			_, _ = ac.Wait()
		}
		_ = c.Logout().Wait()
		_ = syncBox(app, box)
	}()
	publishMailState(app, box)
	app.Logger().Info("mail sent", "box", box, "by", a["name"], "to", b.To, "msgid", msgID)
	return rec, nil
}

// handleMailToLead creates a lead from an incoming e-mail.
func handleMailToLead(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	if mailPerm(a) == "none" || !canWrite(a, "leads", "", nil, e.App) {
		return e.ForbiddenError("permission_denied", nil)
	}
	var b struct{ ID string }
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	r, err := e.App.FindRecordById("mails", b.ID)
	if err != nil || !boxKeyAllowed(e.App, a, r.GetString("box")) {
		return e.NotFoundError("", nil)
	}
	if id := r.GetString("leadId"); id != "" {
		if _, err := findDoc(e.App, "leads", id); err == nil {
			return e.JSON(http.StatusOK, map[string]any{"id": id, "existing": true})
		}
	}
	raw, _ := readRaw(e.App, r)
	p := parseMail(raw)
	text := strings.TrimSpace(p.Text)
	if text == "" {
		text = htmlToText(p.HTML)
	}
	name := p.FromName
	if name == "" {
		name = p.From
	}
	by, _ := a["name"].(string)
	id, code, err := createLead(e.App, map[string]any{
		"name": clip(name, 120), "email": p.From, "subject": clip(p.Subject, 200), "source": "E-mail", "owner": by,
		"phone": findPhone(text), "note": clip(text, 4000),
	}, by, "Lead založen z e-mailu „"+clip(p.Subject, 120)+"“", "")
	if err != nil {
		return err
	}
	r.Set("leadId", id)
	_ = e.App.Save(r)
	return e.JSON(http.StatusOK, map[string]any{"id": id, "code": code})
}

var phoneRe = regexp.MustCompile(`(?:\+420[ \t]?)?\b\d{3}[ \t]?\d{3}[ \t]?\d{3}\b`)

func findPhone(s string) string {
	return phoneRe.FindString(s)
}
