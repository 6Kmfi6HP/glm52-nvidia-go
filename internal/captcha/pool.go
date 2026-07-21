package captcha

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// backoff schedule on extract failure. Starts small, caps so a sustained
// captcha-block doesn't busy-loop or spam logs. Reset to zero on success.
const (
	backoffMin    = 1 * time.Second
	backoffMax    = 30 * time.Second
	backoffJitter = 250 * time.Millisecond // ±25% via Int63n below
	// log every N consecutive failures instead of each one, so a persistent
	// captcha outage does not flood logs.
	logEveryNth = 10
)

// ExtractFunc obtains one one-shot captcha token.
type ExtractFunc func(ctx context.Context) (string, error)

type entry struct {
	token string
	at    time.Time
}

// Pool pre-warms one-shot captcha tokens on a bounded channel so request
// handlers can Take without waiting on a full browser navigate.
// Tokens older than TTL are discarded on Take (hCaptcha tokens expire ~2–3 min).
type Pool struct {
	extract ExtractFunc
	tokens  chan entry
	ttl     time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	fills   atomic.Uint64
	takes   atomic.Uint64
	errors  atomic.Uint64
	expired atomic.Uint64
}

// PoolConfig controls prewarm depth and parallelism.
type PoolConfig struct {
	Size    int           // buffered ready tokens (default 2)
	Workers int           // concurrent extractors (default 1)
	TTL     time.Duration // max age before a pooled token is discarded (default 90s)
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
	if cfg.TTL <= 0 {
		cfg.TTL = 90 * time.Second
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		extract: extract,
		tokens:  make(chan entry, cfg.Size),
		ttl:     cfg.TTL,
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
	var consecFailures int
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		token, err := p.extract(p.ctx)
		if err != nil {
			p.errors.Add(1)
			consecFailures++
			if p.ctx.Err() != nil {
				return
			}
			// Exponential backoff with jitter — a sustained captcha outage
			// must not busy-loop (fixed 2s did) nor drown the logs. Reset on
			// success below.
			if consecFailures%logEveryNth == 0 {
				log.Printf("captcha pool worker %d: %v (consecutive failures=%d, backing off)",
					id, err, consecFailures)
			}
			backoff := backoffFor(consecFailures)
			select {
			case <-time.After(backoff):
			case <-p.ctx.Done():
				return
			}
			continue
		}

		consecFailures = 0
		select {
		case p.tokens <- entry{token: token, at: time.Now()}:
			p.fills.Add(1)
		case <-p.ctx.Done():
			return
		}
	}
}

// backoffFor computes 2^n * backoffMin capped at backoffMax, ±jitter.
// n=1 → ~1s, n=4 → ~8s, n≥5 → capped near 30s.
func backoffFor(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := backoffMin
	for i := 1; i < n; i++ {
		d *= 2
		if d >= backoffMax {
			d = backoffMax
			break
		}
	}
	jitter := time.Duration(rand.Int63n(int64(2*backoffJitter))) - backoffJitter
	d += jitter
	if d < 0 {
		d = 0
	}
	return d
}

// Take returns a prewarmed token that is still within TTL, or blocks until
// one is ready / ctx cancels. Expired tokens are dropped and counted.
func (p *Pool) Take(ctx context.Context) (string, error) {
	for {
		select {
		case e := <-p.tokens:
			if time.Since(e.at) > p.ttl {
				p.expired.Add(1)
				continue
			}
			p.takes.Add(1)
			return e.token, nil
		case <-ctx.Done():
			return "", ctx.Err()
		case <-p.ctx.Done():
			return "", fmt.Errorf("captcha pool closed")
		}
	}
}

// Stats returns fill/take/error/expired counters for experiments.
func (p *Pool) Stats() (fills, takes, errors, expired uint64) {
	return p.fills.Load(), p.takes.Load(), p.errors.Load(), p.expired.Load()
}

// Ready returns how many tokens are currently buffered (may include soon-to-expire).
func (p *Pool) Ready() int {
	return len(p.tokens)
}

// Close stops workers and drains the browser-facing extract loop.
func (p *Pool) Close() {
	p.cancel()
	p.wg.Wait()
}
