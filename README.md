# linkflow

A URL shortener where the redirect is the excuse and the write path is the project.

Every redirect generates an analytics write. If you insert that row before
responding, redirect latency is bound by a database write and those writes
contend with the reads that serve redirects — your fast read path is hostage to
your write path. **linkflow is a read path that stays fast while a high-volume,
loss-tolerant write path runs underneath it, with the two decoupled
deliberately and the trade-off measured.**

Built in phases, each ending with numbers. This README reports what has
actually been measured, not what the design is expected to achieve.

---

## Status

| Phase | What it adds | State |
|---|---|---|
| 1 | Baseline: Go + Postgres, synchronous click writes | **Done — the control arm** |
| 2 | Bounded channel + batcher goroutine, drop policy | **Done — numbers below** |
| 3 | Ristretto → Redis → Postgres, singleflight, Prometheus | Not started |
| 4 | ClickHouse for click events | Not started |
| 5 | Multi-instance, block ID allocator, durability | Not started |

Both analytics strategies ship in the same binary, selected by
`LINKFLOW_ANALYTICS_MODE`. Phase 1's synchronous recorder is not dead code — it
is the control arm, and keeping it means every phase comparison is one binary
on one machine in one session rather than two builds measured weeks apart.

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

### Note on the phase 1 numbers

An earlier phase 1 ramp in a separate session recorded higher sync throughput
(14.3k req/s at 25 VUs) than the 8.2k measured here. Same code, different
thermal and background-load conditions on a laptop. **Only the A/B above is a
valid comparison** — both arms ran back-to-back under identical conditions.
Cross-session absolute numbers on this hardware are not trustworthy, which is
itself an argument for the dedicated load-generation host described in
Methodology.

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
        │ Postgres              │          │ Bounded chan (10k)      │
        │ point lookup on code  │          │ non-blocking send       │
        │ source of truth       │          │ full → drop + count     │
        └───────────┬───────────┘          └────────────┬────────────┘
                    │                                   │
                    │                       ┌───────────▼────────────┐
                    │                       │ Batcher goroutine      │
                    │                       │ flush on 1000 or 200ms │
                    │                       └───────────┬────────────┘
                    │                                   │
                    │                       ┌───────────▼────────────┐
                    │                       │ Postgres COPY          │
                    │                       │ (ClickHouse in ph. 4)  │
                    │                       └────────────────────────┘
                    │
              302 returned before the click event is durable
```

That last line is the design. The response goes out as soon as the destination
is known; at that moment the click event exists only in memory. Phase 3 adds
the cache tiers on the left, phase 4 replaces the sink on the right.

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

To run the phase 1 control arm instead of the decoupled write path:

```bash
make up ANALYTICS_MODE=sync
```

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

### Short codes are enumerable, and that is a known phase 1 debt

Codes come from a Postgres sequence, base62-encoded. This costs a round trip
per creation and means `/aB3` and `/aB4` are both real links — anyone can walk
the keyspace and read other people's destinations.

Both problems have the same fix (`PLAN.md` §5.4): pre-allocated ID blocks
remove the per-creation coordination, and a Feistel bijection applied before
base62 scatters the output so sequential IDs stop producing sequential codes.
`CreateLink` already takes the encoder as a function argument, so that lands
without touching SQL.

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
| Postgres unreachable | Redirects 500 — no cache yet, so the read path has nothing to fall back on. `/readyz` fails, `/healthz` still passes, so a load balancer pulls the instance without a restart loop. |
| Postgres slow *for writes* | Redirects unaffected. Batches stall, the buffer fills, and events start dropping with `dropped_buffer_full` climbing. Exactly the intended degradation. |
| Postgres slow *for reads* | Redirect latency degrades with it. Phase 3's cache tiers are the answer. |
| Click batch fails | All events in that batch are counted `dropped_write_failed`. No retry — a retry queue is unbounded work in front of an already-failing database. |
| Buffer fills | Overflow is dropped and counted. Redirects never block. |
| SIGTERM | HTTP drains first, then the buffer flushes within the shutdown timeout. Late arrivals count as `dropped_after_close`. Final tallies are logged. |
| SIGKILL | Up to 10,000 buffered events are lost, silently — the process is gone before it can count them. This is the price of returning the 302 early. |

---

## What I would do differently

Honest limitations of the current state:

- **The benchmark numbers do not yet meet their own standard.** Same-host load
  generation is a real methodological flaw, disclosed above rather than buried,
  and cross-session runs of the same code varied by ~40%. The A/B pairing is
  what rescues the phase 2 result; a dedicated load host is what would rescue
  the absolute numbers.
- **Buffer size, batch size and flush interval were not tuned.** They are the
  values PLAN.md suggested. The 0.157% drop rate might well go to zero with a
  larger buffer, at the cost of losing more on a crash — that trade has not
  been measured.
- **No caching at all**, so the read path is one Postgres query per redirect
  and nothing protects the database from a hot key. This is now the dominant
  bottleneck — with the write path decoupled, the remaining ceiling is read
  contention on Postgres.
- **Analytics delivery is at-most-once.** Losses are counted, but a `SIGKILL`
  loses the buffer without counting it, so the drop tally is a floor rather
  than an exact figure.
- **A failed batch is dropped whole**, with no retry and no dead-letter path.
  For 1,000 events that is a bigger blast radius per failure than the
  synchronous recorder had.
- **Codes are enumerable.** See above.
- **Migrations run on boot with `IF NOT EXISTS`.** Fine for one instance,
  wrong for several starting concurrently against an empty database.
- Single region, no rate limiting, no auth on link creation.

---

## Reading order

1. [`PLAN.md`](PLAN.md) — the spec, the phase plan, and the reasoning behind
   every constraint above.
2. [`CLAUDE.md`](CLAUDE.md) — conventions and the layout of the packages.
