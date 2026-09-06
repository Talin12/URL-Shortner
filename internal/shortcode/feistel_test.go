package shortcode

import (
	"math/rand/v2"
	"testing"
)

func TestPermuteIsInvertible(t *testing.T) {
	c := NewCodec(0xDEADBEEF)

	ids := []uint32{0, 1, 2, 61, 62, 1 << 16, 1<<16 - 1, 1<<31 - 1, 1<<32 - 1}
	for _, id := range ids {
		if got := c.Unpermute(c.Permute(id)); got != id {
			t.Errorf("Unpermute(Permute(%d)) = %d", id, got)
		}
	}

	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200000; i++ {
		id := rng.Uint32()
		if got := c.Unpermute(c.Permute(id)); got != id {
			t.Fatalf("Unpermute(Permute(%d)) = %d", id, got)
		}
	}
}

// A Feistel network is a bijection by construction, which is the whole reason
// it was chosen: uniqueness with no collision check and no lookup table. This
// checks the property holds over a large contiguous run, which is exactly the
// shape the block allocator produces.
func TestPermuteIsCollisionFreeOverAContiguousRun(t *testing.T) {
	c := NewCodec(12345)

	const n = 1 << 20
	seen := make(map[uint32]uint32, n)
	for id := uint32(0); id < n; id++ {
		out := c.Permute(id)
		if prev, dup := seen[out]; dup {
			t.Fatalf("collision: ids %d and %d both permute to %d", prev, id, out)
		}
		seen[out] = id
	}
}

// The security property: consecutive IDs must not produce guessable codes.
func TestConsecutiveIDsProduceScatteredCodes(t *testing.T) {
	c := NewCodec(99)

	// Neighbouring IDs should land far apart in the output space. With a good
	// permutation the mean gap approaches a third of the domain; anything
	// close to 1 would mean the codes are still walkable.
	var small int
	const samples = 10000
	for id := uint32(1000); id < 1000+samples; id++ {
		a, b := c.Permute(id), c.Permute(id+1)
		diff := a - b
		if b > a {
			diff = b - a
		}
		if diff < 1000 {
			small++
		}
	}

	// A handful of near misses is expected by chance; a systematic pattern is
	// not. Anything over 1% would mean the permutation is not mixing.
	if small > samples/100 {
		t.Errorf("%d of %d consecutive ID pairs landed within 1000 of each other; codes look enumerable", small, samples)
	}
}

func TestEncodeDecodeRoundTripsThroughBase62(t *testing.T) {
	c := NewCodec(7)

	for _, id := range []uint64{0, 1, 999, 100000, 1 << 24, MaxID} {
		code := c.Encode(id)
		got, err := c.Decode(code)
		if err != nil {
			t.Fatalf("Decode(%q) for id %d: %v", code, id, err)
		}
		if got != id {
			t.Errorf("round trip for %d gave %d via %q", id, got, code)
		}
	}
}

func TestCodesStayShort(t *testing.T) {
	c := NewCodec(7)
	// 62^6 > 2^32, so no ID in the domain needs more than six characters.
	for _, id := range []uint64{0, 1, 1 << 20, 1 << 31, MaxID} {
		if got := len(c.Encode(id)); got > 6 {
			t.Errorf("Encode(%d) produced a %d-character code, want <= 6", id, got)
		}
	}
}

func TestDifferentSeedsProduceDifferentCodes(t *testing.T) {
	a, b := NewCodec(1), NewCodec(2)
	var same int
	for id := uint64(0); id < 1000; id++ {
		if a.Encode(id) == b.Encode(id) {
			same++
		}
	}
	if same > 5 {
		t.Errorf("%d of 1000 codes matched across seeds; the key is not being used", same)
	}
}

func TestDecodeRejectsOutOfDomain(t *testing.T) {
	c := NewCodec(7)
	// A base62 string above 2^32 is not a code this codec could have issued.
	if _, err := c.Decode("zzzzzzz"); err == nil {
		t.Error("Decode accepted a code outside the permuted domain")
	}
}

func BenchmarkPermute(b *testing.B) {
	c := NewCodec(1)
	var id uint32
	for b.Loop() {
		id++
		_ = c.Permute(id)
	}
}
