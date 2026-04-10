package algorithms

import (
	"sync"
	"time"
)

// SlidingWindowLog tracks request timestamps per key within a rolling window.
// Provides exact counting with O(1) amortized check time via periodic cleanup.
type SlidingWindowLog struct {
	mu       sync.RWMutex
	window   time.Duration
	limit    uint64
	logs     map[string][]time.Time
}

// NewSlidingWindowLog creates a rate limiter with the given window and max requests.
func NewSlidingWindowLog(window time.Duration, limit uint64) *SlidingWindowLog {
	return &SlidingWindowLog{
		window: window,
		limit:  limit,
		logs:   make(map[string][]time.Time),
	}
}

// Allow checks if a request from key is within the rate limit.
// Returns (allowed bool, remaining uint64, resetAt time.Time).
func (sw *SlidingWindowLog) Allow(key string) (bool, uint64, time.Time) {
	return sw.AllowAt(key, time.Now())
}

// AllowAt checks the rate limit at a specific timestamp (useful for testing).
func (sw *SlidingWindowLog) AllowAt(key string, now time.Time) (bool, uint64, time.Time) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	cutoff := now.Add(-sw.window)

	// Prune expired entries
	entries := sw.logs[key]
	start := 0
	for start < len(entries) && entries[start].Before(cutoff) {
		start++
	}
	entries = entries[start:]

	resetAt := now.Add(sw.window)
	if len(entries) > 0 {
		resetAt = entries[0].Add(sw.window)
	}

	count := uint64(len(entries))
	if count >= sw.limit {
		return false, 0, resetAt
	}

	// Record this request
	entries = append(entries, now)
	sw.logs[key] = entries

	remaining := sw.limit - count - 1
	return true, remaining, resetAt
}

// Count returns the current request count for a key within the window.
func (sw *SlidingWindowLog) Count(key string) uint64 {
	return sw.CountAt(key, time.Now())
}

// CountAt returns the count at a specific time.
func (sw *SlidingWindowLog) CountAt(key string, now time.Time) uint64 {
	sw.mu.RLock()
	defer sw.mu.RUnlock()

	cutoff := now.Add(-sw.window)
	entries := sw.logs[key]
	count := uint64(0)
	for _, t := range entries {
		if !t.Before(cutoff) {
			count++
		}
	}
	return count
}

// Reset removes all entries for a key.
func (sw *SlidingWindowLog) Reset(key string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	delete(sw.logs, key)
}

// ResetAll clears all keys.
func (sw *SlidingWindowLog) ResetAll() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.logs = make(map[string][]time.Time)
}

// Window returns the configured window duration.
func (sw *SlidingWindowLog) Window() time.Duration { return sw.window }

// Limit returns the max requests allowed per window.
func (sw *SlidingWindowLog) Limit() uint64 { return sw.limit }
