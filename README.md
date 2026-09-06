# linkflow

A URL shortener where the redirect is the excuse and the write path is the project.

Every redirect generates an analytics write. If you insert that row before
responding, redirect latency is bound by a database write and those writes
contend with the reads that serve redirects — your fast read path is hostage to
your write path. **linkflow is a read path that stays fast while a high-volume,
loss-tolerant write path runs underneath it, with the two decoupled
deliberately and the trade-off measured.**

Built in six phases, each ending with numbers taken on the same machine in the
same session. **This README reports what was measured, including the three
times the measurement contradicted the design.**

---

## Status

| Phase | What it adds | State |
|---|---|---|
| 1 | Baseline: Go + Postgres, synchronous click writes | **Done — the control arm** |
| 2 | Bounded channel + batcher goroutine, drop policy | **Done — numbers below** |
| 3 | Ristretto → Redis → Postgres, singleflight, Prometheus | **Done — and it made things slower** |
| 4 | ClickHouse for click events | **Done — numbers below** |
| 5 | Multi-instance, block ID allocator, non-enumerable codes | **Done — verified below** |
| 6 | The write-up | **This document** |

Every phase's alternative is still in the binary behind an environment
variable. Phase 1's synchronous recorder is not dead code — it is the control
arm, and keeping all of them means each comparison is one binary on one machine
in one session rather than two builds measured weeks apart.

**The short version of what was learned**, before the detail:

| Phase | Expected | Measured |
|---|---|---|
| 2 | Decoupling helps | 2.3× throughput at equal p99, costing 0.157% of events |
| 3 | Cache helps | **27% slower** against a fast origin; 2.4× faster against a slow one |
| 4 | ClickHouse is faster | Throughput unchanged; Postgres silently **dropped 1.08% of events** |
| 5 | Allocator scales creation | 20,000 concurrent creations, 3 database writes, 0 duplicates |

---

## Results — phase 2 vs phase 1

Same binary, same machine, same session, back-to-back, database volume dropped
and re-seeded between arms. `sync` inserts each click on the redirect path;
`batch` hands it to a bounded channel and returns.

```
Hardware:     Apple M4, 10 cores, 16 GB. Service in Docker Desktop (10 CPU / 8 GB VM).
Caveat:       Load generator ran on the SAME machine as the service. See "Methodology".
Distribution: Zipf(s=1.2) over 10,000 keys
Duration:     30 s per step, no warm-up discarded
Batching:     flush on 1000 events or 200 ms, 10,000-event buffer
```

### Throughput

| VUs | sync (phase 1) | batch (phase 2) | change |
|---:|---:|---:|---:|
| 25 | 8,227 req/s | 21,306 req/s | **+159%** |
| 50 | 10,583 req/s | 23,285 req/s | **+120%** |
| 100 | 9,743 req/s | **24,698 req/s** | **+154%** |
| 200 | 9,320 req/s | 19,294 req/s | +107% |
| 400 | 8,694 req/s | 14,476 req/s | +67% |

### Redirect latency

| VUs | sync p50 | batch p50 | sync p99 | batch p99 | p99 change | sync p99.9 | batch p99.9 |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 25 | 2.77 ms | 0.98 ms | 7.00 ms | 3.61 ms | **−48%** | 17.47 ms | 8.33 ms |
| 50 | 4.31 ms | 1.85 ms | 10.81 ms | 6.54 ms | **−40%** | 31.29 ms | 14.24 ms |
| 100 | 9.80 ms | 3.60 ms | 18.41 ms | 11.04 ms | **−40%** | 38.18 ms | 20.04 ms |
| 200 | 20.27 ms | 9.04 ms | 41.18 ms | 30.85 ms | −25% | 64.69 ms | 69.20 ms |
| 400 | 44.10 ms | 25.40 ms | 80.02 ms | 66.03 ms | −18% | 97.75 ms | 93.97 ms |

Errors: 0.00% in every run, both arms.

### What the numbers say

**The knee moved.** Sync peaks at 50 VUs (10,583 req/s) and is flat-to-falling
after; batch keeps climbing to 100 VUs (24,698 req/s) before it turns over.
Decoupling did not just make the same curve faster, it moved where the system
stops scaling.

**Held at equal latency, throughput roughly doubles.** Sync's best point is
10,583 req/s at p99 10.81 ms. Batch does 24,698 req/s at p99 11.04 ms — the
same tail latency, **2.3× the traffic**.

**The read path stopped paying for the write path.** p50 fell by roughly 60% at
every step. That is the redirect no longer waiting on an `INSERT` and its index
maintenance before the handler returns.

### The cost, stated plainly

```json
{"mode":"batch","accepted":3092549,"written":3087692,
 "dropped_buffer_full":4857,"dropped_write_failed":0,"batches":3423}
```

**4,857 click events were dropped — 0.157% of 3.09 M.** The buffer filled during
the highest-concurrency steps and the non-blocking send discarded the overflow
rather than making redirects wait. Sync dropped nothing: 1,397,490 accepted,
1,397,490 written. That is the entire trade in two numbers, and it is the right
one *for analytics* and the wrong one for billing.

Average batch size was 902 events (3,423 flushes), close to the 1,000 cap —
under load the size trigger fires well before the 200 ms timer, which is what
you want.

Batch also ingested **2.2× more events** (3.09 M vs 1.40 M) in the same wall
time, simply because it served 2.2× more redirects.

## Results — phase 3: the cache that made things slower

Three cache configurations, same Zipf(s=1.2) load over 10,000 keys, fresh
database per arm. The local tier holds 1,000 entries — 10% of the keyspace, so
skew has to do the work.

| VUs | no cache | Redis only | two-tier |
|---:|---:|---:|---:|
| 50 | **26,277 req/s** | 17,465 req/s | 18,960 req/s |
| 100 | **24,980 req/s** | 18,091 req/s | 19,156 req/s |
| 200 | **21,141 req/s** | 18,415 req/s | 15,158 req/s |

**Adding a cache cost 27% of throughput.** Not the expected result, and worth
stating plainly rather than tuning until the graph flatters the design.

The reason is in the setup: the origin is a 10,000-row table sitting entirely
in Postgres' shared buffers, on the same host, reached over loopback. That
lookup takes on the order of 0.1 ms. A Redis round trip through Docker's
network stack costs more than the query it is meant to avoid, and on a miss the
service pays *both* — Redis lookup, Postgres query, then a Redis write to
populate. PLAN.md §1 says it outright: "Postgres will do that in under a
millisecond without you doing anything clever."

### The same test with a slow origin

Re-run with 5 ms injected into the origin query — a database that is loaded,
or simply not on the same machine. Nothing else changed. 100 VUs:

| | no cache | two-tier | change |
|---|---:|---:|---:|
| Throughput | 12,759 req/s | **30,757 req/s** | **+141%** |
| p50 | 7.52 ms | 2.88 ms | −62% |
| p99 | 17.26 ms | 10.00 ms | −42% |
| Postgres queries | 197,786 | **9,920** | −95% |

**The cache is worth nothing at 0.1 ms origin latency and worth 2.4× at 5 ms.**
That is the actual finding, and it is more useful than a graph showing the
cache always wins: a cache is a bet that the origin is slow or far away, and
this benchmark's origin is neither.

### What the tiers actually did

Under Zipf(1.2), a local cache holding **10% of the keyspace absorbed 42.7%**
of all lookups (394,196 hits against 528,589 misses). That is TinyLFU admission
doing what an LRU would not: keeping the hot set resident instead of letting a
scan of cold keys evict it.

Singleflight turned out to matter more than either cache tier. With **no cache
at all** and a slow origin, it collapsed 229,323 concurrent requests into the
197,786 queries that actually ran — **54% of load absorbed** by deduplication
alone, purely because Zipf skew means many simultaneous requests want the same
key.

### The stampede, measured

The claim in PLAN.md §5.2 is that concurrent misses for one key should collapse
into a single origin query. `make stampede` fires N simultaneous requests at a
freshly created code and reads `linkflow_origin_queries_total` either side:

| | simultaneous cold misses | Postgres queries |
|---|---:|---:|
| singleflight **on** | 216 | **1** |
| singleflight **off** | 251 | **242** |

Getting an honest number here took two fixes to the harness, both of which had
been quietly producing a passing result:

1. **The tool followed redirects**, so it reported example.com's 404 rather
   than our 302.
2. **It dialled connections after the barrier.** Establishing 10,000
   connections takes ~1.9 s, the first query finished long before the rest
   arrived, and the cache warmed — so the stampede never formed and *both*
   arms reported one query. Pre-establishing the connections is what made it a
   stampede.

The origin delay exists for the same reason: against a sub-millisecond
Postgres, the cache warms before a herd can form, and the test measures the
load generator instead of the service.

---

## Results — phase 4: Postgres vs ClickHouse ingest

Same load against both click event sinks, fresh volumes between arms. The
redirect path is identical in both; only where click events land changes.

| VUs | Postgres req/s | ClickHouse req/s | Postgres p99 | ClickHouse p99 |
|---:|---:|---:|---:|---:|
| 100 | *(outlier, see below)* | 30,710 | — | 8.99 ms |
| 200 | 32,868 | 30,884 | 16.23 ms | 16.75 ms |
| 400 | 31,240 | 26,011 | 35.16 ms | 41.32 ms |

**Throughput is not the story — Postgres is fine, and at 400 VUs it is
slightly ahead.** The story is what happened underneath:

| | Postgres | ClickHouse |
|---|---:|---:|
| Events accepted | 3,058,099 | 2,628,759 |
| Events written | 3,025,183 | 2,628,759 |
| **Events dropped (buffer full)** | **32,916 (1.08%)** | **0 (0.00%)** |
| Storage on disk | 440 MB | **7.33 MiB** |
| Bytes per event | ~145 | **~2.9** |

**Postgres could not drain the buffer fast enough and lost 1.08% of events.
ClickHouse lost none.** That is the ingest bottleneck the migration was for,
and it is visible only because the drop counters exist — throughput and latency
alone would have said Postgres was winning.

The storage difference is 50×. Same events, same count of columns: 440 MB
against 7.33 MiB. Columnar storage plus compression on data that is mostly
repeated codes and near-identical user agents is exactly the workload this
trade is designed for.

The rollup matters too. `click_counts_daily` is maintained on insert by a
materialized view, so `/api/links/{code}/stats` sums a handful of pre-aggregated
rows rather than scanning raw events. Counts always `SUM`: SummingMergeTree
collapses rows in the background, so before a merge one code legitimately has
several rows.

### The discarded data point

The first Postgres run reported 6,748 req/s with a **max latency of 138
seconds** — a single request that stalled for over two minutes. It is the first
k6 run after container start, and the methodology below says no warm-up is
discarded, so it landed in the data. It is excluded from the comparison above
as a cold-start artifact, and it is a direct argument for adding a discarded
warm-up period rather than pretending the number is meaningful.

---

### Note on the phase 1 numbers

An earlier phase 1 ramp in a separate session recorded higher sync throughput
(14.3k req/s at 25 VUs) than the 8.2k measured here. Same code, different
thermal and background-load conditions on a laptop. **Only the A/B above is a
valid comparison** — both arms ran back-to-back under identical conditions.
Cross-session absolute numbers on this hardware are not trustworthy, which is
itself an argument for the dedicated load-generation host described in
Methodology.

---

## Results — phase 5: creation without coordination

Three instances behind nginx, sharing one Postgres and one Redis. The question
is not throughput — it is whether the ID allocator's only coordination point,
a single database row, actually holds when three processes hammer it.

**20,000 links created concurrently through nginx, 100 concurrent creators:**

```
created:      20000 links in 1.674s (11947/s), 0 failed
unique codes: 20000 of 20000
code length:  3 chars: 4, 4 chars: 63, 5 chars: 4144, 6 chars: 15789
enumerable:   0 of 19999 consecutive pairs share a prefix (0.000%)
```

| Property | Result |
|---|---|
| Duplicate codes | **0** — with no collision check anywhere in the path |
| Database writes for 20,001 links | **3** (`id_blocks.next_id`: 1,000,000 → 1,030,000) |
| Consecutive codes sharing a prefix | **0 of 19,999** |
| Code issued by one replica, resolved through nginx | 60/60 → 302 |
| Load distribution across replicas | 100 / 100 / 100 |

**One database write per 10,000 links, and none at all in between.** Phase 1
took a round trip per creation; this takes one `UPDATE` per block and then
serves from an atomic cursor in memory. The row lock that `UPDATE` takes is the
entire cross-instance coordination mechanism.

Uniqueness holds with **no collision check, no retry loop and no lookup table**,
because there is nothing to collide: IDs are unique by allocation, and a
Feistel network is a bijection by construction, so distinct IDs cannot produce
the same code.

### Why a Feistel network rather than a hash

Block-allocated IDs are sequential, and base62 of a sequential ID is
sequential — `/aB3` and `/aB4` would both be real links, and anyone could walk
the keyspace reading other people's destinations. That is a security problem
created by a performance decision, which is what makes it interesting.

| Approach | Problem |
|---|---|
| Hash the ID | Reintroduces collisions, so back to a per-creation check |
| Random code + collision check | The database round trip the allocator just removed |
| Lookup table | The coordination the allocator just removed |
| **Feistel permutation** | Bijective by construction: unique, scattered, stateless |

Verified collision-free over a contiguous run of 2^20 IDs — the exact shape the
allocator produces — and invertible over 200,000 random values. Codes stay at
most 6 characters because the permuted domain is 2^32 and 62^6 > 2^32.

*Not measured:* whether three instances serve more redirect throughput than one.
That benchmark was started and stopped before it completed, so the scaling
claim is unverified and no number for it appears here.

---

## Architecture

```
                         ┌──────────────────────────┐
   GET /{code}  ───────► │  Go service              │
                         └────────────┬─────────────┘
                                      │
                    ┌─────────────────┴─────────────────┐
                    │                                   │
              READ PATH                            WRITE PATH
                    │                                   │
        ┌───────────▼───────────┐          ┌────────────▼────────────┐
        │ 1. Ristretto (local)  │          │ Bounded chan (10k)      │
        │ 2. Redis (shared)     │          │ non-blocking send       │
        │ 3. Postgres (origin)  │          │ full → drop + count     │
        │    singleflight on 3  │          │                         │
        └───────────┬───────────┘          └────────────┬────────────┘
                    │                                   │
                    │                       ┌───────────▼────────────┐
                    │                       │ Batcher goroutine      │
                    │                       │ flush on 1000 or 200ms │
                    │                       └───────────┬────────────┘
                    │                                   │
                    │                       ┌───────────▼────────────┐
                    │                       │ ClickHouse batch insert│
                    │                       │ (or Postgres COPY)     │
                    │                       └────────────────────────┘
                    │
              302 returned before the click event is durable
```

That last line is the design. The response goes out as soon as the destination
is known; at that moment the click event exists only in memory.

The full target shape is in [`PLAN.md`](PLAN.md) §4.

---

## Quick start

```bash
make up                          # Postgres + service, waits for health
curl -X POST localhost:8080/api/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://go.dev/blog/"}'
# {"code":"Q0u","short_url":"http://localhost:8080/Q0u",...}

curl -i localhost:8080/Q0u       # 302 Location: https://go.dev/blog/
curl localhost:8080/api/links/Q0u/stats
make stats                       # analytics counters, including drops
```

The stack includes Redis, ClickHouse, Prometheus and a provisioned Grafana
dashboard at `http://localhost:3000/d/linkflow-overview` (`make grafana`).

Every design claim has a switch so it can be measured rather than asserted:

```bash
make up ANALYTICS_MODE=sync           # phase 1 control: synchronous click writes
make up ANALYTICS_SINK=clickhouse     # phase 4: click events to ClickHouse
make stampede                         # 10k concurrent requests at a cold key
make cache-stats                      # per-tier hit counters
```

| Variable | Off value | What it isolates |
|---|---|---|
| `LINKFLOW_ANALYTICS_MODE` | `sync` | The decoupled write path |
| `LINKFLOW_ANALYTICS_SINK` | `postgres` | The ClickHouse migration |
| `LINKFLOW_LOCAL_CACHE_ITEMS` | `0` | The in-process tier |
| `LINKFLOW_REDIS_ADDR` | `""` | The shared tier |
| `LINKFLOW_SINGLEFLIGHT` | `false` | Stampede protection |
| `LINKFLOW_ORIGIN_DELAY` | `0` | Benchmark only: makes a stampede reproducible |

Port 5432 is usually taken by a local Postgres install, so compose publishes
the container on **5433** by default. Override with `LINKFLOW_POSTGRES_PORT`
and `LINKFLOW_PORT`.

### API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/links` | Create a short link from `{"url": "..."}` |
| `GET` | `/{code}` | 302 to the destination; records a click |
| `GET` | `/api/links/{code}` | Link metadata |
| `GET` | `/api/links/{code}/stats` | Click count |
| `GET` | `/healthz` | Liveness — does not touch Postgres |
| `GET` | `/readyz` | Readiness — pings Postgres |
| `GET` | `/debug/analytics` | Recorder counters: accepted, written, dropped |
| `GET` | `/metrics` | Prometheus exposition |

`healthz` and `readyz` are split on purpose: an instance that has lost its
database should be pulled from the load balancer, not restarted.

### Development

```bash
make check              # fmt + vet + unit tests
make test-db            # create linkflow_test in the compose Postgres
make test-integration   # tests that need a live database
```

---

## Design decisions

### Analytics must never break the redirect

`analytics.Recorder.Record` returns **nothing**. There is no error for the
redirect handler to check, because a failed click write is not a failed
redirect — the interface gives implementations no way to propagate failure onto
the hot path.

When the buffer fills, the choice was between three options:

1. **Block the redirect.** An analytics failure becomes a redirect outage.
2. **Spill to local disk.** Durable, but now you own a disk queue and its
   recovery logic.
3. **Drop the event and count it.** Redirects unaffected, analytics degraded
   and visibly so.

This takes option 3, in five lines:

```go
select {
case r.events <- ev:
default:
    r.counts.droppedBufferFull.Add(1)
}
```

That cost 0.157% of events under the heaviest load measured (4,857 of 3.09 M).
**It is the right trade for analytics and the wrong one for billing or fraud
detection**, where a dropped record costs money — there the answer is a durable
queue and backpressure that reaches the client, and the redirect latency budget
has to absorb it.

Each loss category is counted separately — buffer full, arrived during
shutdown, write failed — because a drop policy is only defensible while the
drops are *visible*. `/debug/analytics` exposes them until Prometheus lands in
phase 3.

### One goroutine owns the buffer

The batcher is the channel's only reader, so the batch slice needs no locking
and a flush is never concurrent with itself. The slice is truncated rather than
reallocated, so steady-state batching does not allocate.

Flushes use `context.Background()` with their own timeout, not the request
context. The request is long gone by the time a batch fills; inheriting its
context would cancel writes for no reason.

### Shutdown drains, in a specific order

`main.go` stops the HTTP server *before* closing the recorder, so the buffer
drains with nothing arriving behind it. Events that do arrive after close are
counted as `dropped_after_close` rather than lost silently. A `SIGKILL` still
loses whatever is buffered — that is the durability cost of returning the 302
early, and phase 5 optionally buys it back with Redis Streams.

### The allocator's cursor and bounds must be one object

The first version of the block allocator kept the cursor and the block end in
two separate atomics. That tears. A reader can observe the **new** end
alongside the **old** cursor and return an ID from the previous, exhausted
range — an ID that by then belongs to a different instance's block. The
symptom would be duplicate short codes appearing rarely, under load, across
instances: close to undebuggable in production.

Both now live in one immutable `block` swapped by a single pointer store, so
the window cannot exist. A 50-goroutine test with deliberately tiny blocks
failed on the first run and is what caught it.

IDs are deliberately **not dense**. Callers racing past the end of a block burn
the IDs they drew, and a restart abandons the rest of a block. Nothing requires
IDs to be contiguous, and the permutation scatters them before anyone sees
them.

### nginx: `proxy_pass` through a variable silently disables keepalive

The upstream was originally proxied through a variable so nginx would
re-resolve Docker DNS per request. That form bypasses the `upstream` block
entirely — which is where `keepalive` is configured — so nginx opened a fresh
TCP connection for every request. Measured cost: **1,645 req/s at p50 73 ms
with 3,120 sockets in TIME_WAIT**, against tens of thousands per second
directly against the service.

The named upstream resolves once at startup and expands every A record into a
round-robin peer. The trade is that scaling replicas needs an nginx reload; at
a fixed replica count that is obviously the right side of it.

### Only http and https destinations

`normalizeDestination` rejects everything else. A shortener that accepts
`javascript:` or `data:` URLs is an attack delivery mechanism aimed at whoever
trusts the short link.

### Redirects are not cacheable

Responses carry `Cache-Control: no-store, private`. A cached 302 means the
click never reaches the service, and analytics under-counts silently. Silent
under-counting is worse than visible dropping.

---

## Benchmark methodology

Reproduce with:

```bash
# phase 2 (decoupled write path)
make clean && make up ANALYTICS_MODE=batch
make seed SEED_COUNT=10000
make bench-ramp

# phase 1 control arm, fresh database so neither run inherits the other's
# table growth
make clean && make up ANALYTICS_MODE=sync
make seed SEED_COUNT=10000
make bench-ramp

# phase 3: the stampede claim, with and without the fix
LINKFLOW_ORIGIN_DELAY=5ms make up && make stampede
LINKFLOW_ORIGIN_DELAY=5ms LINKFLOW_SINGLEFLIGHT=false make up && make stampede

# phase 4: click events to ClickHouse instead of Postgres
make clean && make up ANALYTICS_SINK=clickhouse && make bench-ramp

# phase 5: three instances, then verify the allocator holds
make cluster REPLICAS=3
make allocator-check CREATE_N=20000
```

- **Zipf(s=1.2), not uniform.** Real link traffic is heavily skewed. A uniform
  draw over a large keyspace makes every request a miss, which both understates
  the system and would make the phase 3 cache tiers look pointless. Skew is
  what makes a local cache work at all, so measuring without it would be
  measuring the wrong system.
- **Redirects are not followed** (`redirects: 0`). Following them would measure
  example.com.
- **The key set is fixed and pre-seeded.** Sampling codes at random from an
  unknown keyspace would produce 404s and measure nothing.
- **p99.9 is reported next to p99.** A p99 alone hides the tail that actually
  hurts.

**Known flaw in the current numbers:** the load generator ran on the same host
as the service, which `PLAN.md` §5.5 explicitly warns against. k6 and the Go
service fought for the same cores. Every figure above is therefore a floor, and
cross-phase comparisons on this machine are meaningful in a way that the
absolute numbers are not. Moving load generation to a separate host is a
prerequisite for any number that goes on a resume.

---

## Failure modes

| Failure | Behaviour (batch mode) |
|---|---|
| Postgres unreachable | Redirects served from cache keep working until entries expire; anything that misses returns 500. `/readyz` fails, `/healthz` still passes, so a load balancer pulls the instance without a restart loop. |
| Redis unreachable | Counted as `linkflow_shared_cache_errors_total` and stepped past to the origin. Costs latency, not availability. |
| ClickHouse unreachable | Batches fail, events count as `dropped_write_failed`. Redirects unaffected. |
| Postgres slow *for writes* | Redirects unaffected. Batches stall, the buffer fills, and events start dropping with `dropped_buffer_full` climbing. Exactly the intended degradation. |
| Postgres slow *for reads* | Redirect latency degrades with it. Phase 3's cache tiers are the answer. |
| Click batch fails | All events in that batch are counted `dropped_write_failed`. No retry — a retry queue is unbounded work in front of an already-failing database. |
| Buffer fills | Overflow is dropped and counted. Redirects never block. |
| SIGTERM | HTTP drains first, then the buffer flushes within the shutdown timeout. Late arrivals count as `dropped_after_close`. Final tallies are logged. |
| SIGKILL | Up to 10,000 buffered events are lost, silently — the process is gone before it can count them. This is the price of returning the 302 early. |
| One instance dies | nginx routes around it. The dead instance abandons the unused remainder of its ID block; IDs are not required to be contiguous, so nothing else notices. |
| Two instances start against an empty database | Both run `IF NOT EXISTS` migrations concurrently. Idempotent, but unguarded — a versioned migration tool with an advisory lock is the correct answer. |
| nginx dies | Total outage. It is a single point of failure in this topology, unreplicated. |

---

## What I got wrong

`PLAN.md` §10 ends with "what did you get wrong the first time?" and notes that
the people who claim nothing are the ones who did not measure anything. Here is
the list, kept because the failure mode it illustrates is the important one:
**five of these bugs produced a plausible number rather than an error.**

### The harness lied before the service did

| Bug | What it reported | Why it was wrong |
|---|---|---|
| Stampede tool followed redirects | `404: 10000` | Go's HTTP client follows 302s by default, so it measured example.com, not us |
| Stampede tool dialled inside the measurement | "1 database query" — **with the fix disabled** | 10,000 connections take ~1.9 s to establish; the first query finished and warmed the cache before the herd formed |
| Empty env var treated as unset | Service exited on a run measuring life without Redis | `LINKFLOW_REDIS_ADDR=""` fell through to the `localhost` default, so "disable Redis" was inexpressible |
| Seeder wrote an empty key set anyway | **71,051 req/s** | Every creation had failed. The throughput was 100% errors against zero keys |
| No warm-up discarded | 6,748 req/s, max latency **138 seconds** | First run after container start; a cold-start artifact sitting in the results table |

The second one is the one worth dwelling on. It reported the *correct* answer —
one query — for the *wrong* reason, and it reported it for both the treated and
control arms. A test that passes when the feature is switched off is not
evidence of anything, and only checking the control arm exposed it.

### Two correctness bugs the tests caught

- **The allocator tore.** Cursor and block-end in separate atomics meant a
  reader could combine the new end with the old cursor and return an ID from
  another instance's block. Duplicate short codes, rarely, under load, across
  processes. A 50-goroutine test with tiny blocks failed immediately.
- **nginx opened a TCP connection per request.** `proxy_pass` through a
  variable bypasses the `upstream` block where `keepalive` lives. 1,645 req/s
  and 3,120 sockets in TIME_WAIT — visible only because the number was
  implausible enough to chase.

### And one that had nothing to do with measurement

`.gitignore` contained `linkflow` to ignore the built binary. Unanchored
patterns match at any depth, so it silently excluded the `cmd/linkflow/`
**source** directory. The repository was pushed without a `main` package and
could not have built. `git add cmd` had reported success.

### The pattern

Every one of these was caught by asking "is this number believable?" rather
than by a test failing. The lesson that generalises: **a benchmark harness
needs its own control arm, and a result that cannot fail is not a result.** The
codebase now refuses to produce several of these — the seeder will not write an
unusable key set, k6 will not start without one — but the general problem is
not solved by better assertions, only by suspicion.

---

## What I would do differently

Honest limitations of the current state:

- **The benchmark numbers do not yet meet their own standard.** Same-host load
  generation is a real methodological flaw, disclosed above rather than buried,
  and cross-session runs of the same code varied by ~40%. The A/B pairing is
  what rescues the phase 2 result; a dedicated load host is what would rescue
  the absolute numbers.
- **Buffer size, batch size and flush interval were not tuned.** They are the
  values PLAN.md suggested. Postgres dropped 1.08% of events at high ingest
  and a larger buffer might absorb that, at the cost of losing more on a
  crash — that trade has not been measured.
- **No warm-up is discarded**, which let a 138-second cold-start outlier into
  the phase 4 data. It is excluded and flagged, but the harness should be
  discarding a warm-up window instead of relying on me to notice.
- **The cache is not justified by this benchmark.** It costs throughput
  against a fast local origin and only pays when the origin is slow. Shipping
  it anyway is a bet about production, not a conclusion from the data — and
  the honest version of that sentence belongs in the README rather than a
  graph cropped to the case where it wins.
- **The two-tier result is sensitive to a number I chose**: 1,000 local
  entries against 10,000 keys. A different ratio moves the hit rate and the
  conclusion with it, and I have not swept it.
- **Analytics delivery is at-most-once.** Losses are counted, but a `SIGKILL`
  loses the buffer without counting it, so the drop tally is a floor rather
  than an exact figure.
- **A failed batch is dropped whole**, with no retry and no dead-letter path.
  For 1,000 events that is a bigger blast radius per failure than the
  synchronous recorder had.
- **Migrations run on boot with `IF NOT EXISTS`.** Fine for one instance,
  wrong for several starting concurrently against an empty database.
- **Horizontal scaling is unverified.** Three instances run correctly and
  share work evenly, but the 1-vs-3 throughput benchmark was never completed,
  so there is no number behind "it scales".
- **nginx is a single point of failure**, and the codec seed is a
  configuration value that silently invalidates every existing code if it
  changes.
- **Not production-ready, deliberately.** No auth on link creation, no rate
  limiting, no URL reputation checks. A public shortener with those gaps
  becomes a phishing relay within days; `PLAN.md` §7 rules that work out of
  scope on purpose, and the honest consequence is that this should not be
  exposed to the internet as it stands.
- Single region.

---

## What this project actually demonstrates

Not that a URL shortener can be fast — a single indexed lookup was always going
to be fast, which is why `PLAN.md` opens by saying the redirect is not the
achievement.

What it demonstrates is a read path held at sub-11 ms p99 while a
three-million-event write path runs underneath it, with the interference
between them removed deliberately and the cost of that removal stated as a
number rather than waved away. And, more usefully: three cases where running
the experiment produced the opposite of the expected answer, and the write-up
says so instead of quietly re-tuning until the graph agreed.

The cache section is the one worth reading. It cost 27% of throughput, and the
honest conclusion — that a cache is a bet about the origin's latency, and this
benchmark's origin was too fast for the bet to pay — is more useful than any
number where it wins.

---

## Reading order

1. [`PLAN.md`](PLAN.md) — the spec, the phase plan, and the reasoning behind
   every constraint above.
2. [`CLAUDE.md`](CLAUDE.md) — conventions and the layout of the packages.
3. The switches in [Quick start](#quick-start) — every claim above can be
   re-run with the feature turned off.
