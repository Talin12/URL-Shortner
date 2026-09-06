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
| 1 | Baseline: Go + Postgres, synchronous click writes | **Done — numbers below** |
| 2 | Bounded channel + batcher goroutine, drop policy | Not started |
| 3 | Ristretto → Redis → Postgres, singleflight, Prometheus | Not started |
| 4 | ClickHouse for click events | Not started |
| 5 | Multi-instance, block ID allocator, durability | Not started |

Phase 1 is deliberately naive: no cache, one Postgres round trip per redirect,
and a synchronous click insert on the hot path. That is the point. Without a
credible *before*, none of the later phases have anything to be measured
against.

---

## Results — phase 1 baseline

```
Hardware:     Apple M4, 10 cores, 16 GB. Service in Docker Desktop (10 CPU / 8 GB VM).
Caveat:       Load generator ran on the SAME machine as the service. See "Methodology".
Distribution: Zipf(s=1.2) over 10,000 keys
Duration:     30 s per step, no warm-up discarded
Analytics:    Synchronous INSERT per redirect (phase 1 baseline)
```

### Concurrency ramp — finding the knee

| VUs | Throughput | p50 | p95 | p99 | p99.9 | Errors |
|---:|---:|---:|---:|---:|---:|---:|
| 25  | **14,334 req/s** | 1.58 ms | 2.64 ms | **4.04 ms** | 14.26 ms | 0.00% |
| 50  | 12,834 req/s | 3.46 ms | 6.34 ms | 10.41 ms | 25.87 ms | 0.00% |
| 100 | 11,033 req/s | 8.38 ms | 13.86 ms | 20.76 ms | 52.35 ms | 0.00% |
| 200 | 7,247 req/s | 22.37 ms | 56.03 ms | 106.16 ms | 172.50 ms | 0.00% |
| 400 | 2,843 req/s | 103.75 ms | 310.05 ms | 702.59 ms | 1133.31 ms | 0.00% |
| 800 | 5,371 req/s | 112.37 ms | 335.37 ms | 655.21 ms | 834.00 ms | 0.00% |

**The knee is at or below 25 VUs.** Throughput does not rise with concurrency
here — it *falls*, monotonically, while p99 climbs 160x. There is no ceiling to
find, because the system is already past saturation at the lowest step
measured. Real capacity for this baseline is **~14.3k req/s at p99 4 ms**;
everything to the right of that row is a system already broken, and the 800-VU
row reading higher than the 400-VU row is run-to-run noise under contention,
not a recovery.

Two causes, and the whole point of phase 1 is that they are separable:

1. **Every redirect pays for a synchronous `INSERT` plus index maintenance.**
   The read path is bound by the write path, exactly as predicted.
2. **Load generator and service shared 10 cores.** More concurrency meant more
   k6 threads stealing CPU from the thing being measured.

Cause 2 is a methodology flaw to fix. Cause 1 is the project. Phase 2 targets
cause 1 and will be measured on this same machine, so the comparison holds even
though the absolute numbers do not travel.

*(Not probed below 25 VUs — the true peak may sit lower still. Worth a
follow-up sweep at 1/2/5/10 VUs.)*

### Sustained run at 100 VUs, 60 s

```
Throughput:   12,303 req/s
Latency:      p50 6.91ms  p95 14.20ms  p99 25.74ms  max 158.25ms
Errors:       0.00%  (738,243 requests, 0 failures)
```

**Read these as an upper bound on the baseline, not as the system's capacity.**
The load generator and the service competed for the same 10 cores, so both
throughput and latency here are pessimistic — and unusable as a headline
number. They are still a valid *baseline*, because phase 2 will be measured on
the same machine under the same contention.

After the full benchmark session, `click_events` held **2.5 M rows in 364 MB** —
every one of them written synchronously, on the redirect path, with an index to
maintain on each insert. That table is the phase 2 argument in one line.

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
        │ Postgres              │          │ Postgres                │
        │ point lookup on code  │          │ INSERT per redirect      │
        │ source of truth       │          │ ON THE HOT PATH          │
        └───────────────────────┘          └─────────────────────────┘

   Phase 1: both paths share one storage engine and one code path.
   That is exactly the coupling phases 2-4 exist to break.
```

The target shape, for reference, is in [`PLAN.md`](PLAN.md) §4: Ristretto →
Redis → Postgres on the read side, and a bounded channel → batcher goroutine →
ClickHouse on the write side, with the 302 returned before the click event is
durable anywhere.

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

`analytics.Recorder` has a `Record` method that returns **nothing**. There is
no error for the redirect handler to check, because a failed click write is not
a failed redirect. Phase 1's implementation logs and swallows; phase 2's will
drop the event and increment a counter. Neither can propagate failure upward,
because the interface gives them no way to.

That is a deliberate availability choice: analytics is the subsystem allowed to
fail. **It would be the wrong choice for billing or fraud events**, where a
dropped record costs money — there the right answer is a durable queue and
backpressure that reaches the client.

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
make up
make seed SEED_COUNT=10000
make bench VUS=100 DURATION=60s     # single point
make bench-ramp                     # 25 → 800 VUs sweep
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

| Failure | Phase 1 behaviour |
|---|---|
| Postgres unreachable | Total outage. Redirects 500, `/readyz` fails, `/healthz` still passes. |
| Postgres slow | Redirect latency degrades with it — there is no cache to absorb it. |
| Click insert fails | Logged and swallowed. The redirect already returned 302. |
| Process killed mid-request | In-flight requests are lost; nothing is buffered, so nothing else is. |
| SIGTERM | HTTP server drains, then the recorder closes. Phase 1 has no buffer to flush; the ordering exists for phase 2. |

---

## What I would do differently

Honest limitations of the current state:

- **The benchmark numbers do not yet meet their own standard.** Same-host load
  generation is a real methodological flaw, disclosed above rather than
  buried.
- **No caching at all**, so the read path is one Postgres query per redirect
  and nothing protects the database from a hot key.
- **Analytics delivery is at-most-once**, and phase 1 does not even count what
  it loses — the failure is logged, not measured. The drop counter arrives in
  phase 2.
- **Codes are enumerable.** See above.
- **Migrations run on boot with `IF NOT EXISTS`.** Fine for one instance,
  wrong for several starting concurrently against an empty database.
- Single region, no rate limiting, no auth on link creation.

---

## Reading order

1. [`PLAN.md`](PLAN.md) — the spec, the phase plan, and the reasoning behind
   every constraint above.
2. [`CLAUDE.md`](CLAUDE.md) — conventions and the layout of the packages.
