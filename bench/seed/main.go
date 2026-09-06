// Command seed populates the service with links and writes their codes to a
// JSON file for the k6 benchmark to sample from.
//
// The benchmark needs a fixed, known key set: sampling codes at random from an
// unknown keyspace would make every request a 404 and measure nothing.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		baseURL = flag.String("base-url", "http://localhost:8080", "service base URL")
		count   = flag.Int("count", 10000, "number of links to create")
		workers = flag.Int("workers", 16, "concurrent creation workers")
		out     = flag.String("out", "bench/codes.json", "where to write the code list")
	)
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	codes := make([]string, *count)

	var (
		next     atomic.Int64
		failures atomic.Int64
		wg       sync.WaitGroup
	)
	start := time.Now()

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= *count {
					return
				}
				code, err := create(client, *baseURL, fmt.Sprintf("https://example.com/destination/%d", i))
				if err != nil {
					failures.Add(1)
					continue
				}
				codes[i] = code
			}
		}()
	}
	wg.Wait()

	// Creation failures leave empty slots; drop them rather than handing k6
	// codes that will 404 and quietly inflate the error rate.
	kept := codes[:0]
	for _, c := range codes {
		if c != "" {
			kept = append(kept, c)
		}
	}

	body, err := json.Marshal(kept)
	if err != nil {
		log.Fatalf("encode codes: %v", err)
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}

	log.Printf("seeded %d links (%d failed) in %s -> %s", len(kept), failures.Load(), time.Since(start).Round(time.Millisecond), *out)
}

func create(client *http.Client, baseURL, destination string) (string, error) {
	payload, err := json.Marshal(map[string]string{"url": destination})
	if err != nil {
		return "", err
	}

	resp, err := client.Post(baseURL+"/api/links", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("create returned %d: %s", resp.StatusCode, msg)
	}

	var out struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Code, nil
}
