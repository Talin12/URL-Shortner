package httpapi

import "testing"

func TestNormalizeDestinationAccepts(t *testing.T) {
	cases := []string{
		"https://example.com",
		"http://example.com/path?q=1#frag",
		"  https://example.com/trimmed  ",
	}
	for _, in := range cases {
		if _, err := normalizeDestination(in); err != nil {
			t.Errorf("normalizeDestination(%q) = %v, want nil", in, err)
		}
	}
}

func TestNormalizeDestinationRejects(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"whitespace":     "   ",
		"no scheme":      "example.com",
		"javascript":     "javascript:alert(1)",
		"data":           "data:text/html,<script>alert(1)</script>",
		"file":           "file:///etc/passwd",
		"scheme no host": "https://",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeDestination(in); err == nil {
				t.Errorf("normalizeDestination(%q) accepted an unsafe or malformed URL", in)
			}
		})
	}
}

func TestNormalizeDestinationRejectsOverlongURL(t *testing.T) {
	long := "https://example.com/" + string(make([]byte, maxDestinationLen))
	if _, err := normalizeDestination(long); err == nil {
		t.Error("normalizeDestination accepted a URL past the length cap")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q, want %q", got, "abc")
	}
	if got := truncate("ab", 5); got != "ab" {
		t.Errorf("truncate shortened a string under the limit: %q", got)
	}
}
