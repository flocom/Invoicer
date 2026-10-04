package web

import (
	"sync"
	"time"
)

// limiter is a small in-memory token bucket keyed by arbitrary strings
// (IP address, e-mail, …).
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

// allow consumes one token from key; burst tokens refill over `per`.
func (l *limiter) allow(key string, burst int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.at) > time.Hour {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), at: now}
		l.buckets[key] = b
	}
	rate := float64(burst) / per.Seconds()
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
