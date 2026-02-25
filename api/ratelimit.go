package main

import (
	"net/http"
	"sync"
	"time"
)

// rateLimiter implements a per-IP token bucket rate limiter.
// No external dependencies — suitable for a single-instance deployment.
type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*bucket
	rate     int           // tokens per window
	window   time.Duration // refill window
}

type bucket struct {
	tokens    int
	lastReset time.Time
}

func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*bucket),
		rate:     rate,
		window:   window,
	}
	// Evict stale entries every 5 minutes to prevent unbounded map growth.
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.cleanup()
		}
	}()
	return rl
}

func (rl *rateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-2 * rl.window)
	for ip, b := range rl.visitors {
		if b.lastReset.Before(cutoff) {
			delete(rl.visitors, ip)
		}
	}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.visitors[ip]
	if !exists || now.Sub(b.lastReset) >= rl.window {
		rl.visitors[ip] = &bucket{tokens: rl.rate - 1, lastReset: now}
		return true
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// rateLimitMiddleware returns HTTP 429 when the per-IP rate is exceeded.
func rateLimitMiddleware(rl *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr
			if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
				ip = forwarded
			}
			if !rl.allow(ip) {
				w.Header().Set("Retry-After", "60")
				writeJSON(w, 429, map[string]string{"error": "too many requests"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
