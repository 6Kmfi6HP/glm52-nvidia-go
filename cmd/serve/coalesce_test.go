package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMergeableContent(t *testing.T) {
	okChunk := map[string]any{
		"choices": []any{
			map[string]any{
				"delta":         map[string]any{"content": "hi"},
				"finish_reason": nil,
			},
		},
	}
	got, ok := mergeableContent(okChunk)
	if !ok || got != "hi" {
		t.Fatalf("mergeable content: got %q ok=%v", got, ok)
	}

	withUsage := map[string]any{
		"usage": map[string]any{"total_tokens": 1},
		"choices": []any{
			map[string]any{"delta": map[string]any{"content": "x"}},
		},
	}
	if _, ok := mergeableContent(withUsage); ok {
		t.Fatal("usage chunk should not be mergeable")
	}

	withReason := map[string]any{
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"content":           "x",
					"reasoning_content": "think",
				},
			},
		},
	}
	if _, ok := mergeableContent(withReason); ok {
		t.Fatal("reasoning delta should not be mergeable")
	}
}

func TestTryMergeContent(t *testing.T) {
	pending := map[string]any{
		"choices": []any{
			map[string]any{"delta": map[string]any{"content": "Hel"}},
		},
	}
	if !tryMergeContent(pending, "lo") {
		t.Fatal("merge failed")
	}
	ch := pending["choices"].([]any)[0].(map[string]any)
	delta := ch["delta"].(map[string]any)
	if delta["content"] != "Hello" {
		t.Fatalf("got %v", delta["content"])
	}
}

func TestCoalesceSSEMergesWindow(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"A"},"finish_reason":null}]}`,
		``,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"B"},"finish_reason":null}]}`,
		``,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"C"},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	rec := httptest.NewRecorder()
	if err := coalesceSSE(rec, strings.NewReader(upstream), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	body := rec.Body.String()
	events := parseDataEvents(body)
	if len(events) < 2 {
		t.Fatalf("expected coalesced content + DONE, got %d events: %q", len(events), body)
	}
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("last event want [DONE], got %q", events[len(events)-1])
	}

	var content strings.Builder
	for _, ev := range events[:len(events)-1] {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(ev), &chunk); err != nil {
			t.Fatal(err)
		}
		choices := chunk["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		content.WriteString(delta["content"].(string))
	}
	if content.String() != "ABC" {
		t.Fatalf("content=%q events=%d body=%q", content.String(), len(events)-1, body)
	}
	// First content flushes immediately (TTFT); later deltas may coalesce.
	if len(events)-1 < 1 || len(events)-1 > 3 {
		t.Fatalf("want 1-3 content events after eager-first flush, got %d body=%q", len(events)-1, body)
	}
	// First event should be just "A" (eager flush).
	var first map[string]any
	if err := json.Unmarshal([]byte(events[0]), &first); err != nil {
		t.Fatal(err)
	}
	firstContent := first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"].(string)
	if firstContent != "A" {
		t.Fatalf("first event content=%q want A (eager TTFT flush)", firstContent)
	}
}

func TestCoalesceSSEPassthroughWhenOff(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"Z\"}}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := coalesceSSE(rec, strings.NewReader(upstream), 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "Z") {
		t.Fatalf("passthrough body=%q", rec.Body.String())
	}
}

// failAfterNWriter fails writes after n successful Write calls so coalesceSSE
// returns early while the upstream reader may still be producing lines.
type failAfterNWriter struct {
	http.ResponseWriter
	n, writes int
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.n {
		return 0, errWriteFailed
	}
	return w.ResponseWriter.Write(p)
}

func (w *failAfterNWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var errWriteFailed = errors.New("write failed")

func TestCoalesceSSEEarlyWriteErrorDoesNotHang(t *testing.T) {
	var lines []string
	for i := 0; i < 64; i++ {
		lines = append(lines,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
			``,
		)
	}
	upstream := strings.Join(lines, "\n")

	rec := httptest.NewRecorder()
	w := &failAfterNWriter{ResponseWriter: rec, n: 1}

	done := make(chan error, 1)
	go func() {
		done <- coalesceSSE(w, strings.NewReader(upstream), 50*time.Millisecond)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected write error")
		}
		if !errors.Is(err, errWriteFailed) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("coalesceSSE hung after write error (reader goroutine leak?)")
	}
}

func parseDataEvents(body string) []string {
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				out = append(out, strings.TrimPrefix(line, "data: "))
			}
		}
	}
	return out
}
