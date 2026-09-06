// Command stampede demonstrates the cache stampede fix from PLAN.md 5.2.
//
// It creates a brand-new link (never resolved, so cold in every tier), fires N
// concurrent requests at it, and reports how many Postgres queries that
// produced by reading linkflow_origin_queries_total before and after.
//
// With singleflight the answer is 1. Restart the service with
// LINKFLOW_SINGLEFLIGHT=false and run it again: the counter climbs with the
// concurrency. That pair of numbers is the point -- the fix is measured, not
// asserted.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		baseURL    = flag.String("base-url", "http://localhost:8080", "service base URL")
		concurrent = flag.Int("concurrent", 10000, "simultaneous requests for the cold key")
	)
	flag.Parse()

	// One shared client with a pool large enough not to be the bottleneck --
	// otherwise this measures Go's connection limit, not the service.
	client := &http.Client{
		Timeout: 30 * time.Second,
		// Do not follow the 302. Following it would report the destination
		// host's status instead of ours -- the same reason the k6 script sets
		// redirects: 0.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			MaxIdleConns:        *concurrent,
			MaxIdleConnsPerHost: *concurrent,
			MaxConnsPerHost:     *concurrent,
		},
	}

	// Establish every connection before the barrier. Without this the herd is
	// paced by TCP setup: dispatching 10,000 fresh connections takes over a
	// second, the first query finishes long before the rest arrive, and the
	// cache warms so the stampede never actually forms. That would measure
	// the load generator, not the service.
	if err := warmConnections(client, *baseURL, *concurrent); err != nil {
		log.Fatalf("pre-establish connections: %v", err)
	}
	fmt.Printf("connections:   %d established before the barrier\n", *concurrent)

	code, err := createLink(client, *baseURL)
	if err != nil {
		log.Fatalf("create cold link: %v", err)
	}
	fmt.Printf("cold key:      %s (never resolved, so absent from every tier)\n", code)

	before, err := originQueries(client, *baseURL)
	if err != nil {
		log.Fatalf("read metrics: %v", err)
	}

	// Every goroutine blocks on the same channel so they are released together
	// rather than trickling in, which is what makes this a stampede.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}

	for i := 0; i < *concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			req, err := http.NewRequest(http.MethodGet, *baseURL+"/"+code, nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				mu.Lock()
				statuses[0]++
				mu.Unlock()
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			mu.Lock()
			statuses[resp.StatusCode]++
			mu.Unlock()
		}()
	}

	begin := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(begin)

	after, err := originQueries(client, *baseURL)
	if err != nil {
		log.Fatalf("read metrics: %v", err)
	}

	fmt.Printf("concurrency:   %d simultaneous requests\n", *concurrent)
	fmt.Printf("elapsed:       %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("responses:     %s\n", formatStatuses(statuses))
	fmt.Printf("pg queries:    %.0f  (before %.0f, after %.0f)\n", after-before, before, after)

	delta := after - before
	switch {
	case delta <= 1:
		fmt.Printf("\nRESULT: %d concurrent misses collapsed into %.0f database query.\n", *concurrent, delta)
	default:
		fmt.Printf("\nRESULT: %d concurrent misses produced %.0f database queries.\n", *concurrent, delta)
	}
}

// warmConnections opens n keep-alive connections and returns them to the idle
// pool, so the measured requests need no dialing.
func warmConnections(client *http.Client, baseURL string, n int) error {
	var wg sync.WaitGroup
	var failed atomic.Int64

	// A barrier here too: without it each goroutine would reuse the idle
	// connection the previous one just released, and we would open one
	// connection rather than n.
	release := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(baseURL + "/healthz")
			if err != nil {
				failed.Add(1)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			// Hold the connection out of the pool until every goroutine has
			// one of its own.
			<-release
			resp.Body.Close()
		}()
	}

	// Give the dials time to land, then let everyone return their connection.
	time.Sleep(2 * time.Second)
	close(release)
	wg.Wait()

	if f := failed.Load(); f > int64(n/100) {
		return fmt.Errorf("%d of %d warm-up connections failed", f, n)
	}
	return nil
}

func createLink(client *http.Client, baseURL string) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"url": fmt.Sprintf("https://example.com/stampede/%d", time.Now().UnixNano()),
	})

	resp, err := client.Post(baseURL+"/api/links", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("create returned %d: %s", resp.StatusCode, body)
	}

	var out struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Code, nil
}

// originQueries scrapes one counter out of the Prometheus exposition format.
// Parsing a single well-known line is cheaper than pulling in a parser.
func originQueries(client *http.Client, baseURL string) (float64, error) {
	resp, err := client.Get(baseURL + "/metrics")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	const name = "linkflow_origin_queries_total"
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		return strconv.ParseFloat(fields[1], 64)
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s not found in /metrics", name)
}

func formatStatuses(statuses map[int]int) string {
	var b strings.Builder
	for code, n := range statuses {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		if code == 0 {
			fmt.Fprintf(&b, "transport error: %d", n)
			continue
		}
		fmt.Fprintf(&b, "%d: %d", code, n)
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}
