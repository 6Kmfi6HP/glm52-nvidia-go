package captcha

import (
	"context"
	"fmt"
	"sync"
)

// BrowserGroup fans Extract across n independent Chrome processes.
// Same-Chrome multi-tab does not work on build.nvidia.com: a second tab never
// mounts the invisible hCaptcha widget (CreateTarget probe: widget timeout).
type BrowserGroup struct {
	browsers []*Browser
	free     chan *Browser
	done     chan struct{}

	mu     sync.Mutex
	closed bool
}

// NewBrowserGroup starts n warmed browsers (n Chrome processes). n < 1 means 1.
func NewBrowserGroup(parent context.Context, n int) (*BrowserGroup, error) {
	if n < 1 {
		n = 1
	}
	g := &BrowserGroup{
		browsers: make([]*Browser, 0, n),
		free:     make(chan *Browser, n),
		done:     make(chan struct{}),
	}
	for i := 0; i < n; i++ {
		b, err := NewBrowser(parent)
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
		defer g.release(b)
		return b.Extract(ctx)
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
