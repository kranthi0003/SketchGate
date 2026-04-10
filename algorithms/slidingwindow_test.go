package algorithms

import (
	"testing"
	"time"
)

func TestSlidingWindowLog_Basic(t *testing.T) {
	sw := NewSlidingWindowLog(time.Second, 3)

	// First 3 requests should be allowed
	for i := 0; i < 3; i++ {
		allowed, remaining, _ := sw.Allow("user1")
		if !allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
		expected := uint64(2 - i)
		if remaining != expected {
			t.Errorf("request %d: expected remaining %d, got %d", i+1, expected, remaining)
		}
	}

	// 4th request should be denied
	allowed, _, _ := sw.Allow("user1")
	if allowed {
		t.Error("4th request should be denied")
	}
}

func TestSlidingWindowLog_WindowExpiry(t *testing.T) {
	sw := NewSlidingWindowLog(100*time.Millisecond, 2)

	now := time.Now()

	sw.AllowAt("user1", now)
	sw.AllowAt("user1", now.Add(10*time.Millisecond))

	// Should be denied within window
	allowed, _, _ := sw.AllowAt("user1", now.Add(50*time.Millisecond))
	if allowed {
		t.Error("should be denied within window")
	}

	// Should be allowed after window expires
	allowed, _, _ = sw.AllowAt("user1", now.Add(110*time.Millisecond))
	if !allowed {
		t.Error("should be allowed after window expires")
	}
}

func TestSlidingWindowLog_IndependentKeys(t *testing.T) {
	sw := NewSlidingWindowLog(time.Second, 1)

	allowed1, _, _ := sw.Allow("user1")
	allowed2, _, _ := sw.Allow("user2")

	if !allowed1 || !allowed2 {
		t.Error("different users should have independent limits")
	}

	// Both should now be denied
	denied1, _, _ := sw.Allow("user1")
	denied2, _, _ := sw.Allow("user2")
	if denied1 || denied2 {
		t.Error("both users should be denied after exceeding limit")
	}
}

func TestSlidingWindowLog_Reset(t *testing.T) {
	sw := NewSlidingWindowLog(time.Second, 1)
	sw.Allow("user1")

	sw.Reset("user1")

	allowed, _, _ := sw.Allow("user1")
	if !allowed {
		t.Error("should be allowed after reset")
	}
}

func TestSlidingWindowLog_Count(t *testing.T) {
	sw := NewSlidingWindowLog(time.Second, 10)

	sw.Allow("user1")
	sw.Allow("user1")
	sw.Allow("user1")

	count := sw.Count("user1")
	if count != 3 {
		t.Errorf("expected count 3, got %d", count)
	}
}
