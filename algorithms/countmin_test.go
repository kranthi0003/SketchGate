package algorithms

import (
	"fmt"
	"sync"
	"testing"
)

func TestCountMinSketch_Basic(t *testing.T) {
	cms := NewCountMinSketch(0.001, 0.01)

	cms.Increment("user1", 10)
	cms.Increment("user2", 5)
	cms.Increment("user1", 3)

	est1 := cms.Estimate("user1")
	if est1 < 13 {
		t.Errorf("expected estimate >= 13 for user1, got %d", est1)
	}

	est2 := cms.Estimate("user2")
	if est2 < 5 {
		t.Errorf("expected estimate >= 5 for user2, got %d", est2)
	}

	// Unknown key should return 0
	est3 := cms.Estimate("unknown")
	if est3 != 0 {
		t.Errorf("expected 0 for unknown key, got %d", est3)
	}
}

func TestCountMinSketch_NeverUndercounts(t *testing.T) {
	cms := NewCountMinSketch(0.01, 0.01)

	counts := map[string]uint64{
		"alpha": 100,
		"beta":  200,
		"gamma": 50,
	}

	for key, count := range counts {
		cms.Increment(key, count)
	}

	for key, exact := range counts {
		est := cms.Estimate(key)
		if est < exact {
			t.Errorf("undercount for %s: exact=%d, estimate=%d", key, exact, est)
		}
	}
}

func TestCountMinSketch_Total(t *testing.T) {
	cms := NewCountMinSketch(0.001, 0.01)
	cms.Increment("a", 10)
	cms.Increment("b", 20)

	if cms.Total() != 30 {
		t.Errorf("expected total 30, got %d", cms.Total())
	}
}

func TestCountMinSketch_Reset(t *testing.T) {
	cms := NewCountMinSketch(0.001, 0.01)
	cms.Increment("user1", 100)
	cms.Reset()

	if cms.Estimate("user1") != 0 {
		t.Error("expected 0 after reset")
	}
	if cms.Total() != 0 {
		t.Error("expected total 0 after reset")
	}
}

func TestCountMinSketch_Concurrent(t *testing.T) {
	cms := NewCountMinSketch(0.001, 0.01)
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("user%d", n%10)
			cms.Increment(key, 1)
			cms.Estimate(key)
		}(i)
	}

	wg.Wait()

	if cms.Total() != 100 {
		t.Errorf("expected total 100, got %d", cms.Total())
	}
}

func BenchmarkCountMinSketch_Increment(b *testing.B) {
	cms := NewCountMinSketch(0.001, 0.01)
	for i := 0; i < b.N; i++ {
		cms.Increment(fmt.Sprintf("user%d", i%1000), 1)
	}
}

func BenchmarkCountMinSketch_Estimate(b *testing.B) {
	cms := NewCountMinSketch(0.001, 0.01)
	for i := 0; i < 1000; i++ {
		cms.Increment(fmt.Sprintf("user%d", i), uint64(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cms.Estimate(fmt.Sprintf("user%d", i%1000))
	}
}
