package main

import "testing"

func TestFindCacheSignals(t *testing.T) {
	tests := []struct {
		name  string
		usage map[string]any
		want  []string
	}{
		{
			name:  "nil",
			usage: nil,
			want:  nil,
		},
		{
			name: "plain openai usage no cache",
			usage: map[string]any{
				"prompt_tokens":     float64(100),
				"completion_tokens": float64(10),
				"total_tokens":      float64(110),
			},
			want: nil,
		},
		{
			name: "openai prompt_tokens_details.cached_tokens",
			usage: map[string]any{
				"prompt_tokens": float64(100),
				"prompt_tokens_details": map[string]any{
					"cached_tokens": float64(80),
				},
			},
			want: []string{"prompt_tokens_details", "prompt_tokens_details.cached_tokens"},
		},
		{
			name: "deepseek flat cache fields",
			usage: map[string]any{
				"prompt_tokens":            float64(100),
				"prompt_cache_hit_tokens":  float64(70),
				"prompt_cache_miss_tokens": float64(30),
			},
			want: []string{"prompt_cache_hit_tokens", "prompt_cache_miss_tokens"},
		},
		{
			name: "anthropic cache fields",
			usage: map[string]any{
				"input_tokens":                float64(100),
				"cache_read_input_tokens":     float64(60),
				"cache_creation_input_tokens": float64(40),
			},
			want: []string{"cache_read_input_tokens", "cache_creation_input_tokens"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findCacheSignals(tt.usage)
			if !sameSet(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ma := map[string]struct{}{}
	for _, s := range a {
		ma[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := ma[s]; !ok {
			return false
		}
	}
	return true
}
