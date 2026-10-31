package main

// Čtení přijatých dokladů umělou inteligencí: fotka účtenky nebo PDF faktury
// jde k OpenAI nebo k Anthropicu (podle nastavení) a zpět přijdou vyplněné
// údaje dokladu i s položkami. Nic se samo neuloží – stránka údaje jen
// předvyplní a člověk je zkontroluje.
//
// Klíče leží jen na serveru (kolekce secrets, klíč "ai"), stejně jako heslo
// k Fakturoidu. Stránka se dozví jen to, jestli je čtení zapnuté.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/pocketbase/pocketbase/core"
)

type aiCfg struct {
	Provider        string             `json:"provider"` // openai | anthropic
	OpenaiKey       string             `json:"openaiKey,omitempty"`
	AnthropicKey    string             `json:"anthropicKey,omitempty"`
	OpenaiModel     string             `json:"openaiModel"`
	OpenaiModel2    string             `json:"openaiModel2"` // "Přečíst pečlivěji"
	AnthropicModel  string             `json:"anthropicModel"`
	AnthropicModel2 string             `json:"anthropicModel2"`
	Usage           map[string]aiUsage `json:"usage,omitempty"` // by month "2026-10"
}

type aiUsage struct {
	N   int     `json:"n"`
	USD float64 `json:"usd"`
}

const (
	defOpenai, defOpenai2       = "gpt-5-mini", "gpt-5.4-mini"
	defAnthropic, defAnthropic2 = "claude-haiku-5-5", "claude-sonnet-5-5"
	usdCzk                      = 23.0 // jen pro orientační cenu v Kč
)

// USD per 1M tokens (input, output). A model not listed here still works, it
// just has no price shown.
var aiPrices = map[string][2]float64{
	"gpt-5-mini":        {0.25, 2},
	"gpt-5.4-mini":      {0.75, 4.5},
	"gpt-5-nano":        {0.05, 0.4},
	"claude-haiku-5-5":  {0.10, 0.50},
	"claude-sonnet-5-5": {2, 10},
	"claude-opus-5-5":   {4, 20},
}

func aiCost(model string, in, out int64) float64 {
	p, ok := aiPrices[model]
	if !ok {
		return 0
	}
	return (float64(in)*p[0] + float64(out)*p[1]) / 1e6
}

var aiMu sync.Mutex // guards the usage counter in the secrets record

func loadAiCfg(app core.App) (*aiCfg, *core.Record) {
	c := &aiCfg{}
	r, err := app.FindFirstRecordByData("secrets", "key", "ai")
	if err == nil {
		raw, _ := json.Marshal(r.Get("data"))
		_ = json.Unmarshal(raw, c)
	}
	if c.OpenaiModel == "" {
		c.OpenaiModel = defOpenai
	}
	if c.OpenaiModel2 == "" {
		c.OpenaiModel2 = defOpenai2
	}
	if c.AnthropicModel == "" {
		c.AnthropicModel = defAnthropic
	}
	if c.AnthropicModel2 == "" {
		c.AnthropicModel2 = defAnthropic2
	}
	if c.Provider == "" {
		if c.OpenaiKey == "" && c.AnthropicKey != "" {
			c.Provider = "anthropic"
		} else {
			c.Provider = "openai"
		}
	}
	if err != nil {
		return c, nil
	}
	return c, r
}

func (c *aiCfg) ready() bool {
	if c.Provider == "anthropic" {
		return c.AnthropicKey != ""
	}
	return c.OpenaiKey != ""
}

func (c *aiCfg) model(careful bool) string {
	switch {
	case c.Provider == "anthropic" && careful:
		return c.AnthropicModel2
	case c.Provider == "anthropic":
		return c.AnthropicModel
	case careful:
		return c.OpenaiModel2
	}
	return c.OpenaiModel
}

func saveAiCfg(app core.App, c *aiCfg, rec *core.Record) error {
	if rec == nil {
		coll, err := app.FindCachedCollectionByNameOrId("secrets")
		if err != nil {
			return err
		}
		rec = core.NewRecord(coll)
		rec.Set("key", "ai")
	}
	rec.Set("data", c)
	return app.Save(rec)
}

func bindAiRoutes(se *core.ServeEvent) {
	r := se.Router
	r.GET("/api/stinko/acc/ai", handleAiCfg)
	r.POST("/api/stinko/acc/ai", handleAiCfgSave)
	r.POST("/api/stinko/acc/read", handleAiRead)
}

func (c *aiCfg) month() aiUsage { return c.Usage[time.Now().Format("2006-01")] }

func handleAiCfg(e *core.RequestEvent) error {
	a, err := authAccount(e)
	if err != nil {
		return err
	}
	if accPerm(a) == "none" {
		return e.ForbiddenError("permission_denied", nil)
	}
	c, _ := loadAiCfg(e.App)
	out := map[string]any{"ready": c.ready(), "provider": c.Provider}
	if a["role"] == "admin" {
		m := c.month()
		out["hasOpenai"], out["hasAnthropic"] = c.OpenaiKey != "", c.AnthropicKey != ""
		out["openaiModel"], out["openaiModel2"], out["anthropicModel"], out["anthropicModel2"] = c.OpenaiModel, c.OpenaiModel2, c.AnthropicModel, c.AnthropicModel2
		out["monthN"], out["monthCzk"] = m.N, m.USD*usdCzk
	}
	return e.JSON(http.StatusOK, out)
}

func handleAiCfgSave(e *core.RequestEvent) error {
	if _, err := adminAccount(e); err != nil {
		return err
	}
	var b aiCfg
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	aiMu.Lock()
	defer aiMu.Unlock()
	c, rec := loadAiCfg(e.App)
	if b.Provider == "openai" || b.Provider == "anthropic" {
		c.Provider = b.Provider
	}
	// an empty key field keeps the stored key; "-" removes it
	setKey := func(dst *string, v string) bool {
		v = strings.TrimSpace(v)
		switch v {
		case "":
			return false
		case "-":
			*dst = ""
			return false
		}
		*dst = v
		return true
	}
	newOpenai, newAnthropic := setKey(&c.OpenaiKey, b.OpenaiKey), setKey(&c.AnthropicKey, b.AnthropicKey)
	for dst, v := range map[*string]string{&c.OpenaiModel: b.OpenaiModel, &c.OpenaiModel2: b.OpenaiModel2, &c.AnthropicModel: b.AnthropicModel, &c.AnthropicModel2: b.AnthropicModel2} {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	// check a new key before saving it
	if newOpenai {
		if err := openaiCheck(c.OpenaiKey); err != nil {
			return e.BadRequestError(err.Error(), nil)
		}
	}
	if newAnthropic {
		if err := anthropicCheck(c.AnthropicKey); err != nil {
			return e.BadRequestError(err.Error(), nil)
		}
	}
	if err := saveAiCfg(e.App, c, rec); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"ok": true, "ready": c.ready(), "provider": c.Provider})
}

// ---------- what we ask for ----------

type aiDoc struct {
	Kind     string   `json:"kind"`
	Supplier aiParty  `json:"supplier"`
	Number   string   `json:"number"`
	VS       string   `json:"vs"`
	Issued   string   `json:"issued"`
	Duzp     string   `json:"duzp"`
	Due      string   `json:"due"`
	Currency string   `json:"currency"`
	Total    float64  `json:"total"`
	Base     float64  `json:"base"`
	Vat      float64  `json:"vat"`
	PaidHow  string   `json:"paidHow"`
	Category string   `json:"category"`
	Items    []aiItem `json:"items"`
	Note     string   `json:"note"`
}

type aiParty struct {
	Name string `json:"name"`
	Ico  string `json:"ico"`
	Dic  string `json:"dic"`
}

type aiItem struct {
	Name    string  `json:"name"`
	Qty     float64 `json:"qty"`
	Unit    string  `json:"unit"`
	Price   float64 `json:"price"`
	VatRate float64 `json:"vatRate"`
	Total   float64 `json:"total"`
}

func aiSchema() map[string]any {
	s := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	n := func(d string) map[string]any { return map[string]any{"type": "number", "description": d} }
	obj := func(props map[string]any) map[string]any {
		req := make([]string, 0, len(props))
		for k := range props {
			req = append(req, k)
		}
		return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
	}
	item := obj(map[string]any{
		"name": s("název položky tak, jak je na dokladu"), "qty": n("množství"), "unit": s("jednotka (ks, kg, l, h…), jinak prázdné"),
		"price": n("cena za jednotku včetně DPH"), "vatRate": n("sazba DPH v procentech (21, 12, 0); neznámá = 0"), "total": n("cena položky celkem včetně DPH"),
	})
	return obj(map[string]any{
		"kind":     map[string]any{"type": "string", "enum": []string{"invoice", "receipt"}, "description": "invoice = faktura, receipt = účtenka / pokladní doklad"},
		"supplier": obj(map[string]any{"name": s("obchodní jméno dodavatele (prodejce)"), "ico": s("IČO dodavatele, jen číslice"), "dic": s("DIČ dodavatele, např. CZ12345678")}),
		"number":   s("číslo dokladu"), "vs": s("variabilní symbol, jen číslice"),
		"issued": s("datum vystavení YYYY-MM-DD"), "duzp": s("datum uskutečnění zdanitelného plnění YYYY-MM-DD"), "due": s("datum splatnosti YYYY-MM-DD"),
		"currency": s("měna, třikrát písmeno, např. CZK nebo EUR"),
		"total":    n("částka k úhradě celkem včetně DPH"), "base": n("základ daně celkem (bez DPH)"), "vat": n("DPH celkem"),
		"paidHow":  map[string]any{"type": "string", "enum": []string{"bank", "cash", "card", ""}, "description": "jak bylo zaplaceno, pokud to doklad uvádí: bank = převodem, cash = hotově, card = kartou; jinak prázdné"},
		"category": s("kategorie nákladu: jedna z nabídnutých, nebo prázdné"),
		"items":    map[string]any{"type": "array", "items": item},
		"note":     s("krátká poznámka česky, jen pokud je něco nejasné nebo nečitelné; jinak prázdné"),
	})
}

func aiPrompt(cats []string) string {
	return `Jsi asistent účetní české firmy (Prima Shade s.r.o., IČO 11730421 – to je odběratel, ne dodavatel).
Z přiloženého dokladu (faktura nebo účtenka) vypiš údaje podle schématu.
Pravidla:
- Dodavatel je ten, kdo doklad vystavil a komu se platí.
- Data piš jako YYYY-MM-DD. Částky jako čísla s desetinnou tečkou, bez měny a mezer.
- Co na dokladu není nebo nejde přečíst, nech prázdné ("" nebo 0). Nic si nevymýšlej.
- Položky vypiš všechny, včetně slev (se zápornou cenou).
- Kategorie nákladu, vyber nejbližší: ` + strings.Join(cats, ", ") + `.`
}

// ---------- reading ----------

type aiInput struct {
	mime string // image/jpeg, image/png, image/webp, application/pdf
	data []byte
	name string
}

type aiResult struct {
	Doc     aiDoc   `json:"doc"`
	Model   string  `json:"model"`
	In      int64   `json:"in"`
	Out     int64   `json:"out"`
	CostCzk float64 `json:"costCzk"`
}

func handleAiRead(e *core.RequestEvent) error {
	if _, err := accEditor(e); err != nil {
		return err
	}
	var b struct {
		FileID  string `json:"fileId"`
		Image   string `json:"image"` // base64 JPEG, already made smaller by the page
		Careful bool   `json:"careful"`
	}
	if err := e.BindBody(&b); err != nil {
		return e.BadRequestError("", err)
	}
	c, _ := loadAiCfg(e.App)
	if !c.ready() {
		return e.BadRequestError("Čtení dokladů není zapnuté – vložte API klíč v Účetnictví → Nastavení.", nil)
	}
	var in aiInput
	if b.Image != "" {
		data, err := base64.StdEncoding.DecodeString(b.Image)
		if err != nil {
			return e.BadRequestError("Obrázek se nepodařilo přečíst.", nil)
		}
		in = aiInput{mime: http.DetectContentType(data), data: data, name: "doklad.jpg"}
	} else if b.FileID != "" {
		var err error
		if in, err = loadBlob(e.App, b.FileID); err != nil {
			return e.NotFoundError("Soubor dokladu nebyl nalezen.", nil)
		}
	} else {
		return e.BadRequestError("Chybí doklad.", nil)
	}
	switch {
	case in.mime == "application/pdf", in.mime == "image/jpeg", in.mime == "image/png", in.mime == "image/webp", in.mime == "image/gif":
	default:
		return e.BadRequestError("Tento typ souboru neumím přečíst – nahrajte fotku (JPG/PNG) nebo PDF.", nil)
	}
	if len(in.data) > 20<<20 {
		return e.BadRequestError("Soubor je na přečtení moc velký (víc než 20 MB).", nil)
	}
	cats := accCats
	if l, ok := accCfg(e.App)["categories"].([]any); ok && len(l) > 0 {
		cats = cats[:0:0]
		for _, x := range l {
			if s, _ := x.(string); s != "" {
				cats = append(cats, s)
			}
		}
	}
	model := c.model(b.Careful)
	ctx, cancel := context.WithTimeout(e.Request.Context(), 3*time.Minute)
	defer cancel()
	var res *aiResult
	var err error
	if c.Provider == "anthropic" {
		res, err = anthropicRead(ctx, c.AnthropicKey, model, aiPrompt(cats), in)
	} else {
		res, err = openaiRead(ctx, c.OpenaiKey, model, aiPrompt(cats), in)
	}
	if err != nil {
		e.App.Logger().Warn("ai read", "provider", c.Provider, "model", model, "err", err)
		return e.BadRequestError(err.Error(), nil)
	}
	res.Model = model
	usd := aiCost(model, res.In, res.Out)
	res.CostCzk = usd * usdCzk
	aiMu.Lock()
	if c2, rec := loadAiCfg(e.App); rec != nil {
		if c2.Usage == nil {
			c2.Usage = map[string]aiUsage{}
		}
		k := time.Now().Format("2006-01")
		u := c2.Usage[k]
		u.N++
		u.USD += usd
		c2.Usage[k] = u
		_ = saveAiCfg(e.App, c2, rec)
	}
	aiMu.Unlock()
	cleanAiDoc(&res.Doc)
	return e.JSON(http.StatusOK, res)
}

var accCats = []string{"Materiál a zboží", "Subdodávky a montáže", "Doprava a PHM", "Nájem", "Energie", "Telefon a internet", "Software a služby", "Marketing a reklama", "Kancelář", "Nářadí a vybavení", "Pojištění", "Bankovní poplatky", "Daně a poplatky", "Ostatní"}

func cleanAiDoc(d *aiDoc) {
	d.Supplier.Ico = digitsRe.ReplaceAllString(d.Supplier.Ico, "")
	d.VS = digitsRe.ReplaceAllString(d.VS, "")
	d.Supplier.Dic = strings.ToUpper(strings.ReplaceAll(d.Supplier.Dic, " ", ""))
	for _, p := range []*string{&d.Issued, &d.Duzp, &d.Due} {
		if _, err := time.Parse("2006-01-02", *p); err != nil {
			*p = ""
		}
	}
	d.Currency = strings.ToUpper(strings.TrimSpace(d.Currency))
	if d.Items == nil {
		d.Items = []aiItem{}
	}
}

func loadBlob(app core.App, key string) (aiInput, error) {
	rec, err := app.FindFirstRecordByData("blobs", "key", key)
	if err != nil {
		return aiInput{}, err
	}
	fsys, err := app.NewFilesystem()
	if err != nil {
		return aiInput{}, err
	}
	defer fsys.Close()
	r, err := fsys.GetReader(rec.BaseFilesPath() + "/" + rec.GetString("file"))
	if err != nil {
		return aiInput{}, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, 21<<20))
	if err != nil {
		return aiInput{}, err
	}
	mime := http.DetectContentType(data)
	if bytes.HasPrefix(data, []byte("%PDF")) {
		mime = "application/pdf"
	}
	return aiInput{mime: mime, data: data, name: rec.GetString("name")}, nil
}

// ---------- OpenAI (Chat Completions) ----------

var aiHTTP = &http.Client{Timeout: 3 * time.Minute}

func openaiBase() string {
	if u := os.Getenv("STINKO_OPENAI_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://api.openai.com/v1"
}

func openaiErr(status int, body []byte) error {
	var j struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &j)
	switch {
	case status == 401:
		return errors.New("OpenAI odmítl API klíč – zkontrolujte ho.")
	case status == 429 && (j.Error.Code == "insufficient_quota" || strings.Contains(j.Error.Message, "quota")):
		return errors.New("Na účtu OpenAI došel kredit – dobijte ho, nebo přepněte na Anthropic.")
	case status == 429:
		return errors.New("OpenAI je teď přetížený, zkuste to za chvíli.")
	case j.Error.Message != "":
		return fmt.Errorf("OpenAI: %s", j.Error.Message)
	}
	return fmt.Errorf("OpenAI vrátil chybu %d.", status)
}

func openaiCheck(key string) error {
	req, _ := http.NewRequest("GET", openaiBase()+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := aiHTTP.Do(req)
	if err != nil {
		return errors.New("OpenAI teď neodpovídá, zkuste to za chvíli.")
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return openaiErr(res.StatusCode, body)
	}
	return nil
}

func openaiRead(ctx context.Context, key, model, prompt string, in aiInput) (*aiResult, error) {
	b64 := base64.StdEncoding.EncodeToString(in.data)
	var part map[string]any
	if in.mime == "application/pdf" {
		name := in.name
		if !strings.HasSuffix(strings.ToLower(name), ".pdf") {
			name = "doklad.pdf"
		}
		part = map[string]any{"type": "file", "file": map[string]any{"filename": name, "file_data": "data:application/pdf;base64," + b64}}
	} else {
		part = map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + in.mime + ";base64," + b64, "detail": "high"}}
	}
	body := map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "system", "content": prompt},
			map[string]any{"role": "user", "content": []any{part, map[string]any{"type": "text", "text": "Vypiš údaje z tohoto dokladu."}}},
		},
		"response_format":       map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "doklad", "strict": true, "schema": aiSchema()}},
		"max_completion_tokens": 16000,
	}
	if strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o") {
		body["reasoning_effort"] = "low"
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", openaiBase()+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := aiHTTP.Do(req)
	if err != nil {
		return nil, errors.New("OpenAI teď neodpovídá, zkuste to za chvíli.")
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return nil, openaiErr(res.StatusCode, data)
	}
	var j struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &j); err != nil || len(j.Choices) == 0 {
		return nil, errors.New("OpenAI vrátil nečekanou odpověď.")
	}
	out := &aiResult{In: j.Usage.Prompt, Out: j.Usage.Completion}
	ch := j.Choices[0]
	if ch.Message.Refusal != "" {
		return nil, errors.New("OpenAI doklad odmítl přečíst.")
	}
	if ch.FinishReason == "length" {
		return nil, errors.New("Doklad je moc dlouhý na přečtení najednou.")
	}
	if err := json.Unmarshal([]byte(ch.Message.Content), &out.Doc); err != nil {
		return nil, errors.New("Z dokladu se nepodařilo vyčíst údaje.")
	}
	return out, nil
}

// ---------- Anthropic (Messages API, official SDK) ----------

func anthropicClient(key string) anthropic.Client {
	opts := []option.RequestOption{option.WithAPIKey(key), option.WithMaxRetries(2)}
	if u := os.Getenv("STINKO_ANTHROPIC_URL"); u != "" {
		opts = append(opts, option.WithBaseURL(u))
	}
	return anthropic.NewClient(opts...)
}

func anthropicErr(err error) error {
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == 401:
			return errors.New("Anthropic odmítl API klíč – zkontrolujte ho.")
		case ae.StatusCode == 400 && strings.Contains(strings.ToLower(ae.Error()), "credit"):
			return errors.New("Na účtu Anthropic došel kredit – dobijte ho v console.anthropic.com.")
		case ae.StatusCode == 429 || ae.StatusCode >= 500:
			return errors.New("Anthropic je teď přetížený, zkuste to za chvíli.")
		}
		return fmt.Errorf("Anthropic vrátil chybu %d.", ae.StatusCode)
	}
	return errors.New("Anthropic teď neodpovídá, zkuste to za chvíli.")
}

func anthropicCheck(key string) error {
	cl := anthropicClient(key)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := cl.Models.List(ctx, anthropic.ModelListParams{}); err != nil {
		return anthropicErr(err)
	}
	return nil
}

func anthropicRead(ctx context.Context, key, model, prompt string, in aiInput) (*aiResult, error) {
	b64 := base64.StdEncoding.EncodeToString(in.data)
	var doc anthropic.ContentBlockParamUnion
	if in.mime == "application/pdf" {
		doc = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		doc = anthropic.NewImageBlockBase64(in.mime, b64)
	}
	cl := anthropicClient(key)
	resp, err := cl.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 16000,
		System:    []anthropic.TextBlockParam{{Text: prompt}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(doc, anthropic.NewTextBlock("Vypiš údaje z tohoto dokladu.")),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: aiSchema()},
		},
	})
	if err != nil {
		return nil, anthropicErr(err)
	}
	out := &aiResult{In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return nil, errors.New("Anthropic doklad odmítl přečíst.")
	case anthropic.StopReasonMaxTokens:
		return nil, errors.New("Doklad je moc dlouhý na přečtení najednou.")
	}
	text := ""
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text += t.Text
		}
	}
	if err := json.Unmarshal([]byte(text), &out.Doc); err != nil {
		return nil, errors.New("Z dokladu se nepodařilo vyčíst údaje.")
	}
	return out, nil
}
