package captcha

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// ExtractFunc obtains one one-shot captcha token.
type ExtractFunc func(ctx context.Context) (string, error)

// Pool pre-warms one-shot captcha tokens on a bounded channel so request
// handlers can Take without waiting on a full browser navigate.
type Pool struct {
	extract ExtractFunc
	tokens  chan string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	fills   atomic.Uint64
	takes   atomic.Uint64
	errors  atomic.Uint64
}

// PoolConfig controls prewarm depth and parallelism.
type PoolConfig struct {
	Size    int // buffered ready tokens (default 2)
	Workers int // concurrent extractors (default 1)
}

// NewPool starts background workers that keep tokens filled up to Size.
// extract must be safe for concurrent use up to Workers (e.g. Browser.Extract).
func NewPool(parent context.Context, extract ExtractFunc, cfg PoolConfig) *Pool {
	if cfg.Size < 1 {
		cfg.Size = 2
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		extract: extract,
		tokens:  make(chan string, cfg.Size),
		ctx:     ctx,
		cancel:  cancel,
	}
	for i := 0; i < cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
	return p
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		token, err := p.extract(p.ctx)
		if err != nil {
			p.errors.Add(1)
			if p.ctx.Err() != nil {
				return
			}
			log.Printf("captcha pool worker %d: %v", id, err)
			select {
			case <-time.After(2 * time.Second):
			case <-p.ctx.Done():
				return
			}
			continue
		}

		select {
		case p.tokens <- token:
			p.fills.Add(1)
		case <-p.ctx.Done():
			return
		}
	}
}

// Take returns a prewarmed token, or blocks until one is ready / ctx cancels.
func (p *Pool) Take(ctx context.Context) (string, error) {
	select {
	case token := <-p.tokens:
		p.takes.Add(1)
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-p.ctx.Done():
		return "", fmt.Errorf("captcha pool closed")
	}
}

// Stats returns fill/take/error counters for experiments.
func (p *Pool) Stats() (fills, takes, errors uint64) {
	return p.fills.Load(), p.takes.Load(), p.errors.Load()
}

// Ready returns how many tokens are currently buffered.
func (p *Pool) Ready() int {
	return len(p.tokens)
}

// Close stops workers and drains the browser-facing extract loop.
func (p *Pool) Close() {
	p.cancel()
	p.wg.Wait()
}
