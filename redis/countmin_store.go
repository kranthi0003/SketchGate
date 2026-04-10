package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// Lua script for atomic Count-Min Sketch increment in Redis.
// KEYS[1..depth] = hash keys for each row
// ARGV[1] = count to increment
// ARGV[2] = total key name
//
// Returns: array of new values for each row
var cmsIncrementScript = goredis.NewScript(`
local count = tonumber(ARGV[1])
local totalKey = ARGV[2]
local results = {}

for i, key in ipairs(KEYS) do
    local val = redis.call('INCRBY', key, count)
    table.insert(results, val)
end

redis.call('INCRBY', totalKey, count)
return results
`)

// CountMinSketchStore provides a Redis-backed Count-Min Sketch.
type CountMinSketchStore struct {
	client *Client
	prefix string
	width  uint
	depth  uint
	seeds  []uint64
}

// NewCountMinSketchStore creates a distributed CMS backed by Redis.
func NewCountMinSketchStore(client *Client, prefix string, width, depth uint) *CountMinSketchStore {
	seeds := make([]uint64, depth)
	for i := range seeds {
		seeds[i] = uint64(i) * 0x9E3779B97F4A7C15
	}
	if prefix == "" {
		prefix = "sketchgate:cms"
	}
	return &CountMinSketchStore{
		client: client,
		prefix: prefix,
		width:  width,
		depth:  depth,
		seeds:  seeds,
	}
}

// Increment atomically adds count to key across all hash rows.
func (cms *CountMinSketchStore) Increment(ctx context.Context, key string, count uint64) error {
	keys := cms.hashKeys(key)
	totalKey := fmt.Sprintf("%s:total", cms.prefix)

	_, err := cmsIncrementScript.Run(ctx, cms.client.rdb, keys, count, totalKey).Result()
	if err != nil {
		return fmt.Errorf("cms increment failed: %w", err)
	}
	return nil
}

// Estimate returns the minimum count across all hash rows (may overcount).
func (cms *CountMinSketchStore) Estimate(ctx context.Context, key string) (uint64, error) {
	keys := cms.hashKeys(key)

	pipe := cms.client.rdb.Pipeline()
	cmds := make([]*goredis.StringCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.Get(ctx, k)
	}
	_, err := pipe.Exec(ctx)
	if err != nil && err != goredis.Nil {
		return 0, fmt.Errorf("cms estimate failed: %w", err)
	}

	var min uint64 = ^uint64(0)
	for _, cmd := range cmds {
		val, err := cmd.Uint64()
		if err != nil {
			val = 0 // key doesn't exist yet
		}
		if val < min {
			min = val
		}
	}
	return min, nil
}

// Total returns the total count across all keys.
func (cms *CountMinSketchStore) Total(ctx context.Context) (uint64, error) {
	totalKey := fmt.Sprintf("%s:total", cms.prefix)
	val, err := cms.client.rdb.Get(ctx, totalKey).Uint64()
	if err == goredis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return val, nil
}

// Reset clears the entire sketch from Redis.
func (cms *CountMinSketchStore) Reset(ctx context.Context) error {
	pattern := fmt.Sprintf("%s:*", cms.prefix)
	iter := cms.client.rdb.Scan(ctx, 0, pattern, 100).Iterator()

	pipe := cms.client.rdb.Pipeline()
	for iter.Next(ctx) {
		pipe.Del(ctx, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return err
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (cms *CountMinSketchStore) hashKeys(key string) []string {
	keys := make([]string, cms.depth)
	for i := uint(0); i < cms.depth; i++ {
		idx := hashWithSeed(key, cms.seeds[i], cms.width)
		keys[i] = fmt.Sprintf("%s:row:%d:col:%d", cms.prefix, i, idx)
	}
	return keys
}

func hashWithSeed(key string, seed uint64, width uint) uint {
	// FNV-1a inspired hash with seed mixing
	h := seed ^ 0xcbf29ce484222325
	for _, b := range []byte(key) {
		h ^= uint64(b)
		h *= 0x100000001b3
	}
	return uint(h % uint64(width))
}
