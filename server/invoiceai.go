package main

// Supplier invoice → stock: "📄 Naskladnit z faktury" in Sklad uploads a supplier's
// invoice (PDF or photo), Claude reads its lines, and the CRM shows them for a check
// before anything is put on stock. The invoice file is kept as a blob and linked from
// each stock movement.
//
// The Anthropic API key is entered by the admin in Nastavení → Umělá inteligence and
// stays on the server (collection "secrets", key "anthropic"); ANTHROPIC_API_KEY in the
// environment works too.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

const aiModel = "claude-opus-5-5"

type aiCfg struct {
	Key string `json:"key,omitempty"`
}

func loadAICfg(app core.App) (*aiCfg, *core.Record) {
	cfg := &aiCfg{}
	r, err := app.FindFirstRecordByData("secrets", "key", "anthropic")
	if err == nil {
		raw, _ := json.Marshal(r.Get("data"))
		_ = json.Unmarshal(raw, cfg)
	}
	if cfg.Key == "" {
		cfg.Key = os.Getenv("ANTHROPIC_API_KEY")
	}
	return cfg, r
}

func aiClient(key string) anthropic.Client {
	opts := []option.RequestOption{option.WithAPIKey(key), option.WithRequestTimeout(4 * time.Minute)}
	if u := os.Getenv("STINKO_ANTHROPIC_URL"); u != "" { // tests: fake API
		opts = append(opts, option.WithBaseURL(u))
	}
	return anthropic.NewClient(opts...)
}

func bindAIRoutes(se *core.ServeEvent) {
	r := se.Router
	r.GET("/api/stinko/ai/cfg", handleAICfg)
	r.POST("/api/stinko/ai/cfg", handleAICfgSave)
	r.POST("/api/stinko/stock/invoice", handleStockInvoice).Bind(apis.BodyLimit(30 << 20))
}

func handleAICfg(e *core.RequestEvent) error {
	if _, err := authAccount(e); err != nil {
		return err
	}
	c, _ := loadAICfg(e.App)
	return e.JSON(http.StatusOK, map[string]any{"ready": c.Key != ""})
}

func handleAICfgSave(e *core.RequestEvent) error {
	if _, err := adminAccount(e); err != nil {
		return err
	}
	var b aiCfg
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	b.Key = strings.TrimSpace(b.Key)
	_, rec := loadAICfg(e.App)
	if b.Key == "" {
		return e.BadRequestError("Vložte API klíč.", nil)
	}
	// a tiny request proves the key works before it is saved
	ctx, cancel := context.WithTimeout(e.Request.Context(), time.Minute)
	defer cancel()
	cl := aiClient(b.Key)
	if _, err := cl.Messages.New(ctx, anthropic.MessageNewParams{
		Model: aiModel, MaxTokens: 1000,
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow},
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Odpověz jedním slovem: OK"))},
	}); err != nil {
		return e.BadRequestError(aiErrText(err), nil)
	}
	if rec == nil {
		coll, err := e.App.FindCachedCollectionByNameOrId("secrets")
		if err != nil {
			return err
		}
		rec = core.NewRecord(coll)
		rec.Set("key", "anthropic")
	}
	rec.Set("data", b)
	if err := e.App.Save(rec); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func aiErrText(err error) string {
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case 401, 403:
			return "Claude odmítl API klíč – zkontrolujte ho v Nastavení → Umělá inteligence."
		case 429:
			return "Claude je teď přetížený nebo došel kredit na účtu – zkuste to za chvíli."
		case 413:
			return "Soubor je pro AI příliš velký."
		}
		if ae.StatusCode >= 500 {
			return "Služba Claude je dočasně nedostupná – zkuste to za chvíli."
		}
		return "Claude vrátil chybu " + http.StatusText(ae.StatusCode) + "."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Čtení faktury trvalo příliš dlouho – zkuste to znovu."
	}
	return "Spojení s Claude se nepovedlo."
}

func canEditStock(a map[string]any) bool {
	if a["role"] == "admin" {
		return true
	}
	perms, _ := a["perms"].(map[string]any)
	return perms["stock"] == "edit"
}

type invLine struct {
	Name      string  `json:"name"`
	Code      string  `json:"code"`
	Qty       float64 `json:"qty"`
	Unit      string  `json:"unit"`
	UnitPrice float64 `json:"unitPrice"` // Kč per unit without VAT, after line discounts
	Total     float64 `json:"total"`     // Kč without VAT for the whole line
	Vat       float64 `json:"vat"`
	Kind      string  `json:"kind"` // goods | shipping | other
}

type invRead struct {
	Supplier string    `json:"supplier"`
	Number   string    `json:"number"`
	Date     string    `json:"date"`
	Currency string    `json:"currency"`
	Lines    []invLine `json:"lines"`
	Note     string    `json:"note"`
}

var invSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"supplier", "number", "date", "currency", "lines", "note"},
	"properties": map[string]any{
		"supplier": map[string]any{"type": "string"},
		"number":   map[string]any{"type": "string"},
		"date":     map[string]any{"type": "string", "description": "YYYY-MM-DD, empty if unknown"},
		"currency": map[string]any{"type": "string", "description": "ISO code, e.g. CZK, EUR"},
		"note":     map[string]any{"type": "string", "description": "anything the person checking the lines should know, in Czech; empty if nothing"},
		"lines": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"name", "code", "qty", "unit", "unitPrice", "total", "vat", "kind"},
			"properties": map[string]any{
				"name":      map[string]any{"type": "string"},
				"code":      map[string]any{"type": "string", "description": "supplier's item code / SKU, empty if none"},
				"qty":       map[string]any{"type": "number"},
				"unit":      map[string]any{"type": "string", "description": "ks, m, sada, …"},
				"unitPrice": map[string]any{"type": "number", "description": "price per unit WITHOUT VAT after any line discount"},
				"total":     map[string]any{"type": "number", "description": "line total WITHOUT VAT"},
				"vat":       map[string]any{"type": "number", "description": "VAT rate in percent, 0 if none"},
				"kind":      map[string]any{"type": "string", "enum": []string{"goods", "shipping", "other"}},
			},
		}},
	},
}

const invPrompt = `Toto je faktura od dodavatele zboží pro firmu, která prodává a montuje pergoly. Přečti z ní všechny položky.

Pravidla:
- Ceny vždy uváděj BEZ DPH. Pokud faktura uvádí jen ceny s DPH, přepočítej je podle sazby DPH dané položky.
- unitPrice je cena za jednu měrnou jednotku po odečtení slevy na řádku; total je cena celého řádku bez DPH.
- Dopravné, poštovné, balné a podobné poplatky označ kind "shipping". Zálohy, zaokrouhlení a odpočty záloh vynech. Ostatní nezbožové položky (služby) označ "other".
- Čísla piš jako čísla (desetinná tečka), bez měny a mezer.
- Měna je měna faktury; nepřepočítávej ji.
- Do note napiš česky, pokud je něco nečitelné nebo nejisté (např. "Řádek 3 je rozmazaný, množství odhadnuto"). Jinak prázdný řetězec.`

func handleStockInvoice(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	if !canEditStock(a) {
		return e.ForbiddenError("permission_denied", nil)
	}
	c, _ := loadAICfg(e.App)
	if c.Key == "" {
		return e.BadRequestError("Čtení faktur není zapnuté – admin vloží API klíč v Nastavení → Umělá inteligence.", nil)
	}
	f, fh, err := e.Request.FormFile("file")
	if err != nil {
		return e.BadRequestError("Chybí soubor.", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 30<<20))
	if err != nil {
		return err
	}
	ctype := http.DetectContentType(data)
	var block anthropic.ContentBlockParamUnion
	b64 := base64.StdEncoding.EncodeToString(data)
	switch {
	case ctype == "application/pdf":
		block = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	case ctype == "image/jpeg" || ctype == "image/png" || ctype == "image/gif" || ctype == "image/webp":
		block = anthropic.NewImageBlockBase64(ctype, b64)
	default:
		return e.BadRequestError("Nahrajte fakturu jako PDF nebo fotku (JPG, PNG).", nil)
	}
	ctx, cancel := context.WithTimeout(e.Request.Context(), 4*time.Minute)
	defer cancel()
	cl := aiClient(c.Key)
	msg, err := cl.Messages.New(ctx, anthropic.MessageNewParams{
		Model: aiModel, MaxTokens: 16000,
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortMedium,
			Format: anthropic.JSONOutputFormatParam{Schema: invSchema},
		},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(block, anthropic.NewTextBlock(invPrompt))},
	})
	if err != nil {
		e.App.Logger().Warn("invoice ai", "err", err)
		return e.BadRequestError(aiErrText(err), nil)
	}
	if msg.StopReason == anthropic.StopReasonRefusal {
		return e.BadRequestError("Claude tento soubor odmítl zpracovat.", nil)
	}
	if msg.StopReason == anthropic.StopReasonMaxTokens {
		return e.BadRequestError("Faktura má na jedno čtení příliš mnoho položek.", nil)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	var out invRead
	if err := json.Unmarshal([]byte(text.String()), &out); err != nil {
		return e.BadRequestError("Z faktury se nepodařilo přečíst položky.", err)
	}
	name := fh.Filename
	if name == "" {
		name = "faktura"
	}
	fileID, err := storeBlob(e.App, name, ctype, data)
	if err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"invoice": out, "fileId": fileID, "fileName": name})
}
