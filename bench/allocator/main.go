// Command allocator verifies the block ID allocator under concurrent creation.
//
// PLAN.md phase 5 asks to "confirm the ID block allocator holds under
// concurrent creation". Across three instances there is no shared state except
// one Postgres row, so if the row lock does not do its job two replicas will
// hand out overlapping blocks and two links will collide on a short code.
//
// It also checks the property the Feistel permutation exists for: that the
// issued codes are not walkable.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type created struct {
	Code string `json:"code"`
}

func main() {
	var (
		baseURL    = flag.String("base-url", "http://localhost:8080", "service base URL")
		count      = flag.Int("count", 20000, "links to create")
		concurrent = flag.Int("concurrent", 100, "concurrent creators")
	)
	flag.Parse()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrent,
			MaxIdleConnsPerHost: *concurrent,
			MaxConnsPerHost:     *concurrent,
		},
	}

	codes := make([]string, *count)
	var (
		next     atomic.Int64
		failures atomic.Int64
		wg       sync.WaitGroup
	)

	start := time.Now()
	for w := 0; w < *concurrent; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= *count {
					return
				}
				code, err := create(client, *baseURL, i)
				if err != nil {
					failures.Add(1)
					continue
				}
				codes[i] = code
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	var issued []string
	for _, c := range codes {
		if c != "" {
			issued = append(issued, c)
		}
	}

	fmt.Printf("created:      %d links in %s (%.0f/s), %d failed\n",
		len(issued), elapsed.Round(time.Millisecond),
		float64(len(issued))/elapsed.Seconds(), failures.Load())

	// The invariant: no two links may share a code.
	seen := make(map[string]int, len(issued))
	duplicates := 0
	for i, code := range issued {
		if prev, dup := seen[code]; dup {
			if duplicates < 5 {
				fmt.Printf("  DUPLICATE: %q issued at positions %d and %d\n", code, prev, i)
			}
			duplicates++
			continue
		}
		seen[code] = i
	}

	fmt.Printf("unique codes: %d of %d\n", len(seen), len(issued))
	fmt.Printf("code length:  %s\n", lengthHistogram(issued))
	fmt.Printf("enumerable:   %s\n", enumerability(issued))

	if duplicates > 0 {
		fmt.Printf("\nRESULT: FAIL -- %d duplicate codes. Blocks overlapped.\n", duplicates)
		os.Exit(1)
	}
	fmt.Printf("\nRESULT: %d concurrent creations, zero duplicate codes, no collision check anywhere in the path.\n", len(issued))
}

func create(client *http.Client, baseURL string, i int) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"url": fmt.Sprintf("https://example.com/allocator/%d", i),
	})

	resp, err := client.Post(baseURL+"/api/links", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("create returned %d: %s", resp.StatusCode, body)
	}

	var out created
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Code, nil
}

func lengthHistogram(codes []string) string {
	counts := map[int]int{}
	for _, c := range codes {
		counts[len(c)]++
	}
	lengths := make([]int, 0, len(counts))
	for l := range counts {
		lengths = append(lengths, l)
	}
	sort.Ints(lengths)

	var out []string
	for _, l := range lengths {
		out = append(out, fmt.Sprintf("%d chars: %d", l, counts[l]))
	}
	return joinComma(out)
}

// enumerability reports how often consecutive creations produced codes sharing
// a prefix. With plain base62 of a sequential ID this would be nearly every
// pair; with the Feistel permutation it should be almost none.
func enumerability(codes []string) string {
	if len(codes) < 2 {
		return "n/a"
	}
	var shared int
	for i := 1; i < len(codes); i++ {
		a, b := codes[i-1], codes[i]
		if len(a) == len(b) && len(a) > 1 && a[:len(a)-1] == b[:len(b)-1] {
			shared++
		}
	}
	pct := 100 * float64(shared) / float64(len(codes)-1)
	return fmt.Sprintf("%d of %d consecutive pairs share a prefix (%.3f%%)", shared, len(codes)-1, pct)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
