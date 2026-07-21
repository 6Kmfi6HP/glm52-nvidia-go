package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// coalesceSSE reads upstream SSE, merges consecutive content deltas within
// window, and writes/flushes coalesced events to the client.
func coalesceSSE(w http.ResponseWriter, src io.Reader, window time.Duration) error {
	if window <= 0 {
		return pipeSSE(w, src)
	}

	flusher, _ := w.(http.Flusher)
	writeEvent := func(line string) error {
		if _, err := io.WriteString(w, line+"\n\n"); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 16)
	done := make(chan struct{})
	defer close(done)

	go func() {
		reader := bufio.NewReaderSize(src, 64<<10)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				select {
				case ch <- readResult{line: strings.TrimRight(line, "\r\n")}:
				case <-done:
					return
				}
			}
			if err != nil {
				select {
				case ch <- readResult{err: err}:
				case <-done:
				}
				return
			}
		}
	}()

	var (
		pending   map[string]any
		timer     *time.Timer
		timerC    <-chan time.Time
		firstSent bool
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
	}
	defer stopTimer()
	flushPending := func() error {
		stopTimer()
		if pending == nil {
			return nil
		}
		raw, err := json.Marshal(pending)
		pending = nil
		if err != nil {
			return err
		}
		return writeEvent("data: " + string(raw))
	}
	armTimer := func() {
		stopTimer()
		timer = time.NewTimer(window)
		timerC = timer.C
	}

	for {
		select {
		case <-timerC:
			if err := flushPending(); err != nil {
				return err
			}

		case rr := <-ch:
			if rr.err != nil && rr.line == "" {
				ferr := flushPending()
				if rr.err == io.EOF {
					return ferr
				}
				if ferr != nil {
					return ferr
				}
				return rr.err
			}

			line := rr.line
			switch {
			case line == "" || strings.HasPrefix(line, ":"):
				// ignore keep-alives / comments

			case line == "data: [DONE]":
				if err := flushPending(); err != nil {
					return err
				}
				if err := writeEvent(line); err != nil {
					return err
				}

			case strings.HasPrefix(line, "data: "):
				data := strings.TrimPrefix(line, "data: ")
				var chunk map[string]any
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					if err := flushPending(); err != nil {
						return err
					}
					if err := writeEvent(line); err != nil {
						return err
					}
					break
				}

				content, ok := mergeableContent(chunk)
				if !ok {
					if err := flushPending(); err != nil {
						return err
					}
					if err := writeEvent(line); err != nil {
						return err
					}
					break
				}

				if pending == nil {
					pending = chunk
					// Flush the first content token immediately (TTFT), then
					// coalesce subsequent deltas within the window.
					if !firstSent {
						if err := flushPending(); err != nil {
							return err
						}
						firstSent = true
						break
					}
					armTimer()
					break
				}
				if tryMergeContent(pending, content) {
					break
				}
				if err := flushPending(); err != nil {
					return err
				}
				pending = chunk
				armTimer()

			default:
				if err := flushPending(); err != nil {
					return err
				}
				if err := writeEvent(line); err != nil {
					return err
				}
			}

			if rr.err != nil {
				ferr := flushPending()
				if rr.err == io.EOF {
					return ferr
				}
				if ferr != nil {
					return ferr
				}
				return rr.err
			}
		}
	}
}

// mergeableContent returns the delta content if this chunk is a pure content
// delta (safe to coalesce).
func mergeableContent(chunk map[string]any) (string, bool) {
	if u, ok := chunk["usage"]; ok && u != nil {
		return "", false
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return "", false
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return "", false
	}
	if fr, ok := ch["finish_reason"]; ok && fr != nil {
		if s, ok := fr.(string); ok && s != "" {
			return "", false
		}
	}
	delta, ok := ch["delta"].(map[string]any)
	if !ok {
		return "", false
	}
	content, hasContent := delta["content"].(string)
	if !hasContent || content == "" {
		return "", false
	}
	for k := range delta {
		switch k {
		case "content", "role":
		default:
			return "", false // reasoning_content, tool_calls, etc.
		}
	}
	return content, true
}

func tryMergeContent(pending map[string]any, content string) bool {
	choices, ok := pending["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	delta, ok := ch["delta"].(map[string]any)
	if !ok {
		return false
	}
	prev, _ := delta["content"].(string)
	delta["content"] = prev + content
	return true
}
