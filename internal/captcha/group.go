package captcha

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// BrowserGroup fans Extract across n independent Chrome processes.
// Same-Chrome multi-tab does not work on build.nvidia.com: a second tab never
// mounts the invisible hCaptcha widget (CreateTarget probe: widget timeout).
//
// After a hard extract failure (empty token / dead widget), the offending Chrome
// is killed and replaced so the pool is not stuck on a zombie process.
type BrowserGroup struct {
	parent   context.Context
	cfg      BrowserConfig
	browsers []*Browser
	free     chan *Browser
	done     chan struct{}

	mu     sync.Mutex
	closed bool
}

// NewBrowserGroup starts n warmed browsers (n Chrome processes). n < 1 means 1.
func NewBrowserGroup(parent context.Context, n int, cfg BrowserConfig) (*BrowserGroup, error) {
	if n < 1 {
		n = 1
	}
	cfg = cfg.withDefaults()
	g := &BrowserGroup{
		parent:   parent,
		cfg:      cfg,
		browsers: make([]*Browser, 0, n),
		free:     make(chan *Browser, n),
		done:     make(chan struct{}),
	}
	if cfg.Proxy != "" {
		log.Printf("captcha chrome proxy=%s", cfg.Proxy)
	}
	for i := 0; i < n; i++ {
		b, err := NewBrowser(parent, cfg)
		if err != nil {
			g.Close()
			return nil, fmt.Errorf("captcha browser %d: %w", i, err)
		}
		g.browsers = append(g.browsers, b)
		g.free <- b
	}
	return g, nil
}

// Len returns how many Chrome workers are available.
func (g *BrowserGroup) Len() int {
	return len(g.browsers)
}

// Extract borrows a free browser, mints one token, then returns it to the pool.
// Hard failures recycle the Chrome process once before giving up.
func (g *BrowserGroup) Extract(ctx context.Context) (string, error) {
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return "", fmt.Errorf("captcha browser group closed")
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-g.done:
		return "", fmt.Errorf("captcha browser group closed")
	case b := <-g.free:
		tok, err := b.Extract(ctx)
		if err == nil {
			g.release(b)
			return tok, nil
		}
		if ctx.Err() != nil || !isHardExtractFailure(err) {
			g.release(b)
			return "", err
		}
		log.Printf("captcha browser hard failure; recycling chrome: %v", err)
		nb, rerr := g.recycle(b)
		if rerr != nil {
			// old browser already closed inside recycle on success only;
			// on failure keep the slot with the old browser if still usable.
			g.release(b)
			return "", fmt.Errorf("%w; chrome recycle: %v", err, rerr)
		}
		tok, err = nb.Extract(ctx)
		g.release(nb)
		if err != nil {
			return "", fmt.Errorf("after chrome recycle: %w", err)
		}
		return tok, nil
	}
}

func (g *BrowserGroup) release(b *Browser) {
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return
	}
	select {
	case g.free <- b:
	default:
	}
}

// recycle replaces old with a freshly warmed Chrome. On success old is closed
// and must not be released; the caller releases the returned browser instead.
func (g *BrowserGroup) recycle(old *Browser) (*Browser, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, fmt.Errorf("captcha browser group closed")
	}
	nb, err := NewBrowser(g.parent, g.cfg)
	if err != nil {
		return nil, err
	}
	replaced := false
	for i, b := range g.browsers {
		if b == old {
			g.browsers[i] = nb
			replaced = true
			break
		}
	}
	if !replaced {
		nb.Close()
		return nil, fmt.Errorf("browser not in group")
	}
	old.Close()
	log.Printf("captcha browser recycled after hard extract failure")
	return nb, nil
}

// Close stops every Chrome process in the group.
func (g *BrowserGroup) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	close(g.done)
	g.mu.Unlock()

	for _, b := range g.browsers {
		b.Close()
	}
}

func isHardExtractFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "empty captcha token") ||
		strings.Contains(s, "re-navigate failed") ||
		strings.Contains(s, "hcaptcha global not ready") ||
		strings.Contains(s, "chromedp navigate") ||
		strings.Contains(s, "captcha token did not refresh")
}
