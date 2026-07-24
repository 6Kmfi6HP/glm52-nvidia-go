package main

import (
	"os"
	"testing"
)

func TestParseListenAddr(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{":8080", "", 8080},
		{"127.0.0.1:9090", "127.0.0.1", 9090},
		{"", "", 8080},
	}
	for _, tc := range cases {
		h, p, err := parseListenAddr(tc.in)
		if err != nil {
			t.Fatalf("addr=%q: %v", tc.in, err)
		}
		if h != tc.wantHost || p != tc.wantPort {
			t.Fatalf("addr=%q got %q %d want %q %d", tc.in, h, p, tc.wantHost, tc.wantPort)
		}
	}
	if _, _, err := parseListenAddr("not-an-addr"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildConfigEmptyAPIKeys(t *testing.T) {
	cfg, path, err := buildConfig(":18080")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cfg.AuthDir)
	if cfg.Port != 18080 {
		t.Fatalf("port=%d", cfg.Port)
	}
	if len(cfg.APIKeys) != 0 {
		t.Fatalf("APIKeys=%v want empty", cfg.APIKeys)
	}
	if path == "" || cfg.AuthDir == "" {
		t.Fatal("missing paths")
	}
	if !cfg.RemoteManagement.DisableControlPanel {
		t.Fatal("expected control panel disabled")
	}
}
