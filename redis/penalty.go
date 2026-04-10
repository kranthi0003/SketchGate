package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// Lua script for atomic penalty queue operations.
// Adds a user to the penalty queue with a score (violation count).
var penaltyAddScript = goredis.NewScript(`
local key = KEYS[1]
local member = ARGV[1]
local increment = tonumber(ARGV[2])

local newScore = redis.call('ZINCRBY', key, increment, member)
return newScore
`)

// PenaltyQueue manages a sorted set of penalized users in Redis.
// Users with higher violation counts are ranked higher.
type PenaltyQueue struct {
	client *Client
	key    string
}

// PenaltyEntry represents a user in the penalty queue.
type PenaltyEntry struct {
	Key        string
	Violations float64
}

// NewPenaltyQueue creates a Redis-backed penalty queue.
func NewPenaltyQueue(client *Client, key string) *PenaltyQueue {
	if key == "" {
		key = "sketchgate:penalties"
	}
	return &PenaltyQueue{client: client, key: key}
}

// Penalize increments the violation count for a user.
func (pq *PenaltyQueue) Penalize(ctx context.Context, userKey string, violations int64) (float64, error) {
	score, err := penaltyAddScript.Run(ctx, pq.client.rdb,
		[]string{pq.key}, userKey, violations,
	).Float64()
	if err != nil {
		return 0, fmt.Errorf("penalize failed: %w", err)
	}
	return score, nil
}

// TopOffenders returns the top N most penalized users.
func (pq *PenaltyQueue) TopOffenders(ctx context.Context, n int64) ([]PenaltyEntry, error) {
	results, err := pq.client.rdb.ZRevRangeWithScores(ctx, pq.key, 0, n-1).Result()
	if err != nil {
		return nil, fmt.Errorf("top offenders query failed: %w", err)
	}

	entries := make([]PenaltyEntry, len(results))
	for i, z := range results {
		entries[i] = PenaltyEntry{
			Key:        z.Member.(string),
			Violations: z.Score,
		}
	}
	return entries, nil
}

// IsPenalized checks if a user is in the penalty queue.
func (pq *PenaltyQueue) IsPenalized(ctx context.Context, userKey string) (bool, float64, error) {
	score, err := pq.client.rdb.ZScore(ctx, pq.key, userKey).Result()
	if err == goredis.Nil {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, score, nil
}

// Pardon removes a user from the penalty queue.
func (pq *PenaltyQueue) Pardon(ctx context.Context, userKey string) error {
	return pq.client.rdb.ZRem(ctx, pq.key, userKey).Err()
}

// Size returns the number of users in the penalty queue.
func (pq *PenaltyQueue) Size(ctx context.Context) (int64, error) {
	return pq.client.rdb.ZCard(ctx, pq.key).Result()
}

// Reset clears the entire penalty queue.
func (pq *PenaltyQueue) Reset(ctx context.Context) error {
	return pq.client.rdb.Del(ctx, pq.key).Err()
}
