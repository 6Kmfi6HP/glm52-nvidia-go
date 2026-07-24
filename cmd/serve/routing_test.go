package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An unknown model is rejected before any captcha token is spent, with a 400
// that names the model — so clients learn it isn't a playground model. This
// exercises the per-model registry lookup in the serve request path without
// any upstream call, browser, or network.
func TestUnknownModelRejected(t *testing.T) {
	s := &server{}
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"model":"no-such-org/never","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)

	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "no-such-org/never") {
		t.Fatalf("response %q should name the unknown model", got)
	}
}

// GET /v1/models returns the OpenAI list shape (object=list, data[].id) and is
// sorted. No pool/Chrome/network needed.
func TestListModelEndpoint(t *testing.T) {
	s := &server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	s.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if out.Object != "list" {
		t.Fatalf("object = %q want list", out.Object)
	}
	if len(out.Data) == 0 {
		t.Fatal("empty data")
	}
	byID := map[string]bool{}
	for _, m := range out.Data {
		if m.Object != "model" {
			t.Errorf("object = %q for %q", m.Object, m.ID)
		}
		if !strings.Contains(m.ID, "/") {
			t.Errorf("odd model id %q", m.ID)
		}
		byID[m.ID] = true
	}
	// sorted ascending
	for i := 1; i < len(out.Data); i++ {
		if out.Data[i-1].ID > out.Data[i].ID {
			t.Fatalf("not sorted: %q before %q", out.Data[i-1].ID, out.Data[i].ID)
		}
	}
	// a couple of known registry entries must be present
	for _, want := range []string{"z-ai/glm-5.2", "deepseek-ai/deepseek-v4-pro"} {
		if !byID[want] {
			t.Errorf("missing expected model %q", want)
		}
	}
	// a runtime-only model (nvcfFunctionId="None") is intentionally absent
	if byID["moonshotai/kimi-k2.6"] {
		t.Errorf("runtime-only model kimi-k2.6 should not be in the static registry")
	}
}
