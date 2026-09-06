package resolver

import (
	"time"

	"github.com/dgraph-io/ristretto/v2"
)

// Ristretto is the in-process tier. TinyLFU admission is the reason this is
// Ristretto rather than a plain LRU: under a skewed workload a scan of cold
// keys would evict the hot set from an LRU, while TinyLFU refuses to admit
// keys that are not worth more than what they would displace.
type Ristretto struct {
	cache *ristretto.Cache[string, string]
}

// NewRistretto builds the local cache sized for maxItems entries.
func NewRistretto(maxItems int64) (*Ristretto, error) {
	c, err := ristretto.NewCache(&ristretto.Config[string, string]{
		// Ristretto's own guidance: roughly 10x the expected item count, so
		// the admission filter has enough counters to estimate frequency.
		NumCounters: maxItems * 10,
		MaxCost:     maxItems,
		BufferItems: 64,
		// Every entry costs 1, so MaxCost is simply an item count.
		Cost: func(string) int64 { return 1 },
	})
	if err != nil {
		return nil, err
	}
	return &Ristretto{cache: c}, nil
}

// Get returns a cached destination.
func (r *Ristretto) Get(code string) (string, bool) {
	return r.cache.Get(code)
}

// Set stores a destination. Ristretto admits asynchronously, so a Set is not
// guaranteed to be visible to the next Get -- that is a deliberate trade for
// the write throughput, and a missed admission only costs one extra lookup.
func (r *Ristretto) Set(code, destination string, ttl time.Duration) {
	r.cache.SetWithTTL(code, destination, 1, ttl)
}

// Close releases the cache.
func (r *Ristretto) Close() { r.cache.Close() }
