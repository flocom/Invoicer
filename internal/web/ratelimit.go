package web

import (
	"crypto/sha256"
	"net"
	"sync"
	"time"
)

// limiter is a small in-memory token bucket keyed by arbitrary strings
// (IP address, e-mail, …). Keys are hashed so attacker-controlled input
// cannot inflate memory, and the table size is capped.
type limiter struct {
	mu      sync.Mutex
	buckets map[[16]byte]*bucket
	last    time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

const maxBuckets = 50000

func newLimiter() *limiter { return &limiter{buckets: map[[16]byte]*bucket{}} }

func limiterKey(s string) [16]byte {
	sum := sha256.Sum256([]byte(s))
	var k [16]byte
	copy(k[:], sum[:16])
	return k
}

func (l *limiter) gc(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.at) > time.Hour {
			delete(l.buckets, k)
		}
	}
	l.last = now
}

// allow consumes one token from key; burst tokens refill over `per`.
func (l *limiter) allow(key string, burst int, per time.Duration) bool {
	return l.take(key, burst, per, 1)
}

// peek reports whether a token is available without consuming it.
func (l *limiter) peek(key string, burst int, per time.Duration) bool {
	return l.take(key, burst, per, 0)
}

func (l *limiter) take(key string, burst int, per time.Duration, n float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) > 10*time.Minute {
		l.gc(now)
	}
	k := limiterKey(key)
	b, ok := l.buckets[k]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			l.gc(now)
			for victim := range l.buckets { // evict an arbitrary entry
				if len(l.buckets) < maxBuckets {
					break
				}
				delete(l.buckets, victim)
			}
		}
		b = &bucket{tokens: float64(burst), at: now}
		l.buckets[k] = b
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
	b.tokens -= n
	return true
}

// ipKey groups IPv6 clients by /64 (one subscriber) for rate limiting.
func ipKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if p.To4() != nil {
		return p.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
