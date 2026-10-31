// Prima Shade CRM server: PocketBase with a small document store that mirrors the
// window.claude db/assets API the CRM page was written against.
//
//	docs   – one row per CRM document (col + key + JSON data), realtime enabled
//	blobs  – uploaded files, served at /_blob/{key} and /offer/{name}
//	users  – PocketBase auth records, one per CRM account (field acc)
//
// CRM accounts (role, perms, PBKDF2 salt/hash) stay in docs col "accounts";
// this server verifies passwords against them and enforces write permissions.
package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/types"
)

const (
	cookieName = "stk"
	tokenTTL   = 14 * 24 * 3600 // seconds
	maxUpload  = 200 << 20
)

// writeSections lists, per collection, the CRM sections whose "edit" right
// allows writing it. Broad on purpose: automations touch neighbouring records
// (a deal phase change moves stock, an accepted offer updates the deal).
var writeSections = map[string][]string{
	"clients":    {"clients", "deals", "leads", "offers", "activities", "calendar"},
	"leads":      {"leads", "clients", "activities", "calendar"},
	"deals":      {"deals", "offers", "installs", "activities", "calendar", "stock", "documents"},
	"activities": {"activities", "calendar", "deals", "leads", "clients", "installs"},
	"products":   {"pricelist", "stock", "deals", "installs", "offers"},
	"offers":     {"offers", "deals"},
	"installs":   {"installs", "deals", "calendar"},
	"comms":      {"deals", "clients", "leads", "activities", "offers"},
	"acc_out":    {"accounting"},
	"acc_in":     {"accounting"},
	"acc_bank":   {"accounting"},
	"acc_cfg":    {"accounting"},
}

func main() {
	app := pocketbase.New()

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureSchema(e.App); err != nil {
			return err
		}
		if err := ensureMailSchema(e.App); err != nil {
			return err
		}
		if err := ensureSecretsSchema(e.App); err != nil {
			return err
		}
		return ensureOfferSchema(e.App)
	})

	// Other accounts' password hashes are hidden from non-admins everywhere
	// (list responses and realtime messages both pass through enrich).
	app.OnRecordEnrich("docs").BindFunc(func(e *core.RecordEnrichEvent) error {
		if col := e.Record.GetString("col"); isAccCol(col) {
			// Účetnictví: only admins and accounts with the accounting right
			var a map[string]any
			if e.RequestInfo != nil && e.RequestInfo.Auth != nil {
				a, _ = loadAccount(e.App, e.RequestInfo.Auth.GetString("acc"))
			}
			if a == nil || a["active"] == false || accPerm(a) == "none" {
				e.Record.Set("data", map[string]any{"_hidden": true})
			}
			return e.Next()
		}
		if e.RequestInfo != nil && e.RequestInfo.Auth != nil {
			col := e.Record.GetString("col")
			if sc := scopeFor(e.App, e.RequestInfo.Auth.GetString("acc")); sc.own && !sc.visible(col, e.Record.GetString("key"), docData(e.Record)) {
				e.Record.Set("data", map[string]any{"_hidden": true})
				return e.Next()
			}
		}
		// who is online is shown to admins only
		if e.Record.GetString("col") == "presence" {
			var authAcc string
			if e.RequestInfo != nil && e.RequestInfo.Auth != nil {
				authAcc = e.RequestInfo.Auth.GetString("acc")
			}
			if a, _ := loadAccount(e.App, authAcc); a == nil || a["role"] != "admin" {
				e.Record.Set("data", map[string]any{})
			}
		}
		if e.Record.GetString("col") == "accounts" {
			var authAcc string
			if e.RequestInfo != nil && e.RequestInfo.Auth != nil {
				authAcc = e.RequestInfo.Auth.GetString("acc")
			}
			if authAcc != e.Record.GetString("key") {
				if a, _ := loadAccount(e.App, authAcc); a == nil || a["role"] != "admin" {
					d := docData(e.Record)
					delete(d, "salt")
					delete(d, "hash")
					e.Record.Set("data", d)
				}
			}
		}
		return e.Next()
	})

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		r := se.Router
		r.GET("/api/stinko/status", func(e *core.RequestEvent) error {
			n, _ := e.App.CountRecords("docs", dbx.HashExp{"col": "accounts"})
			return e.JSON(http.StatusOK, map[string]any{"hasAccounts": n > 0})
		})
		r.POST("/api/stinko/login", handleLogin)
		r.POST("/api/stinko/setup", handleSetup)
		r.GET("/api/stinko/me", handleMe)
		r.POST("/api/stinko/logout", func(e *core.RequestEvent) error {
			setCookie(e, "", -1)
			return e.NoContent(http.StatusNoContent)
		})
		r.POST("/api/stinko/set", handleSet)
		r.POST("/api/stinko/del", handleDel)
		r.GET("/api/stinko/leadform", handleLeadForm)
		r.POST("/api/stinko/lead", handleWebLead)
		r.GET("/poptavka", func(e *core.RequestEvent) error {
			// the form is embedded in the company website (Profistránka)
			e.Response.Header().Del("X-Frame-Options")
			e.Response.Header().Set("Content-Security-Policy", "frame-ancestors 'self' https://*.profistranka.cz https://primashade.cz https://*.primashade.cz")
			e.Response.Header().Set("Cache-Control", "no-cache")
			return e.FileFS(os.DirFS(publicDir()), "poptavka.html")
		})
		r.POST("/api/stinko/import", handleImport).Bind(apis.BodyLimit(maxUpload))
		r.POST("/api/stinko/upload", handleUpload).Bind(apis.BodyLimit(maxUpload))
		r.GET("/_blob/{key}", func(e *core.RequestEvent) error { return serveBlob(e, e.Request.PathValue("key")) })
		r.GET("/offer/{name}", func(e *core.RequestEvent) error {
			// offer templates ship with the page; an imported copy is used only if present
			if _, err := os.Stat(publicDir() + "/offer/" + e.Request.PathValue("name")); err == nil {
				return e.FileFS(os.DirFS(publicDir()), "offer/"+e.Request.PathValue("name"))
			}
			return serveBlob(e, "offer/"+e.Request.PathValue("name"))
		})
		bindMailRoutes(se)
		bindOfferRoutes(se)
		bindFakRoutes(se)
		bindAccRoutes(se)
		startFakLoop(se.App)
		startMailLoop(se.App)
		r.GET("/{path...}", apis.Static(os.DirFS(publicDir()), true)).Bind(noCacheHTML())
		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func publicDir() string {
	if d := os.Getenv("STINKO_PUBLIC"); d != "" {
		return d
	}
	return "pb_public"
}

// noCacheHTML makes browsers revalidate the page itself so a new CRM version
// shows up on the next reload.
func noCacheHTML() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{Func: func(e *core.RequestEvent) error {
		p := e.Request.URL.Path
		if p == "/" || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js") {
			e.Response.Header().Set("Cache-Control", "no-cache")
		}
		return e.Next()
	}}
}

// ---------- schema ----------

func ensureSchema(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("docs"); err != nil {
		c := core.NewBaseCollection("docs")
		c.Fields.Add(
			&core.TextField{Name: "col", Required: true, Max: 300},
			&core.TextField{Name: "key", Required: true, Max: 300},
			&core.JSONField{Name: "data", MaxSize: 20 << 20},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_docs_col_key", true, "`col`,`key`", "")
		c.ListRule = types.Pointer("@request.auth.active = true")
		c.ViewRule = types.Pointer("@request.auth.active = true")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCollectionByNameOrId("blobs"); err != nil {
		c := core.NewBaseCollection("blobs")
		c.Fields.Add(
			&core.TextField{Name: "key", Required: true, Max: 300},
			&core.TextField{Name: "name", Max: 500},
			&core.TextField{Name: "type", Max: 200},
			&core.FileField{Name: "file", MaxSelect: 1, MaxSize: maxUpload, Protected: true},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_blobs_key", true, "`key`", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}
	if users.Fields.GetByName("acc") == nil {
		users.Fields.Add(&core.TextField{Name: "acc", Max: 300}, &core.BoolField{Name: "active"})
		users.AuthToken.Duration = tokenTTL
		// accounts are created only by this server, never through the public API
		users.CreateRule = nil
		users.UpdateRule = nil
		users.DeleteRule = nil
		users.PasswordAuth.Enabled = false
		users.AddIndex("idx_users_acc", true, "`acc`", "`acc` != ''")
		if err := app.Save(users); err != nil {
			return err
		}
	}
	s := app.Settings()
	changed := !s.RateLimits.Enabled
	s.RateLimits.Enabled = true
	// added one by one so a later server version can add rules to an existing install
	for _, want := range []core.RateLimitRule{
		{Label: "/api/stinko/login", MaxRequests: 10, Duration: 60},
		{Label: "/api/stinko/setup", MaxRequests: 5, Duration: 60},
		{Label: "/api/stinko/lead", MaxRequests: 5, Duration: 300},
	} {
		found := false
		for _, r := range s.RateLimits.Rules {
			found = found || r.Label == want.Label
		}
		if !found {
			s.RateLimits.Rules = append(s.RateLimits.Rules, want)
			changed = true
		}
	}
	if s.Meta.AppName == "Acme" || s.Meta.AppName == "" {
		s.Meta.AppName = "Prima Shade CRM"
		changed = true
	}
	if len(s.TrustedProxy.Headers) == 0 {
		// Caddy in front sets X-Forwarded-For; the rightmost entry is the client
		s.TrustedProxy.Headers = []string{"X-Forwarded-For"}
		changed = true
	}
	if changed {
		if err := app.Save(s); err != nil {
			return err
		}
	}
	return nil
}

// ---------- docs helpers ----------

func findDoc(app core.App, col, key string) (*core.Record, error) {
	return app.FindFirstRecordByFilter("docs", "col = {:c} && key = {:k}", dbx.Params{"c": col, "k": key})
}

func docData(r *core.Record) map[string]any {
	m := map[string]any{}
	if raw, ok := r.Get("data").(types.JSONRaw); ok && len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	} else if v, ok := r.Get("data").(map[string]any); ok {
		for k, x := range v {
			m[k] = x
		}
	}
	return m
}

func putDoc(app core.App, col, key string, data map[string]any) error {
	rec, err := findDoc(app, col, key)
	if err != nil {
		coll, err := app.FindCachedCollectionByNameOrId("docs")
		if err != nil {
			return err
		}
		rec = core.NewRecord(coll)
		rec.Set("col", col)
		rec.Set("key", key)
	}
	rec.Set("data", data)
	clearScopeCache()
	if err := app.Save(rec); err != nil {
		return err
	}
	clearScopeCache()
	if col == "accounts" {
		return syncUser(app, key, data)
	}
	return nil
}

func loadAccount(app core.App, id string) (map[string]any, error) {
	if id == "" {
		return nil, errors.New("no account")
	}
	rec, err := findDoc(app, "accounts", id)
	if err != nil {
		return nil, err
	}
	d := docData(rec)
	d["id"] = id
	return d, nil
}

func isActive(a map[string]any) bool { return a != nil && a["active"] != false }

// syncUser keeps the PocketBase auth record of an account in step with it.
func syncUser(app core.App, accID string, a map[string]any) error {
	u, err := app.FindFirstRecordByData("users", "acc", accID)
	if err != nil {
		coll, err := app.FindCachedCollectionByNameOrId("users")
		if err != nil {
			return err
		}
		u = core.NewRecord(coll)
		u.Set("acc", accID)
		u.SetEmail(randHex(8) + "@stinko.invalid")
		u.SetPassword(randHex(24))
		u.SetVerified(true)
	}
	name, _ := a["name"].(string)
	u.Set("name", name)
	u.Set("active", a["active"] != false)
	return app.Save(u)
}

func publicAccount(a map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		if k != "salt" && k != "hash" {
			out[k] = v
		}
	}
	return out
}

// ---------- auth ----------

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// verifyPw matches the CRM's hashPw: PBKDF2-SHA256, 120000 rounds, 32 bytes, hex.
func verifyPw(pw string, a map[string]any) bool {
	salt, _ := a["salt"].(string)
	hash, _ := a["hash"].(string)
	if pw == "" || salt == "" || hash == "" {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, pw, []byte(salt), 120000, 32)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(k)), []byte(strings.ToLower(hash))) == 1
}

func setCookie(e *core.RequestEvent, token string, maxAge int) {
	secure := e.Request.TLS != nil || e.Request.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(e.Response, &http.Cookie{Name: cookieName, Value: token, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
}

func issueToken(e *core.RequestEvent, accID string) (string, error) {
	u, err := e.App.FindFirstRecordByData("users", "acc", accID)
	if err != nil {
		return "", err
	}
	t, err := u.NewAuthToken()
	if err != nil {
		return "", err
	}
	setCookie(e, t, tokenTTL)
	return t, nil
}

func handleLogin(e *core.RequestEvent) error {
	var body struct{ Username, Password string }
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("", err)
	}
	user := strings.ToLower(strings.TrimSpace(body.Username))
	recs, err := e.App.FindAllRecords("docs", dbx.HashExp{"col": "accounts"})
	if err != nil {
		return err
	}
	for _, r := range recs {
		a := docData(r)
		if un, _ := a["username"].(string); strings.ToLower(un) != user || user == "" {
			continue
		}
		if !verifyPw(body.Password, a) {
			break
		}
		if !isActive(a) {
			return e.ForbiddenError("Účet je zablokovaný. Obraťte se na administrátora.", nil)
		}
		a["id"] = r.GetString("key")
		if err := syncUser(e.App, r.GetString("key"), a); err != nil {
			return err
		}
		t, err := issueToken(e, r.GetString("key"))
		if err != nil {
			return err
		}
		return e.JSON(http.StatusOK, map[string]any{"token": t, "account": publicAccount(a)})
	}
	time.Sleep(400 * time.Millisecond)
	return e.UnauthorizedError("Špatné jméno nebo heslo.", nil)
}

// handleSetup creates the first admin account; refused once any account exists.
func handleSetup(e *core.RequestEvent) error {
	var body struct {
		Account map[string]any `json:"account"`
	}
	if err := e.BindBody(&body); err != nil || body.Account == nil {
		return e.BadRequestError("", err)
	}
	a := body.Account
	id, _ := a["id"].(string)
	if id == "" || a["salt"] == nil || a["hash"] == nil || a["username"] == nil {
		return e.BadRequestError("Chybí údaje účtu.", nil)
	}
	err := e.App.RunInTransaction(func(tx core.App) error {
		n, _ := tx.CountRecords("docs", dbx.HashExp{"col": "accounts"})
		if n > 0 {
			return e.ForbiddenError("Admin účet už existuje.", nil)
		}
		delete(a, "id")
		a["role"], a["active"], a["perms"] = "admin", true, map[string]any{}
		return putDoc(tx, "accounts", id, a)
	})
	if err != nil {
		return err
	}
	t, err := issueToken(e, id)
	if err != nil {
		return err
	}
	a["id"] = id
	return e.JSON(http.StatusOK, map[string]any{"token": t, "account": publicAccount(a)})
}

// authAccount resolves the CRM account behind the request token.
func authAccount(e *core.RequestEvent) (map[string]any, error) {
	if e.Auth == nil || e.Auth.Collection().Name != "users" {
		return nil, e.UnauthorizedError("Nepřihlášen.", nil)
	}
	a, err := loadAccount(e.App, e.Auth.GetString("acc"))
	if err != nil || !isActive(a) {
		return nil, e.ForbiddenError("Účet je zablokovaný nebo neexistuje.", nil)
	}
	return a, nil
}

func handleMe(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		setCookie(e, "", -1)
		return err
	}
	t, err := issueToken(e, a["id"].(string))
	if err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"token": t, "account": publicAccount(a)})
}

// ---------- permissions ----------

func canWrite(a map[string]any, col, key string, data map[string]any, app core.App) bool {
	if a["role"] == "admin" {
		return true
	}
	if col == "data/users/"+a["id"].(string) {
		return true
	}
	if col == "notifs" {
		return true
	}
	if col == "presence" {
		return key == a["id"]
	}
	if col == "newsreads" {
		// "I have read this news": only one's own mark ({newsId}_{accountId})
		id, _ := a["id"].(string)
		return strings.HasSuffix(key, "_"+id) && (data == nil || data["acc"] == id)
	}
	if col == "accounts" {
		// own password and phone only: everything else must stay as is
		if key != a["id"] || data == nil {
			return false
		}
		if p, ok := data["phone"]; ok {
			if s, isStr := p.(string); !isStr || len(s) > 40 {
				return false
			}
		}
		for k, v := range data {
			if k == "salt" || k == "hash" || k == "id" || k == "phone" {
				continue
			}
			old, _ := json.Marshal(a[k])
			nw, _ := json.Marshal(v)
			if string(old) != string(nw) {
				return false
			}
		}
		for k := range a {
			if _, ok := data[k]; !ok && k != "id" && k != "phone" {
				return false
			}
		}
		return true
	}
	perms, _ := a["perms"].(map[string]any)
	for _, sec := range writeSections[col] {
		if perms[sec] == "edit" {
			return true
		}
	}
	return false
}

func handleSet(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	var body struct {
		Col  string         `json:"col"`
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	}
	if err := e.BindBody(&body); err != nil || body.Col == "" || body.ID == "" {
		return e.BadRequestError("", err)
	}
	if body.Data == nil {
		body.Data = map[string]any{}
	}
	if !canWrite(a, body.Col, body.ID, body.Data, e.App) {
		return e.ForbiddenError("permission_denied", nil)
	}
	if body.Col == "accounts" && a["role"] == "admin" {
		if err := keepOneAdmin(e.App, body.ID, body.Data); err != nil {
			return e.BadRequestError(err.Error(), nil)
		}
	}
	if err := putDoc(e.App, body.Col, body.ID, body.Data); err != nil {
		return err
	}
	return e.NoContent(http.StatusNoContent)
}

func keepOneAdmin(app core.App, id string, data map[string]any) error {
	if data["role"] == "admin" && data["active"] != false {
		return nil
	}
	recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": "accounts"})
	for _, r := range recs {
		d := docData(r)
		if r.GetString("key") != id && d["role"] == "admin" && d["active"] != false {
			return nil
		}
	}
	return errors.New("Musí zůstat aspoň jeden aktivní admin.")
}

var adminDeleteOnly = map[string]bool{"deals": true, "leads": true, "clients": true, "offers": true, "activities": true, "products": true, "installs": true, "comms": true, "acc_out": true}

func handleDel(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	var body struct{ Col, ID string }
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("", err)
	}
	if body.Col == "accounts" || !canWrite(a, body.Col, body.ID, nil, e.App) {
		return e.ForbiddenError("permission_denied", nil)
	}
	// deleting CRM records is for admins only; others may still remove their own prefs etc.
	if adminDeleteOnly[body.Col] && a["role"] != "admin" {
		return e.ForbiddenError("permission_denied", nil)
	}
	rec, err := findDoc(e.App, body.Col, body.ID)
	if err == nil {
		if err := e.App.Delete(rec); err != nil {
			return err
		}
		clearScopeCache()
	}
	return e.NoContent(http.StatusNoContent)
}

// handleImport loads documents exported from the claude.ai version (admin only).
func handleImport(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	if a["role"] != "admin" {
		return e.ForbiddenError("permission_denied", nil)
	}
	var body struct {
		Docs []struct {
			Col  string         `json:"col"`
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"docs"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("", err)
	}
	n := 0
	err = e.App.RunInTransaction(func(tx core.App) error {
		for _, d := range body.Docs {
			if d.Col == "" || d.ID == "" || d.Col == "accounts" || strings.HasPrefix(d.Col, "data/") {
				continue
			}
			if err := putDoc(tx, d.Col, d.ID, d.Data); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"imported": n})
}

// ---------- files ----------

func handleUpload(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	files, err := e.FindUploadedFiles("file")
	if err != nil || len(files) == 0 {
		return e.BadRequestError("Chybí soubor.", err)
	}
	key := randHex(16)
	if k := e.Request.FormValue("key"); k != "" {
		if a["role"] != "admin" {
			return e.ForbiddenError("permission_denied", nil)
		}
		key = k
	}
	coll, err := e.App.FindCachedCollectionByNameOrId("blobs")
	if err != nil {
		return err
	}
	rec, err := e.App.FindFirstRecordByData("blobs", "key", key)
	if err != nil {
		rec = core.NewRecord(coll)
		rec.Set("key", key)
	}
	f := files[0]
	name := e.Request.FormValue("name")
	if name == "" {
		name = f.OriginalName
	}
	ctype := e.Request.FormValue("type")
	if ctype == "" {
		if fh := e.Request.MultipartForm.File["file"]; len(fh) > 0 {
			ctype = fh[0].Header.Get("Content-Type")
		}
	}
	rec.Set("name", name)
	rec.Set("type", ctype)
	rec.Set("file", f)
	if err := e.App.Save(rec); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"id": key, "name": name, "contentType": ctype, "sizeBytes": f.Size})
}

// serveBlob serves an uploaded file to a signed-in account; the page's <img>
// tags carry the HttpOnly session cookie instead of an Authorization header.
func serveBlob(e *core.RequestEvent, key string) error {
	c, err := e.Request.Cookie(cookieName)
	if err != nil {
		return e.UnauthorizedError("", nil)
	}
	u, err := e.App.FindAuthRecordByToken(c.Value, core.TokenTypeAuth)
	if err != nil || u.Collection().Name != "users" || !u.GetBool("active") {
		return e.UnauthorizedError("", nil)
	}
	return sendBlob(e, key)
}

// sendBlob serves a stored file; the caller has checked who may have it.
func sendBlob(e *core.RequestEvent, key string) error {
	rec, err := e.App.FindFirstRecordByData("blobs", "key", key)
	if err != nil {
		return e.NotFoundError("", nil)
	}
	fsys, err := e.App.NewFilesystem()
	if err != nil {
		return err
	}
	defer fsys.Close()
	if t := rec.GetString("type"); t != "" {
		e.Response.Header().Set("Content-Type", t)
	}
	e.Response.Header().Set("Cache-Control", "private, max-age=604800")
	name := rec.GetString("name")
	if name == "" {
		name = rec.GetString("file")
	}
	// The default "sandbox" CSP makes Chrome refuse to show PDFs in its viewer;
	// a PDF only gets a framing limit. Other files keep the strict default.
	if t := rec.GetString("type"); t == "application/pdf" || strings.HasSuffix(strings.ToLower(name), ".pdf") {
		e.Response.Header().Set("Content-Type", "application/pdf")
		e.Response.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
		e.Response.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(name))
	}
	return fsys.Serve(e.Response, e.Request, rec.BaseFilesPath()+"/"+rec.GetString("file"), name)
}
