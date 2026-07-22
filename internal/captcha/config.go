package captcha

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/chromedp/chromedp"
)

// BrowserConfig controls headless Chrome used for captcha extraction.
type BrowserConfig struct {
	// Proxy is a Chrome --proxy-server URL, e.g. "socks5://127.0.0.1:7890".
	// Empty falls back to CHROME_PROXY.
	Proxy string
}

func (c BrowserConfig) withDefaults() BrowserConfig {
	if c.Proxy == "" {
		c.Proxy = strings.TrimSpace(os.Getenv("CHROME_PROXY"))
	}
	return c
}

// chromeDisableFeatures extends DefaultExecAllocatorOptions' disable-features
// (Flag overwrites, so defaults must be restated) with cuts for Google
// background services that add startup network without helping hCaptcha.
const chromeDisableFeatures = "site-per-process,Translate,BlinkGenPropertyTrees," +
	"OptimizationHints,MediaRouter,InterestFeedContentSuggestions," +
	"AutofillServerCommunication,CertificateTransparencyComponentUpdater"

// ChromeAllocatorOptions is DefaultExecAllocatorOptions plus anti-automation
// and Google-background cuts. Callers still apply CHROME_PATH / sandbox /
// images / proxy env overlays.
func ChromeAllocatorOptions() []chromedp.ExecAllocatorOption {
	return append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
		// Resource-saving flags that do not affect hCaptcha token extraction:
		// the tab only needs JS to fire hcaptcha.execute() and read one attribute.
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-component-extensions-with-background-pages", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-domain-reliability", true),
		chromedp.Flag("no-pings", true),
		chromedp.Flag("disable-features", chromeDisableFeatures),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		chromedp.WindowSize(1280, 900),
	)
}

// chromeProxyOpts sets --proxy-server. Optional CHROME_PROXY_REMOTE_DNS=1 adds
// host-resolver-rules so DNS also goes through SOCKS (can break some clients
// that lack UDP ASSOCIATE / remote-resolve support — leave off by default).
func chromeProxyOpts(proxy string) []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{chromedp.ProxyServer(proxy)}
	if os.Getenv("CHROME_PROXY_REMOTE_DNS") != "1" {
		return opts
	}
	host, ok := proxyHost(proxy)
	if !ok {
		return opts
	}
	scheme := strings.ToLower(proxyScheme(proxy))
	if strings.HasPrefix(scheme, "socks") {
		opts = append(opts, chromedp.Flag("host-resolver-rules",
			fmt.Sprintf("MAP * ~NOTFOUND , EXCLUDE %s", host)))
	}
	return opts
}

func proxyScheme(proxy string) string {
	u, err := url.Parse(proxy)
	if err != nil {
		return ""
	}
	return u.Scheme
}

func proxyHost(proxy string) (string, bool) {
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return "", false
	}
	host := u.Hostname()
	if host == "" {
		return "", false
	}
	// host-resolver-rules EXCLUDE wants a hostname or IP, not host:port.
	if net.ParseIP(host) != nil || strings.Contains(host, ".") || host == "localhost" {
		return host, true
	}
	return host, true
}
