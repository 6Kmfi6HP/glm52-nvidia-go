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
