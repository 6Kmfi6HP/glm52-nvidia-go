package main

import (
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
