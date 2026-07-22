package captcha

import "testing"

func TestProxyHost(t *testing.T) {
	host, ok := proxyHost("socks5://100.74.21.88:7890")
	if !ok || host != "100.74.21.88" {
		t.Fatalf("got %q ok=%v", host, ok)
	}
	host, ok = proxyHost("http://proxy.example:8080")
	if !ok || host != "proxy.example" {
		t.Fatalf("got %q ok=%v", host, ok)
	}
}

func TestBrowserConfigProxyFromEnv(t *testing.T) {
	t.Setenv("CHROME_PROXY", "socks5://127.0.0.1:1080")
	cfg := BrowserConfig{}.withDefaults()
	if cfg.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy=%q", cfg.Proxy)
	}
	cfg = BrowserConfig{Proxy: "socks5://explicit:1"}.withDefaults()
	if cfg.Proxy != "socks5://explicit:1" {
		t.Fatalf("explicit override lost: %q", cfg.Proxy)
	}
}
