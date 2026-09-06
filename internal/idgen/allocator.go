// Package idgen hands out link IDs without coordinating on the hot path.
package idgen

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// ClaimFunc reserves size IDs and returns the first one in the block. It must
// be atomic across processes -- the whole design rests on two instances never
// being handed overlapping blocks.
type ClaimFunc func(ctx context.Context, size uint64) (start uint64, err error)

// maxAttempts bounds the retry loop. Every attempt either yields an ID or
// consumes a block, so reaching this many means blocks are being drained
// faster than they can be claimed rather than that anything is stuck.
const maxAttempts = 1000

// block is one claimed range, immutable except for its cursor.
//
// Cursor and bounds live in the same object on purpose. An earlier version
// kept next and end as two separate atomics, which could tear: a reader could
// observe the new end alongside the old cursor and return an ID from the
// previous, exhausted range -- an ID that by then belonged to another
// instance's block. Swapping one pointer makes that window impossible.
type block struct {
	cursor atomic.Uint64
	start  uint64
	end    uint64
}

// Allocator hands out IDs from a locally held block, claiming a new block from
// the database only when the current one runs out.
//
// The alternatives it replaces (PLAN.md 5.4): a random ID with a collision
// check costs a database round trip per creation and degrades as the keyspace
// fills; a shared auto-increment makes every creation contend on one row. This
// costs one write per BlockSize creations and nothing in between.
//
// IDs are not dense. Callers that race past the end of a block burn the IDs
// they drew, and an instance that restarts abandons the rest of its block.
// Both are fine: nothing requires IDs to be contiguous, and the Feistel
// permutation scatters them before anyone sees them.
type Allocator struct {
	claim     ClaimFunc
	blockSize uint64

	current atomic.Pointer[block]

	// refillMu serialises claims so a burst of exhausted callers makes one
	// database write between them rather than one each.
	refillMu sync.Mutex
}

// New returns an allocator. No block is claimed until the first Next.
func New(claim ClaimFunc, blockSize uint64) *Allocator {
	if blockSize == 0 {
		blockSize = 10000
	}
	return &Allocator{claim: claim, blockSize: blockSize}
}

// Next returns the next ID, claiming a fresh block if the current one is
// exhausted. The common path is one atomic add against the current block.
func (a *Allocator) Next(ctx context.Context) (uint64, error) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		b := a.current.Load()
		if b != nil {
			if id := b.start + b.cursor.Add(1) - 1; id < b.end {
				return id, nil
			}
		}
		if err := a.refill(ctx, b); err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("idgen: no ID after %d attempts; block size %d is too small for this creation rate", maxAttempts, a.blockSize)
}

// Remaining reports how many IDs are left in the current block. For metrics
// and tests; racy by nature.
func (a *Allocator) Remaining() uint64 {
	b := a.current.Load()
	if b == nil {
		return 0
	}
	used := b.cursor.Load()
	size := b.end - b.start
	if used >= size {
		return 0
	}
	return size - used
}

// refill claims a new block, unless another caller already replaced the one
// the caller found exhausted.
func (a *Allocator) refill(ctx context.Context, exhausted *block) error {
	a.refillMu.Lock()
	defer a.refillMu.Unlock()

	// Someone else refilled while this caller waited for the lock.
	if a.current.Load() != exhausted {
		return nil
	}

	start, err := a.claim(ctx, a.blockSize)
	if err != nil {
		return fmt.Errorf("idgen: claim block: %w", err)
	}

	a.current.Store(&block{start: start, end: start + a.blockSize})
	return nil
}
