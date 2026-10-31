package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const aiSampleDoc = `{"kind":"receipt","supplier":{"name":"HORNBACH Baumarkt CS spol. s r.o.","ico":"256 19 761","dic":"cz 25619761"},"number":"4711","vs":"","issued":"2026-10-30","duzp":"2026-10-30","due":"30.10.2026","currency":"czk","total":1452.5,"base":1200.41,"vat":252.09,"paidHow":"card","category":"Materiál a zboží","items":[{"name":"Šroub 6x60","qty":100,"unit":"ks","price":2.5,"vatRate":21,"total":250}],"note":""}`

func checkAiDoc(t *testing.T, d aiDoc) {
	t.Helper()
	cleanAiDoc(&d)
	if d.Supplier.Ico != "25619761" || d.Supplier.Dic != "CZ25619761" || d.Currency != "CZK" || d.Due != "" || d.Issued != "2026-10-30" {
		t.Fatalf("clean: %+v", d)
	}
	if d.Total != 1452.5 || len(d.Items) != 1 || d.Items[0].Qty != 100 || d.PaidHow != "card" {
		t.Fatalf("doc: %+v", d)
	}
}

func TestOpenaiRead(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		content, _ := json.Marshal(aiSampleDoc)
		io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":`+string(content)+`}}],"usage":{"prompt_tokens":3000,"completion_tokens":800}}`)
	}))
	defer srv.Close()
	t.Setenv("STINKO_OPENAI_URL", srv.URL)
	res, err := openaiRead(context.Background(), "sk-test", "gpt-5-mini", aiPrompt(accCats), aiInput{mime: "image/jpeg", data: []byte{0xff, 0xd8, 0xff}})
	if err != nil {
		t.Fatal(err)
	}
	checkAiDoc(t, res.Doc)
	if res.In != 3000 || res.Out != 800 {
		t.Fatalf("usage %+v", res)
	}
	rf := got["response_format"].(map[string]any)["json_schema"].(map[string]any)
	if rf["strict"] != true || got["reasoning_effort"] != "low" {
		t.Fatalf("body: %+v", got)
	}
	user := got["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(user["image_url"].(map[string]any)["url"].(string), "data:image/jpeg;base64,") {
		t.Fatalf("image part %+v", user)
	}
	if c := aiCost("gpt-5-mini", 3000, 800) * usdCzk; c <= 0 || c > 0.1 {
		t.Fatalf("cost %v Kč", c)
	}
}

func TestOpenaiErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"You exceeded your current quota","code":"insufficient_quota"}}`)
	}))
	defer srv.Close()
	t.Setenv("STINKO_OPENAI_URL", srv.URL)
	_, err := openaiRead(context.Background(), "k", "gpt-5-mini", "p", aiInput{mime: "application/pdf", data: []byte("%PDF-1.4")})
	if err == nil || !strings.Contains(err.Error(), "kredit") {
		t.Fatalf("err %v", err)
	}
}

func TestAnthropicRead(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer sk-ant-test" {
			t.Errorf("request %s %v", r.URL.Path, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		text, _ := json.Marshal(aiSampleDoc)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"end_turn","content":[{"type":"text","text":`+string(text)+`}],"usage":{"input_tokens":2500,"output_tokens":700}}`)
	}))
	defer srv.Close()
	t.Setenv("STINKO_ANTHROPIC_URL", srv.URL)
	res, err := anthropicRead(context.Background(), "sk-ant-test", "claude-haiku-5-5", aiPrompt(accCats), aiInput{mime: "application/pdf", data: []byte("%PDF-1.4")})
	if err != nil {
		t.Fatal(err)
	}
	checkAiDoc(t, res.Doc)
	oc := got["output_config"].(map[string]any)
	if oc["effort"] != "low" || oc["format"].(map[string]any)["type"] != "json_schema" {
		t.Fatalf("output_config %+v", oc)
	}
	part := got["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["type"] != "document" {
		t.Fatalf("part %+v", part)
	}
}

func TestAiCfgSaveKeepsKeys(t *testing.T) {
	app := testApp(t)
	if err := ensureSecretsSchema(app); err != nil {
		t.Fatal(err)
	}
	c, rec := loadAiCfg(app)
	if c.ready() || rec != nil || c.Provider != "openai" || c.model(false) != defOpenai || c.model(true) != defOpenai2 {
		t.Fatalf("defaults %+v", c)
	}
	c.OpenaiKey = "sk-1"
	if err := saveAiCfg(app, c, rec); err != nil {
		t.Fatal(err)
	}
	c, rec = loadAiCfg(app)
	if !c.ready() || rec == nil {
		t.Fatalf("saved %+v", c)
	}
	c.Provider = "anthropic"
	if c.ready() || c.model(false) != defAnthropic {
		t.Fatalf("anthropic without key %+v", c)
	}
}
