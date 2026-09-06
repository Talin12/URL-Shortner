package shortcode

import "encoding/binary"

// Codec turns a sequential link ID into a short code and back.
//
// The problem it solves is a consequence of the performance decision made
// elsewhere: block-allocated IDs are sequential, and base62 of a sequential ID
// is sequential too. /aB3 and /aB4 would both be real links, so anyone could
// walk the keyspace and read other people's destinations. Hashing would fix
// enumerability but reintroduce collisions, and a lookup table would
// reintroduce the per-creation coordination the allocator just removed.
//
// A Feistel network is the way out. It is a bijection by construction -- every
// input maps to exactly one output and vice versa -- so codes stay unique with
// no collision check, no table, and no coordination, while consecutive IDs
// produce codes that look unrelated.
type Codec struct {
	keys [rounds]uint32
}

// rounds is the number of Feistel rounds. Three is the classic minimum for a
// pseudorandom permutation; four gives margin. This is obfuscation, not
// encryption -- it stops enumeration, and is not a secret-keeping mechanism.
const rounds = 4

// domainBits is the size of the permuted space: 2^32 IDs, which is 4.29
// billion links and at most 6 base62 characters.
const domainBits = 32

// halfBits splits the domain into two halves for the Feistel rounds.
const halfBits = domainBits / 2

// halfMask masks a value down to one half.
const halfMask = (1 << halfBits) - 1

// MaxID is the largest ID the codec can permute.
const MaxID = uint64(1)<<domainBits - 1

// NewCodec derives round keys from a seed. The same seed must be used for the
// life of a deployment: change it and every existing short code decodes to a
// different link.
func NewCodec(seed uint64) *Codec {
	var c Codec
	state := seed
	for i := range c.keys {
		// SplitMix64: cheap, well-distributed, and deterministic across
		// processes so every instance derives identical keys.
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		c.keys[i] = uint32(z)
	}
	return &c
}

// Permute scrambles an ID. Bijective over [0, MaxID].
func (c *Codec) Permute(id uint32) uint32 {
	left := id >> halfBits
	right := id & halfMask

	for i := 0; i < rounds; i++ {
		left, right = right, left^c.round(right, i)
	}
	return left<<halfBits | right
}

// Unpermute reverses Permute. Running the rounds backwards is all a Feistel
// network needs to invert, which is why the round function does not have to be
// invertible itself.
func (c *Codec) Unpermute(scrambled uint32) uint32 {
	left := scrambled >> halfBits
	right := scrambled & halfMask

	for i := rounds - 1; i >= 0; i-- {
		left, right = right^c.round(left, i), left
	}
	return left<<halfBits | right
}

// Encode permutes an ID and renders it in base62.
func (c *Codec) Encode(id uint64) string {
	return Encode(uint64(c.Permute(uint32(id))))
}

// Decode reverses Encode.
func (c *Codec) Decode(code string) (uint64, error) {
	scrambled, err := Decode(code)
	if err != nil {
		return 0, err
	}
	if scrambled > MaxID {
		return 0, ErrInvalidCode
	}
	return uint64(c.Unpermute(uint32(scrambled))), nil
}

// round is the Feistel round function: a keyed mix of one half. It need not be
// invertible, only well-mixing.
func (c *Codec) round(half uint32, i int) uint32 {
	var buf [8]byte
	binary.LittleEndian.PutUint32(buf[0:4], half)
	binary.LittleEndian.PutUint32(buf[4:8], c.keys[i])

	z := binary.LittleEndian.Uint64(buf[:])
	z = (z ^ (z >> 33)) * 0xFF51AFD7ED558CCD
	z = (z ^ (z >> 33)) * 0xC4CEB9FE1A85EC53
	z ^= z >> 33
	return uint32(z) & halfMask
}
