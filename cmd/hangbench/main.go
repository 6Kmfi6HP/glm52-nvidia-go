// hangbench reproduces serve hang / gateway-timeout failure modes for the
// captcha pool: sticky idle re-nav, empty-pool Take waits, and multi-Chrome cost.
//
//	go run ./cmd/hangbench
//	go run ./cmd/hangbench -idle=65s -workers=1,2 -drain=4 -take-wait=30s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"glm52-nvidia/internal/captcha"
)

func main() {
	idle := flag.Duration("idle", 65*time.Second, "pause before post-idle extract (stickyMaxIdle is 60s)")
	workersCSV := flag.String("workers", "1,2", "comma-separated BrowserGroup sizes to compare")
	drain := flag.Int("drain", 4, "tokens to Take after pool warm (forces refill under load)")
	takeWait := flag.Duration("take-wait", 30*time.Second, "per-Take deadline when pool may be empty (serve captcha-wait)")
	poolSize := flag.Int("pool-size", 2, "pool buffer size")
	skipIdle := flag.Bool("skip-idle", false, "skip the sticky idle phase (faster)")
	idleBurst := flag.Bool("idle-burst", false, "only run drain→idle→concurrent Take (hang/504 path)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	workerNs, err := parseInts(*workersCSV)
	if err != nil || len(workerNs) == 0 {
		log.Fatalf("-workers: %v", err)
	}

	fmt.Printf("hangbench: idle=%s workers=%v drain=%d take-wait=%s pool-size=%d skip-idle=%v idle-burst=%v\n",
		*idle, workerNs, *drain, *takeWait, *poolSize, *skipIdle, *idleBurst)
	fmt.Printf("host chrome/chromedp procs before: %d\n", countChromeProcs())

	if *idleBurst {
		for _, n := range workerNs {
			if ctx.Err() != nil {
				break
			}
			fmt.Printf("\n=== C: idle-burst workers=%d ===\n", n)
			if err := phaseIdleBurst(ctx, n, *poolSize, *drain, *idle, *takeWait); err != nil {
				log.Printf("phase C workers=%d failed: %v", n, err)
			}
		}
		fmt.Printf("\nhost chrome/chromedp procs after: %d\n", countChromeProcs())
		fmt.Println("done")
		return
	}

	// --- Phase A: single sticky Browser cold / steady / post-idle ---
	fmt.Println("\n=== A: sticky Browser extract timing ===")
	if err := phaseSticky(ctx, *idle, *skipIdle); err != nil {
		log.Printf("phase A failed: %v", err)
	}

	// --- Phase B: pool empty Take under workers=N ---
	for _, n := range workerNs {
		if ctx.Err() != nil {
			break
		}
		fmt.Printf("\n=== B: pool workers=%d size=%d drain=%d ===\n", n, *poolSize, *drain)
		if err := phasePool(ctx, n, *poolSize, *drain, *takeWait); err != nil {
			log.Printf("phase B workers=%d failed: %v", n, err)
		}
	}

	fmt.Printf("\nhost chrome/chromedp procs after: %d\n", countChromeProcs())
	fmt.Println("done")
}

func phaseSticky(ctx context.Context, idle time.Duration, skipIdle bool) error {
	t0 := time.Now()
	b, err := captcha.NewBrowser(ctx, captcha.BrowserConfig{})
	if err != nil {
		return fmt.Errorf("NewBrowser: %w", err)
	}
	defer b.Close()
	fmt.Printf("  warm+alloc: %s  chrome_procs=%d\n", time.Since(t0).Round(time.Millisecond), countChromeProcs())

	for i := 1; i <= 3; i++ {
		tok, d, err := timedExtract(ctx, b)
		label := "steady"
		if i == 1 {
			label = "first_after_warm"
		}
		if err != nil {
			fmt.Printf("  extract#%d %s: FAIL %s err=%v\n", i, label, d, err)
			continue
		}
		fmt.Printf("  extract#%d %s: %s tok_len=%d\n", i, label, d, len(tok))
	}

	if skipIdle {
		return nil
	}
	fmt.Printf("  sleeping idle=%s (expect sticky re-nav on next)…\n", idle)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(idle):
	}
	tok, d, err := timedExtract(ctx, b)
	if err != nil {
		fmt.Printf("  extract#post_idle: FAIL %s err=%v\n", d, err)
		return nil
	}
	fmt.Printf("  extract#post_idle: %s tok_len=%d\n", d, len(tok))
	return nil
}

func phasePool(ctx context.Context, workers, size, drainN int, takeWait time.Duration) error {
	t0 := time.Now()
	g, err := captcha.NewBrowserGroup(ctx, workers, captcha.BrowserConfig{})
	if err != nil {
		return fmt.Errorf("NewBrowserGroup: %w", err)
	}
	defer g.Close()
	fmt.Printf("  group warm: %s chromes=%d chrome_procs=%d\n",
		time.Since(t0).Round(time.Millisecond), g.Len(), countChromeProcs())

	pool := captcha.NewPool(ctx, g.Extract, captcha.PoolConfig{
		Size:    size,
		Workers: workers,
		TTL:     90 * time.Second,
	})
	defer pool.Close()

	// Wait until at least 1 ready (same as serve warm).
	deadline := time.Now().Add(3 * time.Minute)
	for pool.Ready() < 1 {
		if time.Now().After(deadline) {
			return fmt.Errorf("pool never ready")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("  pool ready=%d (warm)\n", pool.Ready())

	// Concurrent Takes: first `size` should be fast (buffered); rest hit empty
	// and wait for workers — this is the serve hang / 504 path.
	type result struct {
		i   int
		d   time.Duration
		err error
	}
	results := make([]result, drainN)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < drainN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			takeCtx, cancel := context.WithTimeout(ctx, takeWait)
			defer cancel()
			t := time.Now()
			_, err := pool.Take(takeCtx)
			results[i] = result{i: i, d: time.Since(t), err: err}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	var ok, fail int
	var maxOK time.Duration
	for _, r := range results {
		if r.err != nil {
			fail++
			fmt.Printf("  take#%d FAIL %s err=%v\n", r.i+1, r.d.Round(time.Millisecond), r.err)
			continue
		}
		ok++
		if r.d > maxOK {
			maxOK = r.d
		}
		fmt.Printf("  take#%d OK %s\n", r.i+1, r.d.Round(time.Millisecond))
	}
	fills, takes, errs, expired := pool.Stats()
	fmt.Printf("  summary wall=%s ok=%d fail=%d max_ok=%s ready=%d fills=%d takes=%d errors=%d expired=%d chrome_procs=%d\n",
		wall.Round(time.Millisecond), ok, fail, maxOK.Round(time.Millisecond),
		pool.Ready(), fills, takes, errs, expired, countChromeProcs())
	return nil
}

// phaseIdleBurst drains the pool, idles past stickyMaxIdle, then issues concurrent
// Takes — the "chat, pause, request hangs / 504" path.
func phaseIdleBurst(ctx context.Context, workers, size, burst int, idle, takeWait time.Duration) error {
	t0 := time.Now()
	g, err := captcha.NewBrowserGroup(ctx, workers, captcha.BrowserConfig{})
	if err != nil {
		return err
	}
	defer g.Close()
	pool := captcha.NewPool(ctx, g.Extract, captcha.PoolConfig{
		Size:    size,
		Workers: workers,
		TTL:     90 * time.Second,
	})
	defer pool.Close()

	deadline := time.Now().Add(3 * time.Minute)
	for pool.Ready() < size && time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("  warm %s ready=%d chrome_procs=%d\n",
		time.Since(t0).Round(time.Millisecond), pool.Ready(), countChromeProcs())

	// Keep the buffer FULL during idle so workers stay blocked on send and
	// cannot refresh sticky lastOK — same as a quiet chat with unused pooled tokens.
	fmt.Printf("  holding full pool; idle %s (sticky ages)…\n", idle)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(idle):
	}
	fills, takes, errs, expired := pool.Stats()
	fmt.Printf("  after idle ready=%d fills=%d takes=%d errors=%d expired=%d chrome_procs=%d\n",
		pool.Ready(), fills, takes, errs, expired, countChromeProcs())

	type result struct {
		i   int
		d   time.Duration
		err error
	}
	results := make([]result, burst)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			takeCtx, cancel := context.WithTimeout(ctx, takeWait)
			defer cancel()
			t := time.Now()
			_, err := pool.Take(takeCtx)
			results[i] = result{i: i, d: time.Since(t), err: err}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	var ok, fail int
	var maxOK time.Duration
	for _, r := range results {
		if r.err != nil {
			fail++
			fmt.Printf("  take#%d FAIL %s err=%v\n", r.i+1, r.d.Round(time.Millisecond), r.err)
			continue
		}
		ok++
		if r.d > maxOK {
			maxOK = r.d
		}
		fmt.Printf("  take#%d OK %s\n", r.i+1, r.d.Round(time.Millisecond))
	}
	fills, takes, errs, expired = pool.Stats()
	fmt.Printf("  summary wall=%s ok=%d fail=%d max_ok=%s ready=%d fills=%d takes=%d errors=%d expired=%d chrome_procs=%d\n",
		wall.Round(time.Millisecond), ok, fail, maxOK.Round(time.Millisecond),
		pool.Ready(), fills, takes, errs, expired, countChromeProcs())
	return nil
}

func timedExtract(ctx context.Context, b *captcha.Browser) (string, time.Duration, error) {
	t0 := time.Now()
	tok, err := b.Extract(ctx)
	return tok, time.Since(t0).Round(time.Millisecond), err
}

func parseInts(csv string) ([]int, error) {
	parts := strings.Split(csv, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
		if n < 1 {
			return nil, fmt.Errorf("worker count must be >= 1")
		}
		out = append(out, n)
	}
	return out, nil
}

func countChromeProcs() int {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return -1
	}
	// Count Chromium/Chrome headless processes started by this user (best-effort).
	out, err := exec.Command("ps", "-A", "-o", "comm=").Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		c := strings.TrimSpace(line)
		lc := strings.ToLower(c)
		if strings.Contains(lc, "chrom") {
			n++
		}
	}
	return n
}
