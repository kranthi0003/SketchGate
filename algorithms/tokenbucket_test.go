package algorithms

import (
	"testing"
	"time"
)

func TestTokenBucket_Basic(t *testing.T) {
	tb := NewTokenBucket(10, 5) // 10 tokens/sec, burst of 5

	// Should allow 5 requests (starts full)
	for i := 0; i < 5; i++ {
		allowed, _ := tb.Allow()
		if !allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	// 6th should be denied
	allowed, _ := tb.Allow()
	if allowed {
		t.Error("6th request should be denied (bucket empty)")
	}
}

func TestTokenBucket_Refill(t *testing.T) {
	tb := NewTokenBucket(100, 5) // 100 tokens/sec, burst of 5

	// Drain the bucket
	for i := 0; i < 5; i++ {
		tb.Allow()
	}

	// Wait for refill
	time.Sleep(60 * time.Millisecond)

	// Should have ~6 tokens refilled, capped at burst(5)
	tokens := tb.Tokens()
	if tokens < 3 {
		t.Errorf("expected at least 3 tokens after refill, got %d", tokens)
	}
}

func TestTokenBucket_AllowN(t *testing.T) {
	tb := NewTokenBucket(10, 10)

	allowed, remaining := tb.AllowN(7)
	if !allowed {
		t.Error("should allow 7 tokens")
	}
	if remaining != 3 {
		t.Errorf("expected 3 remaining, got %d", remaining)
	}

	// Should deny 5 more (only 3 left)
	allowed, _ = tb.AllowN(5)
	if allowed {
		t.Error("should deny, not enough tokens")
	}
}

func TestHierarchicalTokenBucket_MultiTier(t *testing.T) {
	htb := NewHierarchicalTokenBucket()

	// Tier 1: per-second (strict) — 5 req/sec, burst 5
	htb.AddTier("per-second", 5, 5)
	// Tier 2: per-minute (lenient) — 100 req/min, burst 100
	htb.AddTier("per-minute", 1.67, 100)

	// First 5 should pass (limited by per-second tier)
	for i := 0; i < 5; i++ {
		allowed, _ := htb.Allow("user1")
		if !allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	// 6th should be denied by per-second tier
	allowed, remaining := htb.Allow("user1")
	if allowed {
		t.Error("6th request should be denied by per-second tier")
	}
	_ = remaining
}

func TestHierarchicalTokenBucket_IndependentKeys(t *testing.T) {
	htb := NewHierarchicalTokenBucket()
	htb.AddTier("global", 10, 2)

	allowed1, _ := htb.Allow("user1")
	allowed2, _ := htb.Allow("user2")

	if !allowed1 || !allowed2 {
		t.Error("different users should have independent buckets")
	}
}

func TestHierarchicalTokenBucket_TierNames(t *testing.T) {
	htb := NewHierarchicalTokenBucket()
	htb.AddTier("per-second", 5, 5)
	htb.AddTier("per-minute", 100, 100)

	names := htb.TierNames()
	if len(names) != 2 {
		t.Errorf("expected 2 tiers, got %d", len(names))
	}
}
