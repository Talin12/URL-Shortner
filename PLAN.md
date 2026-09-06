# Project Spec: High-Throughput Link Service with Analytics Ingestion

A build plan for a URL shortener that is actually worth putting on a resume.

---

## 1. Why the generic version fails

The standard URL shortener is a weekend tutorial. `POST /shorten` writes a row, `GET /{code}` reads a row and returns a 302. Add Redis in front and you are done in an afternoon. Thousands of these exist on GitHub with near-identical READMEs.

The redirect path being fast is not an achievement. It is a single indexed lookup on a small table. Postgres will do that in under a millisecond without you doing anything clever. There is no engineering there to talk about.

**The part that is actually hard is the click event.**

Every redirect generates an analytics write. If you insert that row synchronously before responding, your redirect latency is now bound by a database write, and under load those writes contend with the reads that serve redirects. Your fast read path is now hostage to your write path. That is the real problem, and it is the one nobody builds for.

So the project is not "a URL shortener." The project is:

> A read path that stays fast while a high-volume, lossy-tolerant write path runs underneath it, with the two decoupled deliberately and the trade-off measured.

That reframing is what makes it interviewable.

---

## 2. The core design principle

The two paths have completely different requirements. Write them down and let every decision follow from this table.

| | Redirect (read) | Click event (write) |
|---|---|---|
| Latency budget | Sub-millisecond, hard | Seconds are fine |
| Consistency | Must be correct | Approximate is fine |
| Durability | Source of truth, cannot lose | Losing 0.1% is acceptable |
| Volume | High | Identical, by definition |
| Access pattern | Point lookup by key | Append-only, queried in aggregate |
| Failure mode | Outage | Degraded stats |

Two workloads this different should not share a storage engine or a code path. Almost everything below is a consequence of that.

---

## 3. Stack

### Language: Go

Non-negotiable for this project, and the strongest single reason to build it.

- Your resume is currently Python/Django three times over. A fourth Django service adds nothing. Go adds a language and a category of role.
- Goroutines and channels make the decoupled-write-path design natural rather than bolted on.
- `pprof` is built in, so profiling is a normal part of the workflow instead of a research project.
- It is what backend teams at Razorpay, Swiggy, Zomato, PhonePe, and Dream11 actually hire for.

### Components

| Layer | Choice | Why |
|---|---|---|
| HTTP server | `net/http` (Go 1.22+ routing) or `chi` | Stdlib routing is good now. Skip Gin, you do not need it. |
| URL store | PostgreSQL via `pgx/v5` native pool | Source of truth. Use pgx directly, not through `database/sql`. |
| Distributed cache | Redis via `go-redis/v9` | Shared cache across instances. |
| Local cache | `dgraph-io/ristretto` | Per-instance LRU. TinyLFU admission policy, which matters for hot keys. |
| Analytics store | ClickHouse via `clickhouse-go/v2` | Columnar, built for exactly this ingest-heavy append-only workload. |
| Dedup on cache miss | `golang.org/x/sync/singleflight` | Cache stampede protection in ~5 lines. |
| Metrics | Prometheus + Grafana | Your measurements need to be visible, not printed to stdout. |
| Load generation | `k6` or `bombardier` | k6 gives you scriptable scenarios and clean percentile output. |
| Orchestration | Docker Compose | Whole stack up with one command. |

### On ClickHouse

Do not start with it. Start with batched inserts into Postgres, push load until it degrades, record where and how it broke, then introduce ClickHouse and measure the difference.

The story "I used ClickHouse because it is fast" is worthless. The story "Postgres batch inserts held up to roughly X events/sec before write amplification started pushing redirect p99 up, so I moved click events to ClickHouse and the read path stopped caring" is a real engineering narrative with evidence behind it. The migration is the interesting part. Do not skip to the destination.

---

## 4. Architecture

```
                         ┌──────────────────────────┐
   GET /{code}  ───────► │  Go service (N replicas) │
                         └────────────┬─────────────┘
                                      │
                    ┌─────────────────┴─────────────────┐
                    │                                   │
              READ PATH                            WRITE PATH
                    │                                   │
        ┌───────────▼───────────┐          ┌────────────▼────────────┐
        │ 1. Ristretto (local)  │          │ Buffered chan (bounded) │
        │    ~100ns             │          │ non-blocking send       │
        └───────────┬───────────┘          └────────────┬────────────┘
                    │ miss                              │
        ┌───────────▼───────────┐          ┌────────────▼────────────┐
        │ 2. Redis (shared)     │          │ Batcher goroutine       │
        │    ~0.5ms             │          │ flush on 1000 or 200ms  │
        └───────────┬───────────┘          └────────────┬────────────┘
                    │ miss (singleflight)               │
        ┌───────────▼───────────┐          ┌────────────▼────────────┐
        │ 3. Postgres           │          │ ClickHouse              │
        │    source of truth    │          │ async batch insert      │
        └───────────────────────┘          └─────────────────────────┘
                    │
              302 returned before the click event is durable
```

The last line is the whole design. The response goes out as soon as the destination URL is known. The click event has not been written anywhere durable at that point, and that is on purpose.

---

## 5. The hard problems

These five are the substance. Everything else is plumbing.

### 5.1 Backpressure and the drop policy

The buffered channel is bounded. If ClickHouse gets slow or goes down, the buffer fills. You have three options:

1. **Block the redirect.** Analytics failure becomes a redirect outage. Unacceptable.
2. **Spill to local disk.** Durable, but now you own a disk queue and its recovery logic.
3. **Drop the event and increment a counter.** Redirects unaffected, analytics degraded and visibly so.

Take option 3. Use a non-blocking channel send:

```go
select {
case s.events <- ev:
default:
    s.metrics.EventsDropped.Inc()
}
```

Expose `link_events_dropped_total` in Prometheus and put it on the Grafana dashboard.

This is small in code and large in interview value. You made an explicit availability decision, you picked which subsystem is allowed to fail, and you made the failure observable instead of silent. Be ready to defend it, including the case where the answer would change (billing events, fraud detection, anything where a dropped event costs money).

### 5.2 Cache stampede

A viral link's cache entry expires. Ten thousand concurrent requests miss simultaneously and all ten thousand hit Postgres for the same row. Postgres falls over. Your cache, the thing meant to protect the database, just created a thundering herd against it.

Fix with `singleflight`. Concurrent calls for the same key collapse into one actual database round trip, and the rest wait on the result.

```go
v, err, _ := s.group.Do(code, func() (interface{}, error) {
    return s.fetchFromPostgres(ctx, code)
})
```

Prove it works: instrument the Postgres query count, fire 10k concurrent requests at a cold key, and show the counter went up by 1 instead of 10,000. That before/after number is the kind of thing that belongs in your README.

Optional extension: probabilistic early expiration (the XFetch approach), where entries near expiry get refreshed early with a probability that rises as TTL approaches zero. Smooths the refresh cliff entirely. Nice to mention that you considered it even if you do not build it.

### 5.3 Hot keys

One link goes viral and takes 40% of all traffic. In a multi-instance setup with a shared Redis, every one of those requests crosses the network to the same Redis node. That node saturates.

The local Ristretto cache in front of Redis is the fix, and it is the reason the cache is two-tier rather than one. Hot keys get absorbed in-process at roughly 100ns and never touch the network. Cold keys go to Redis. The skew in the traffic is what makes the local cache effective, so the more extreme the hot key, the better this works.

Measure and report the hit rate at each tier separately: local, Redis, Postgres. Under a realistic skewed load you should see the overwhelming majority terminating at tier 1.

### 5.4 Short code generation

Four approaches, in ascending order of how good they make you look:

| Approach | Problem |
|---|---|
| Random + collision check | DB round trip per creation, degrades as keyspace fills |
| Global auto-increment counter | Contention point, and codes are sequentially guessable |
| Snowflake-style IDs | No coordination, but 64 bits is a long code |
| **Pre-allocated ID blocks** | One DB write per 10,000 links, zero coordination on the hot path |

Build the fourth. Each instance claims a block of 10,000 IDs from a central counter row, then hands them out from a local atomic counter. Creation becomes lock-free until the block runs out.

Then deal with enumerability. Sequential IDs mean `/aB3` and `/aB4` are both real links, so anyone can walk your entire keyspace and read other people's destinations. Fix by mapping the sequential ID through a bijection before base62 encoding. Multiplying by a large odd number modulo 2^k works and is invertible. A small Feistel network is the cleaner version. Codes come out scattered, stay collision-free by construction, and no lookup table is needed.

Very few student projects think about enumerability. It is a security consideration that arises naturally from a performance decision, and connecting those two is exactly the kind of reasoning interviewers are probing for.

### 5.5 Benchmarking that survives scrutiny

This is where your original "1M requests per second" idea would have collapsed, so get it right.

**Load generation is a bottleneck too.** A single machine running k6 saturates in the tens of thousands of RPS. If the load generator shares a box with the service, they fight for the same cores and every number you produce is garbage. Run them separately, or at minimum pin cores and say so.

**Use a Zipfian key distribution, not uniform random.** Real link traffic is heavily skewed: a few links get enormous traffic, most get almost none. Uniform random keys make every request a cache miss, which is both unrealistic and makes your cache design look pointless. Zipf is realistic and it is what lets you demonstrate hot-key handling at all. Most people get this wrong. Getting it right is a differentiator on its own.

**Report the full context, always:**

```
Hardware:     4 vCPU / 8GB, service and load generator on separate hosts
Distribution: Zipf(s=1.2) over 1M keys
Concurrency:  500 connections
Duration:     5 min sustained
Throughput:   62,400 req/s
Latency:      p50 0.8ms  p95 3.1ms  p99 7.4ms  p99.9 22ms
Errors:       0.00%
Cache:        local 91.2% | redis 8.4% | postgres 0.4%
```

**Find the knee, not the ceiling.** Ramp concurrency and plot p99 against throughput. There is a point where throughput stops rising and p99 starts climbing steeply. That inflection is your real capacity. "62k RPS at p99 under 10ms" is a number with meaning. "Peak 90k RPS" with a 400ms p99 is a number that means the system is already broken.

**Profile the bottleneck.** Run `pprof` under load, generate a flame graph, find where the time goes, fix one thing, re-measure. Put both flame graphs in the README. Showing that you found the bottleneck through measurement rather than guessing is worth more than the throughput number itself.

---

## 6. Build phases

Roughly five to six weeks part-time. Every phase ends with something that runs.

### Phase 1 — Baseline (week 1)
Go service, Postgres, create and redirect endpoints, base62 encoding, Docker Compose. Synchronous click writes to Postgres. Deliberately naive.

Benchmark it. **Save these numbers.** This is your before, and without it none of the later improvements have anything to be measured against.

### Phase 2 — Decouple the write path (week 2)
Bounded channel, batcher goroutine, batch inserts to Postgres, non-blocking send with a drop counter, graceful shutdown that flushes the buffer.

Re-benchmark. Redirect p99 should drop sharply. That delta is your first real result.

### Phase 3 — Caching (week 3)
Redis, then Ristretto in front of it, then singleflight on the miss path. Per-tier hit-rate metrics. Prometheus and Grafana wired up.

Re-benchmark under Zipfian load. Demonstrate the stampede fix with the concurrent cold-key test.

### Phase 4 — Analytics store (week 4)
Push ingest until Postgres degrades and document exactly how it fails. Add ClickHouse, move click events there, define the schema and any materialized rollups.

Re-benchmark ingest. Compare against the Postgres numbers.

### Phase 5 — Distribution and durability (week 5)
Run 3 instances behind nginx or Caddy. Confirm the ID block allocator holds under concurrent creation. Optionally insert Redis Streams between the batcher and ClickHouse so events survive a process crash, and measure what that durability costs in latency.

### Phase 6 — Write it up (week 6)
The README is not an afterthought. For a project whose entire point is measurement, the write-up carries most of the value. Budget real time for it.

---

## 7. What to skip

Every hour spent here is an hour not spent on the parts that matter.

- User accounts, auth, sessions
- A link management dashboard with CRUD
- Custom domains
- QR code generation
- Link expiry UI, password-protected links, edit-after-create
- A polished frontend

These are all CRUD. You already have three CRUD projects. If you want to view the analytics, a single read-only HTML page served by the Go binary is enough, and it should take an afternoon.

The one exception worth considering: a live Grafana dashboard showing throughput, per-tier cache hit rate, p99 latency, and dropped events under load. That is not decoration, it is evidence, and it demos well in an interview.

---

## 8. The README

Structure it in this order. Most readers stop after the first two sections, so put the results there.

1. **What this is, in three sentences.** Lead with the read/write asymmetry framing.
2. **Results.** The benchmark table with full hardware and methodology context. Before and after for each phase.
3. **Architecture diagram.** The two-path diagram above.
4. **Design decisions and trade-offs.** One short section per hard problem from section 5. State what you chose, what you gave up, and when you would choose differently.
5. **Benchmark methodology.** Hardware, load generator placement, key distribution, duration, warm-up. Enough that someone could reproduce it.
6. **Failure modes.** What happens when ClickHouse dies, when Redis dies, when the buffer fills, when an instance is killed mid-batch.
7. **What I would do differently.** Honest limitations. Single-region, no rate limiting, at-most-once analytics delivery, whatever else is true.

Section 7 matters more than people expect. Acknowledging real limitations reads as engineering maturity. Claiming a student project is production-ready reads as the opposite.

---

## 9. Resume lines

Fill the placeholders from your own measurements. Never estimate, never extrapolate.

> **LINKFLOW** | Go · PostgreSQL · Redis · ClickHouse · Docker · Prometheus
>
> - Built a high-throughput link redirection service in Go, sustaining **[X]** req/s at **p99 [Y]ms** on **[hardware]** under a Zipfian load distribution, verified with k6 against a separate load-generation host.
> - Decoupled analytics ingestion from the redirect path using a bounded channel and batching goroutine, cutting redirect p99 by **[Z]%** and isolating the read path from downstream write failures.
> - Designed a two-tier cache (in-process TinyLFU + Redis) with singleflight deduplication, reaching a **[N]%** local hit rate and collapsing a 10,000-request cold-key stampede into a single database query.
> - Implemented block-allocated ID generation with a Feistel-network bijection, removing per-creation coordination while keeping short codes non-enumerable.
> - Migrated click-event storage to ClickHouse after profiling Postgres write amplification as the ingest bottleneck, raising sustained ingest from **[A]** to **[B]** events/s.

Five bullets, every one of them a number you personally measured. Compare that to a bullet claiming a million requests per second that falls apart under three follow-up questions.

---

## 10. Interview questions to prepare for

If any of these make you uncomfortable, that part of the project is not finished.

**On the design**
- Why not just write click events synchronously? What does that cost you?
- You drop events under backpressure. What breaks if these were billing events?
- Why two cache tiers instead of one?
- Walk me through what happens if an instance is SIGKILLed with 800 events in its buffer.

**On the numbers**
- What hardware, what payload, where was the load generator running?
- What was p99, and what was p99.9?
- Where did it break, and what was the bottleneck?
- Why Zipfian and not uniform? What would the numbers look like with uniform keys?

**On scaling further**
- 10x traffic tomorrow. What breaks first?
- How would you shard this? What would you shard on?
- Multi-region: how do you handle the write path?
- How do you deploy without dropping in-flight events?

**On the trade-offs**
- Why Go over Python here? Be specific, not vague.
- Why ClickHouse over Postgres, and when would Postgres have been the right call?
- What did you get wrong the first time?

That last one is worth having a real answer to. Everyone has one. The people who claim they do not are the ones who did not measure anything.

---

## 11. Names

Avoid `url-shortener` on GitHub. The name sets the expectation before anyone reads a line.

`linkflow` · `redir` · `hoplink` · `shortwave` · `beacon` · `snap`

---

## Summary

You are not building a URL shortener. You are building a system where a latency-critical read path and a volume-heavy write path share a process without interfering, and you are proving it with measurements you can defend line by line.

The redirect is the excuse. The write path is the project.