package algorithms

import (
	"hash"
	"hash/fnv"
	"math"
	"sync"
)

// CountMinSketch provides sub-linear space frequency estimation.
// Error bound ε with probability 1-δ using width = ⌈e/ε⌉, depth = ⌈ln(1/δ)⌉.
type CountMinSketch struct {
	mu     sync.RWMutex
	width  uint
	depth  uint
	matrix [][]uint64
	seeds  []uint64
	total  uint64
}

// NewCountMinSketch creates a sketch with the given error bound (epsilon)
// and failure probability (delta).
//   - epsilon: acceptable error rate (e.g., 0.001 for 0.1%)
//   - delta: failure probability (e.g., 0.01 for 1%)
func NewCountMinSketch(epsilon, delta float64) *CountMinSketch {
	width := uint(math.Ceil(math.E / epsilon))
	depth := uint(math.Ceil(math.Log(1.0 / delta)))
	return NewCountMinSketchWithSize(width, depth)
}

// NewCountMinSketchWithSize creates a sketch with explicit dimensions.
func NewCountMinSketchWithSize(width, depth uint) *CountMinSketch {
	matrix := make([][]uint64, depth)
	for i := range matrix {
		matrix[i] = make([]uint64, width)
	}

	seeds := make([]uint64, depth)
	for i := range seeds {
		seeds[i] = uint64(i) * 0x9E3779B97F4A7C15 // golden ratio hash spread
	}

	return &CountMinSketch{
		width:  width,
		depth:  depth,
		matrix: matrix,
		seeds:  seeds,
	}
}

// Increment adds count occurrences of key to the sketch.
func (cms *CountMinSketch) Increment(key string, count uint64) {
	cms.mu.Lock()
	defer cms.mu.Unlock()

	for i := uint(0); i < cms.depth; i++ {
		idx := cms.hash(key, i)
		cms.matrix[i][idx] += count
	}
	cms.total += count
}

// Estimate returns the estimated frequency of key (may overcount, never undercount).
func (cms *CountMinSketch) Estimate(key string) uint64 {
	cms.mu.RLock()
	defer cms.mu.RUnlock()

	var min uint64 = math.MaxUint64
	for i := uint(0); i < cms.depth; i++ {
		idx := cms.hash(key, i)
		if cms.matrix[i][idx] < min {
			min = cms.matrix[i][idx]
		}
	}
	return min
}

// Total returns the total count of all increments.
func (cms *CountMinSketch) Total() uint64 {
	cms.mu.RLock()
	defer cms.mu.RUnlock()
	return cms.total
}

// Reset clears all counters.
func (cms *CountMinSketch) Reset() {
	cms.mu.Lock()
	defer cms.mu.Unlock()
	for i := range cms.matrix {
		for j := range cms.matrix[i] {
			cms.matrix[i][j] = 0
		}
	}
	cms.total = 0
}

// Width returns the number of columns.
func (cms *CountMinSketch) Width() uint { return cms.width }

// Depth returns the number of hash functions (rows).
func (cms *CountMinSketch) Depth() uint { return cms.depth }

func (cms *CountMinSketch) hash(key string, row uint) uint {
	var h hash.Hash64 = fnv.New64a()
	// Seed the hash by mixing in the row-specific seed
	seed := cms.seeds[row]
	seedBytes := []byte{
		byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24),
		byte(seed >> 32), byte(seed >> 40), byte(seed >> 48), byte(seed >> 56),
	}
	h.Write(seedBytes)
	h.Write([]byte(key))
	return uint(h.Sum64() % uint64(cms.width))
}
