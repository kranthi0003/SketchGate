package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Lua script for atomic sliding window rate limiting.
// KEYS[1] = rate limit key (e.g., "ratelimit:user1")
// ARGV[1] = window size in milliseconds
// ARGV[2] = max requests allowed
// ARGV[3] = current timestamp in milliseconds
//
// Returns: [allowed (0/1), remaining, reset_at_ms]
var slidingWindowScript = goredis.NewScript(`
local key = KEYS[1]
local window = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local cutoff = now - window

-- Remove expired entries
redis.call('ZREMRANGEBYSCORE', key, '-inf', cutoff)

-- Count current entries
local count = redis.call('ZCARD', key)

if count >= limit then
    -- Get the oldest entry to calculate reset time
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    local reset_at = now + window
    if #oldest > 0 then
        reset_at = tonumber(oldest[2]) + window
    end
    return {0, 0, reset_at}
end

-- Add current request
redis.call('ZADD', key, now, now .. ':' .. math.random(1000000))

-- Set expiry on the key
redis.call('PEXPIRE', key, window)

local remaining = limit - count - 1
local reset_at = now + window
return {1, remaining, reset_at}
`)

// SlidingWindowLimiter provides distributed sliding window rate limiting via Redis.
type SlidingWindowLimiter struct {
	client *Client
	prefix string
}

// RateLimitResult contains the outcome of a rate limit check.
type RateLimitResult struct {
	Allowed   bool
	Remaining int64
	ResetAt   time.Time
}

// NewSlidingWindowLimiter creates a Redis-backed sliding window limiter.
func NewSlidingWindowLimiter(client *Client, prefix string) *SlidingWindowLimiter {
	if prefix == "" {
		prefix = "sketchgate:ratelimit"
	}
	return &SlidingWindowLimiter{client: client, prefix: prefix}
}

// Allow checks if a request from key is within the rate limit.
func (sw *SlidingWindowLimiter) Allow(ctx context.Context, key string, window time.Duration, limit int64) (*RateLimitResult, error) {
	redisKey := fmt.Sprintf("%s:%s", sw.prefix, key)
	nowMs := time.Now().UnixMilli()
	windowMs := window.Milliseconds()

	result, err := slidingWindowScript.Run(ctx, sw.client.rdb,
		[]string{redisKey},
		windowMs, limit, nowMs,
	).Int64Slice()
	if err != nil {
		return nil, fmt.Errorf("sliding window script failed: %w", err)
	}

	return &RateLimitResult{
		Allowed:   result[0] == 1,
		Remaining: result[1],
		ResetAt:   time.UnixMilli(result[2]),
	}, nil
}

// Count returns the current request count for a key.
func (sw *SlidingWindowLimiter) Count(ctx context.Context, key string, window time.Duration) (int64, error) {
	redisKey := fmt.Sprintf("%s:%s", sw.prefix, key)
	cutoff := float64(time.Now().Add(-window).UnixMilli())

	count, err := sw.client.rdb.ZCount(ctx, redisKey, fmt.Sprintf("%f", cutoff), "+inf").Result()
	if err != nil {
		return 0, fmt.Errorf("count failed: %w", err)
	}
	return count, nil
}

// Reset removes all rate limit entries for a key.
func (sw *SlidingWindowLimiter) Reset(ctx context.Context, key string) error {
	redisKey := fmt.Sprintf("%s:%s", sw.prefix, key)
	return sw.client.rdb.Del(ctx, redisKey).Err()
}
