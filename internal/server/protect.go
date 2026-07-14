package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxLimiterBuckets       = 4096
	maxDeliveryCacheEntries = 4096
	bucketIdleTTL           = 30 * time.Minute
)

// rateLimiter is a per-key token bucket with TTL eviction and capacity cap.
type rateLimiter struct {
	mu       sync.Mutex
	capacity float64
	refill   float64 // tokens per second
	buckets  map[string]*bucket
	maxKeys  int
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(capacity int, refillPerSecond float64) *rateLimiter {
	if capacity <= 0 {
		capacity = 30
	}
	if refillPerSecond <= 0 {
		refillPerSecond = float64(capacity) / 60
	}
	return &rateLimiter{
		capacity: float64(capacity),
		refill:   refillPerSecond,
		buckets:  make(map[string]*bucket),
		maxKeys:  maxLimiterBuckets,
	}
}

func (r *rateLimiter) allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purge(now)

	b, ok := r.buckets[key]
	if !ok {
		if len(r.buckets) >= r.maxKeys {
			r.evictOne()
		}
		r.buckets[key] = &bucket{tokens: r.capacity - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * r.refill
		if b.tokens > r.capacity {
			b.tokens = r.capacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *rateLimiter) purge(now time.Time) {
	for id, b := range r.buckets {
		if now.Sub(b.last) > bucketIdleTTL {
			delete(r.buckets, id)
		}
	}
}

func (r *rateLimiter) evictOne() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, b := range r.buckets {
		if first || b.last.Before(oldest) {
			oldestKey = k
			oldest = b.last
			first = false
		}
	}
	if oldestKey != "" {
		delete(r.buckets, oldestKey)
	}
}

func (r *rateLimiter) lenBuckets() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

// deliveryCache remembers delivery IDs for a TTL window to block replays.
type deliveryCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxKeys int
	entries map[string]time.Time
}

func newDeliveryCache(ttl time.Duration) *deliveryCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &deliveryCache{
		ttl:     ttl,
		maxKeys: maxDeliveryCacheEntries,
		entries: make(map[string]time.Time),
	}
}

func (c *deliveryCache) claim(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purge(now)
	if exp, ok := c.entries[id]; ok && now.Before(exp) {
		return false
	}
	if len(c.entries) >= c.maxKeys {
		c.evictOne()
	}
	c.entries[id] = now.Add(c.ttl)
	return true
}

func (c *deliveryCache) purge(now time.Time) {
	for id, exp := range c.entries {
		if !now.Before(exp) {
			delete(c.entries, id)
		}
	}
}

func (c *deliveryCache) evictOne() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, exp := range c.entries {
		if first || exp.Before(oldest) {
			oldestKey = k
			oldest = exp
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

func (c *deliveryCache) lenEntries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

type trustedNets struct {
	nets []*net.IPNet
	ips  []net.IP
}

func parseTrustedProxies(cidrs []string) *trustedNets {
	t := &trustedNets{}
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(raw); err == nil {
			t.nets = append(t.nets, n)
			continue
		}
		if ip := net.ParseIP(raw); ip != nil {
			t.ips = append(t.ips, ip)
		}
	}
	return t
}

func (t *trustedNets) contains(ip net.IP) bool {
	if t == nil || ip == nil {
		return false
	}
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	for _, tip := range t.ips {
		if tip.Equal(ip) {
			return true
		}
	}
	return false
}

// clientIP resolves the client address. Forwarding headers are only honored when
// RemoteAddr belongs to an explicitly configured trusted proxy.
func clientIP(r *http.Request, trusted *trustedNets) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	remote := r.RemoteAddr
	if err == nil {
		remote = host
	}
	remoteIP := net.ParseIP(remote)
	if remoteIP == nil || !trusted.contains(remoteIP) {
		return remote
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// rightmost-untrusted: walk from right; first hop not in trusted is client
		for i := len(parts) - 1; i >= 0; i-- {
			cand := strings.TrimSpace(parts[i])
			ip := net.ParseIP(cand)
			if ip == nil {
				continue
			}
			if !trusted.contains(ip) {
				return cand
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if ip := net.ParseIP(xr); ip != nil {
			return xr
		}
	}
	return remote
}
