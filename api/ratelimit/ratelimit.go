package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"clipfeed/httputil"
)

// RateLimiter implements a per-IP token bucket rate limiter.
// No external dependencies -- suitable for a single-instance deployment.
type RateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*bucket
	rate     int           // tokens per window
	window   time.Duration // refill window
}

type bucket struct {
	tokens    int
	lastReset time.Time
}

// New creates a new RateLimiter with the given rate and window.
func New(rate int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
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

func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-2 * rl.window)
	for ip, b := range rl.visitors {
		if b.lastReset.Before(cutoff) {
			delete(rl.visitors, ip)
		}
	}
}

// Allow returns true if the given IP is within the rate limit.
func (rl *RateLimiter) Allow(ip string) bool {
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

// trustedCIDRs are Docker/loopback networks whose proxy headers we trust.
var trustedCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",    // loopback
		"10.0.0.0/8",     // Docker default bridge & overlay
		"172.16.0.0/12",  // Docker default bridge range
		"192.168.0.0/16", // common local networks
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
	}
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

func isTrustedProxy(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, cidr := range trustedCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP extracts the real client IP for rate limiting.
// Only trusts X-Real-IP / X-Forwarded-For when the request comes from a
// known proxy (Docker internal network or loopback). Direct connections
// from the internet use RemoteAddr, preventing header-spoofed bypasses.
func ClientIP(r *http.Request) string {
	if isTrustedProxy(r.RemoteAddr) {
		if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
			return strings.TrimSpace(realIP)
		}
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// Only trust the first IP (set by the outermost proxy).
			if idx := strings.IndexByte(forwarded, ','); idx != -1 {
				return strings.TrimSpace(forwarded[:idx])
			}
			return strings.TrimSpace(forwarded)
		}
	}
	// Strip port from RemoteAddr for direct connections.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Middleware returns HTTP 429 when the per-IP rate is exceeded.
func Middleware(rl *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r)
			if !rl.Allow(ip) {
				w.Header().Set("Retry-After", "60")
				httputil.WriteJSON(w, 429, map[string]string{"error": "too many requests"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
