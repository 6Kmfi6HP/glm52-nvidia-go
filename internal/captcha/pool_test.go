package captcha

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolTakeBlocksUntilFilled(t *testing.T) {
	var n atomic.Int32
	extract := func(ctx context.Context) (string, error) {
		i := n.Add(1)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		return fmt.Sprintf("tok-%d", i), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewPool(ctx, extract, PoolConfig{Size: 2, Workers: 1})
	defer p.Close()

	takeCtx, takeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer takeCancel()

	tok, err := p.Take(takeCtx)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}

	fills, takes, errs, expired := p.Stats()
	if takes != 1 {
		t.Fatalf("takes=%d want 1", takes)
	}
	if fills < 1 {
		t.Fatalf("fills=%d want >=1", fills)
	}
	if errs != 0 {
		t.Fatalf("errors=%d", errs)
	}
	if expired != 0 {
		t.Fatalf("expired=%d", expired)
	}
}

func TestPoolDiscardsExpired(t *testing.T) {
	var n atomic.Int32
	extract := func(ctx context.Context) (string, error) {
		i := n.Add(1)
		return fmt.Sprintf("tok-%d", i), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewPool(ctx, extract, PoolConfig{Size: 1, Workers: 1, TTL: 30 * time.Millisecond})
	defer p.Close()

	// Wait until one token is buffered, then let it expire.
	deadline := time.Now().Add(2 * time.Second)
	for p.Ready() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Ready() < 1 {
		t.Fatal("pool never filled")
	}
	time.Sleep(40 * time.Millisecond)

	takeCtx, takeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer takeCancel()
	tok, err := p.Take(takeCtx)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}

	_, _, _, expired := p.Stats()
	if expired < 1 {
		t.Fatalf("expired=%d want >=1", expired)
	}
}

func TestPoolClosed(t *testing.T) {
	extract := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := NewPool(ctx, extract, PoolConfig{Size: 1, Workers: 1})
	p.Close()
	cancel()

	_, err := p.Take(context.Background())
	if err == nil {
		t.Fatal("expected error after Close")
	}
}

// Idle: channel fills, tokens age past TTL, reaper drains them so workers
// can refill — without a Take. This is the "chat then wait" failure mode.
func TestPoolReapsStaleDuringIdle(t *testing.T) {
	var n atomic.Int32
	extract := func(ctx context.Context) (string, error) {
		return fmt.Sprintf("tok-%d", n.Add(1)), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewPool(ctx, extract, PoolConfig{Size: 2, Workers: 1, TTL: 200 * time.Millisecond})
	defer p.Close()

	deadline := time.Now().Add(2 * time.Second)
	for p.Ready() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Ready() < 2 {
		t.Fatal("pool never filled")
	}
	fillsBefore, _, _, _ := p.Stats()

	// Past hard TTL and several reaper ticks (ttl/4, min 100ms).
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fills, _, _, expired := p.Stats()
		if expired >= 1 && fills > fillsBefore {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fillsAfter, _, _, expired := p.Stats()
	t.Fatalf("idle reap did not refresh: fills %d→%d expired=%d ready=%d",
		fillsBefore, fillsAfter, expired, p.Ready())
}

// Fresh tokens must not be evacuated by the reaper — that races workers into
// idle mint churn (fills climbing while takes stay 0).
func TestPoolReaperNoChurnWhileFresh(t *testing.T) {
	var n atomic.Int32
	extract := func(ctx context.Context) (string, error) {
		return fmt.Sprintf("tok-%d", n.Add(1)), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewPool(ctx, extract, PoolConfig{Size: 2, Workers: 1, TTL: 5 * time.Second})
	defer p.Close()

	deadline := time.Now().Add(2 * time.Second)
	for p.Ready() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Ready() < 2 {
		t.Fatal("pool never filled")
	}
	fillsBefore, _, _, _ := p.Stats()

	// Several reaper ticks (ttl/4 = 1.25s, capped logic → 1.25s) while still fresh.
	time.Sleep(800 * time.Millisecond)
	_ = p.discardStale() // force one pass
	time.Sleep(200 * time.Millisecond)

	fillsAfter, takes, _, expired := p.Stats()
	if takes != 0 {
		t.Fatalf("takes=%d want 0", takes)
	}
	if expired != 0 {
		t.Fatalf("expired=%d want 0", expired)
	}
	if fillsAfter != fillsBefore {
		t.Fatalf("idle churn: fills %d→%d (reaper must not wake mint while fresh)", fillsBefore, fillsAfter)
	}
	if p.Ready() != 2 {
		t.Fatalf("ready=%d want 2", p.Ready())
	}
}

func TestPoolWorkersReserveCapacityBeforeExtract(t *testing.T) {
	var started atomic.Int32
	release := make(chan struct{})
	extract := func(ctx context.Context) (string, error) {
		n := started.Add(1)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return fmt.Sprintf("tok-%d", n), nil
		}
	}

	p := NewPool(context.Background(), extract, PoolConfig{Size: 1, Workers: 4})
	defer p.Close()
	deadline := time.Now().Add(time.Second)
	for started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Give every worker a chance to race for the single slot.
	time.Sleep(30 * time.Millisecond)
	if got := started.Load(); got != 1 {
		t.Fatalf("extracts started=%d want 1 for one reserved slot", got)
	}
	close(release)
}

func TestPoolTakeWakesImmediatelyOnFill(t *testing.T) {
	release := make(chan struct{})
	extract := func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "tok", nil
		}
	}

	p := NewPool(context.Background(), extract, PoolConfig{Size: 1, Workers: 1})
	defer p.Close()
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = p.Take(context.Background())
		done <- time.Since(start)
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	close(release)
	select {
	case <-done:
		if delay := time.Since(start); delay > 50*time.Millisecond {
			t.Fatalf("Take wake delay=%s want <=50ms", delay)
		}
	case <-time.After(time.Second):
		t.Fatal("Take did not wake after fill")
	}
}

func TestPoolTakeCanceledDoesNotConsumeReadyToken(t *testing.T) {
	p := NewPool(context.Background(), func(context.Context) (string, error) {
		return "tok", nil
	}, PoolConfig{Size: 1, Workers: 1})
	defer p.Close()
	deadline := time.Now().Add(time.Second)
	for p.Ready() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Take(ctx); err == nil {
		t.Fatal("Take with canceled context succeeded")
	}
	if got := p.Ready(); got != 1 {
		t.Fatalf("ready=%d want 1 after canceled Take", got)
	}
}

func TestPoolTakeAfterCloseDoesNotConsumeReadyToken(t *testing.T) {
	p := NewPool(context.Background(), func(context.Context) (string, error) {
		return "tok", nil
	}, PoolConfig{Size: 1, Workers: 1})
	deadline := time.Now().Add(time.Second)
	for p.Ready() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	p.Close()
	if _, err := p.Take(context.Background()); err == nil {
		t.Fatal("Take after Close succeeded")
	}
	if got := p.Ready(); got != 1 {
		t.Fatalf("ready=%d want 1 after closed Take", got)
	}
}
