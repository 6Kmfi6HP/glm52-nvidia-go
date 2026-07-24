package nvidia

import (
	"encoding/json"
	"testing"
)

func TestNormalizeRequestBody(t *testing.T) {
	in := []byte(`{"stream":true,"messages":[]}`)
	out, err := NormalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	opts := raw["stream_options"].(map[string]any)
	if opts["continuous_usage_stats"] != false {
		t.Fatalf("got %#v", opts["continuous_usage_stats"])
	}
	kw := raw["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != true || kw["clear_thinking"] != false {
		t.Fatalf("thinking kwargs = %#v", kw)
	}
}

func TestNormalizeRequestBodyEmptyOrNullKwargs(t *testing.T) {
	cases := []string{
		`{"stream":false,"chat_template_kwargs":{}}`,
		`{"stream":false,"chat_template_kwargs":null}`,
	}
	for _, in := range cases {
		out, err := NormalizeRequestBody([]byte(in))
		if err != nil {
			t.Fatalf("in=%s: %v", in, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatal(err)
		}
		kw := raw["chat_template_kwargs"].(map[string]any)
		if kw["enable_thinking"] != true || kw["clear_thinking"] != false {
			t.Fatalf("in=%s thinking kwargs = %#v", in, kw)
		}
	}
}

func TestNormalizeRequestBodyPreservesThinking(t *testing.T) {
	in := []byte(`{"stream":false,"chat_template_kwargs":{"enable_thinking":false}}`)
	out, err := NormalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	kw := raw["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Fatalf("should preserve caller kwargs, got %#v", kw)
	}
}

func TestNormalizeThinkingKwargsAliases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     map[string]any
		stripped []string
	}{
		{
			name: "zai thinking enabled",
			in:   `{"stream":false,"thinking":{"type":"enabled","clear_thinking":false}}`,
			want: map[string]any{
				"enable_thinking": true,
				"clear_thinking":  false,
			},
			stripped: []string{"thinking"},
		},
		{
			name: "zai thinking disabled",
			in:   `{"stream":false,"thinking":{"type":"disabled"}}`,
			want: map[string]any{
				"enable_thinking": false,
			},
			stripped: []string{"thinking"},
		},
		{
			name: "top-level enable_thinking false",
			in:   `{"stream":false,"enable_thinking":false}`,
			want: map[string]any{
				"enable_thinking": false,
			},
			stripped: []string{"enable_thinking"},
		},
		{
			name: "top-level enable_thinking true + effort",
			in:   `{"stream":false,"enable_thinking":true,"reasoning_effort":"high"}`,
			want: map[string]any{
				"enable_thinking":  true,
				"clear_thinking":   false,
				"reasoning_effort": "high",
			},
			stripped: []string{"enable_thinking", "reasoning_effort"},
		},
		{
			name: "kwargs wins over aliases",
			in:   `{"stream":false,"chat_template_kwargs":{"enable_thinking":false},"thinking":{"type":"enabled"},"enable_thinking":true}`,
			want: map[string]any{
				"enable_thinking": false,
			},
			stripped: []string{"thinking", "enable_thinking"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeRequestBody([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.stripped {
				if _, ok := raw[key]; ok {
					t.Fatalf("alias %q should be stripped, body=%#v", key, raw)
				}
			}
			kw := raw["chat_template_kwargs"].(map[string]any)
			if len(kw) != len(tc.want) {
				t.Fatalf("kwargs=%#v want %#v", kw, tc.want)
			}
			for k, v := range tc.want {
				if kw[k] != v {
					t.Fatalf("kwargs[%q]=%#v want %#v (full=%#v)", k, kw[k], v, kw)
				}
			}
		})
	}
}
