package main

import (
	"context"
	"sync"
	"time"
)

// RateLimiter is a global token bucket shared by every connection, so the
// configured limit applies to the total download speed.
type RateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second, 0 = unlimited
	tokens float64
	last   time.Time
}

func (r *RateLimiter) SetRate(bytesPerSec int64) {
	r.mu.Lock()
	r.rate = float64(bytesPerSec)
	r.tokens = 0
	r.last = time.Now()
	r.mu.Unlock()
}

func (r *RateLimiter) Wait(ctx context.Context, n int) error {
	r.mu.Lock()
	if r.rate <= 0 {
		r.mu.Unlock()
		return nil
	}
	now := time.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.rate
	r.last = now
	if burst := r.rate / 4; r.tokens > burst {
		r.tokens = burst
	}
	r.tokens -= float64(n)
	var wait time.Duration
	if r.tokens < 0 {
		wait = time.Duration(-r.tokens / r.rate * float64(time.Second))
	}
	r.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
