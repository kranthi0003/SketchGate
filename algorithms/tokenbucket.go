package algorithms

import (
	"sync"
	"time"
)

// TokenBucket implements a hierarchical token bucket for tiered burst management.
// Each tier has its own bucket with configurable rate and burst capacity.
type TokenBucket struct {
	mu       sync.Mutex
	rate     float64   // tokens added per second
	burst    uint64    // max tokens (bucket capacity)
	tokens   float64   // current token count
	lastFill time.Time // last time tokens were added
}

// NewTokenBucket creates a bucket with a fill rate (tokens/sec) and burst capacity.
func NewTokenBucket(rate float64, burst uint64) *TokenBucket {
	return &TokenBucket{
		rate:     rate,
		burst:    burst,
		tokens:   float64(burst), // start full
		lastFill: time.Now(),
	}
}

// Allow consumes one token. Returns true if allowed, plus the remaining tokens.
func (tb *TokenBucket) Allow() (bool, uint64) {
	return tb.AllowN(1)
}

// AllowN consumes n tokens. Returns true if enough tokens are available.
func (tb *TokenBucket) AllowN(n uint64) (bool, uint64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refill()

	if tb.tokens < float64(n) {
		return false, uint64(tb.tokens)
	}

	tb.tokens -= float64(n)
	return true, uint64(tb.tokens)
}

// Tokens returns the current available tokens.
func (tb *TokenBucket) Tokens() uint64 {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.refill()
	return uint64(tb.tokens)
}

func (tb *TokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(tb.lastFill).Seconds()
	tb.tokens += elapsed * tb.rate
	if tb.tokens > float64(tb.burst) {
		tb.tokens = float64(tb.burst)
	}
	tb.lastFill = now
}

// HierarchicalTokenBucket manages multiple tiers of token buckets.
// A request must pass through ALL tiers to be allowed (strictest wins).
type HierarchicalTokenBucket struct {
	mu    sync.RWMutex
	tiers map[string]*TierConfig
}

// TierConfig defines a rate-limiting tier.
type TierConfig struct {
	Name    string
	Rate    float64 // tokens per second
	Burst   uint64  // max burst capacity
	buckets map[string]*TokenBucket
	mu      sync.Mutex
}

// NewHierarchicalTokenBucket creates a multi-tier rate limiter.
func NewHierarchicalTokenBucket() *HierarchicalTokenBucket {
	return &HierarchicalTokenBucket{
		tiers: make(map[string]*TierConfig),
	}
}

// AddTier registers a new rate-limiting tier.
func (htb *HierarchicalTokenBucket) AddTier(name string, rate float64, burst uint64) {
	htb.mu.Lock()
	defer htb.mu.Unlock()
	htb.tiers[name] = &TierConfig{
		Name:    name,
		Rate:    rate,
		Burst:   burst,
		buckets: make(map[string]*TokenBucket),
	}
}

// Allow checks if a request from key passes ALL tiers.
// Returns (allowed, map of tier -> remaining tokens).
func (htb *HierarchicalTokenBucket) Allow(key string) (bool, map[string]uint64) {
	return htb.AllowN(key, 1)
}

// AllowN checks if n tokens can be consumed across all tiers.
func (htb *HierarchicalTokenBucket) AllowN(key string, n uint64) (bool, map[string]uint64) {
	htb.mu.RLock()
	defer htb.mu.RUnlock()

	remaining := make(map[string]uint64, len(htb.tiers))

	// First pass: check all tiers have enough tokens (don't consume yet)
	for name, tier := range htb.tiers {
		bucket := tier.getOrCreate(key)
		tokens := bucket.Tokens()
		remaining[name] = tokens
		if tokens < n {
			return false, remaining
		}
	}

	// Second pass: consume tokens from all tiers
	for name, tier := range htb.tiers {
		bucket := tier.getOrCreate(key)
		allowed, rem := bucket.AllowN(n)
		remaining[name] = rem
		if !allowed {
			return false, remaining
		}
	}

	return true, remaining
}

// TierNames returns the names of all configured tiers.
func (htb *HierarchicalTokenBucket) TierNames() []string {
	htb.mu.RLock()
	defer htb.mu.RUnlock()
	names := make([]string, 0, len(htb.tiers))
	for name := range htb.tiers {
		names = append(names, name)
	}
	return names
}

func (tc *TierConfig) getOrCreate(key string) *TokenBucket {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if b, ok := tc.buckets[key]; ok {
		return b
	}
	b := NewTokenBucket(tc.Rate, tc.Burst)
	tc.buckets[key] = b
	return b
}
