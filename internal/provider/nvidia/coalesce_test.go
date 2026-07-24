package nvidia

import (
	"encoding/json"
	"errors"
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

	var events []string
	if err := coalesceSSEEvents(strings.NewReader(upstream), 50*time.Millisecond, func(line string) error {
		events = append(events, strings.TrimPrefix(line, "data: "))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if len(events) < 2 {
		t.Fatalf("expected coalesced content + DONE, got %d events: %#v", len(events), events)
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
		t.Fatalf("content=%q events=%d", content.String(), len(events)-1)
	}
	if len(events)-1 < 1 || len(events)-1 > 3 {
		t.Fatalf("want 1-3 content events after eager-first flush, got %d", len(events)-1)
	}
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
	var body strings.Builder
	if err := coalesceSSEEvents(strings.NewReader(upstream), 0, func(line string) error {
		body.WriteString(line)
		body.WriteString("\n")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), "Z") {
		t.Fatalf("passthrough body=%q", body.String())
	}
}

func TestCoalesceSSEEarlyEmitErrorDoesNotHang(t *testing.T) {
	var lines []string
	for i := 0; i < 64; i++ {
		lines = append(lines,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
			``,
		)
	}
	upstream := strings.Join(lines, "\n")
	errWriteFailed := errors.New("write failed")

	n := 0
	done := make(chan error, 1)
	go func() {
		done <- coalesceSSEEvents(strings.NewReader(upstream), 50*time.Millisecond, func(line string) error {
			n++
			if n > 1 {
				return errWriteFailed
			}
			return nil
		})
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
		t.Fatal("coalesceSSEEvents hung after emit error")
	}
}
