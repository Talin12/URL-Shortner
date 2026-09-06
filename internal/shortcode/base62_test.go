package shortcode

import "testing"

func TestEncodeDecodeRoundTrip(t *testing.T) {
	ids := []uint64{0, 1, 61, 62, 63, 3843, 3844, 1_000_000, 1<<63 - 1, 1<<64 - 1}
	for _, id := range ids {
		code := Encode(id)
		got, err := Decode(code)
		if err != nil {
			t.Fatalf("Decode(%q) from id %d: %v", code, id, err)
		}
		if got != id {
			t.Errorf("round trip for %d: got %d via %q", id, got, code)
		}
	}
}

func TestEncodeKnownValues(t *testing.T) {
	cases := map[uint64]string{
		0:  "0",
		9:  "9",
		10: "A",
		35: "Z",
		36: "a",
		61: "z",
		62: "10",
	}
	for id, want := range cases {
		if got := Encode(id); got != want {
			t.Errorf("Encode(%d) = %q, want %q", id, got, want)
		}
	}
}

func TestDecodeRejectsJunk(t *testing.T) {
	for _, code := range []string{"", "a b", "hello!", "ab-cd", "日本", "zzzzzzzzzzzz"} {
		if _, err := Decode(code); err == nil {
			t.Errorf("Decode(%q) accepted an invalid code", code)
		}
		if Valid(code) {
			t.Errorf("Valid(%q) = true, want false", code)
		}
	}
}

func TestDecodeRejectsOverflow(t *testing.T) {
	// 11 digits of 'z' is larger than MaxUint64.
	if _, err := Decode("zzzzzzzzzzz"); err == nil {
		t.Error("Decode accepted a value past MaxUint64")
	}
}

func BenchmarkEncode(b *testing.B) {
	for i := 0; b.Loop(); i++ {
		_ = Encode(uint64(i))
	}
}
