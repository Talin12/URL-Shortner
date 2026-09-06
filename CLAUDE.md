# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

A Go link-redirection service. Read `PLAN.md` first — it is the spec, and it is
opinionated in ways that change what "correct" means here.

The one-line version: **the redirect is the excuse, the write path is the
project.** A latency-critical read path (resolve code → 302) and a
volume-heavy, loss-tolerant write path (click events) share a process without
interfering, and the trade-off is measured rather than asserted.

Two consequences that matter for almost every change:

- **The redirect must never fail or block because analytics failed.** Dropping
  click events under backpressure is a deliberate design choice, not a bug
  (`PLAN.md` §5.1). Do not "fix" it by making the redirect wait.
- **Numbers must be measured, never estimated.** Every performance claim in the
  README is a benchmark that was actually run, reported with its hardware, key
  distribution, concurrency, and duration.

## Build phase status

`PLAN.md` §6 defines six phases. Current state: **phase 1 (baseline) complete.**

Phase 1 is *deliberately naive* in three places. If you find yourself about to
"improve" one of these without being asked, check the phase plan first — the
naive version exists so later phases have a before-number to beat:

| Deliberately naive in phase 1 | Replaced by |
|---|---|
| Synchronous click insert on the redirect path | Phase 2: bounded channel + batcher goroutine |
| No caching; every redirect hits Postgres | Phase 3: Ristretto → Redis → Postgres, with singleflight |
| `nextval()` per creation, sequential/enumerable codes | Later: block allocator + Feistel bijection (§5.4) |

## Commands

```bash
make help              # list targets
make up                # docker compose: Postgres + service, waits for health
make down              # stop, keep the Postgres volume
make clean             # stop, drop the volume, remove bench artifacts

make check             # fmt + vet + test -- run this before committing
make test              # unit tests, no external dependencies
go test ./internal/shortcode/ -run TestEncodeDecodeRoundTrip -v   # single test
go test ./internal/shortcode/ -bench=. -benchmem                  # Go microbenchmarks
```

Integration tests are behind a build tag and need a live Postgres. They skip
silently when `LINKFLOW_TEST_DATABASE_URL` is unset, which is why plain
`go test ./...` stays dependency-free:

```bash
make test-db           # create the linkflow_test database in the compose Postgres
make test-integration  # runs with -tags=integration
```

### Benchmarking

```bash
make seed              # create SEED_COUNT links, write bench/codes.json
make bench             # k6, Zipf(s=1.2), VUS/DURATION overridable
make bench-ramp        # sweep 25→800 VUs to find the knee
```

`make bench` requires the seeded key set — k6 samples codes from
`bench/codes.json`, so seeding is not optional. Overrides:
`make bench VUS=400 DURATION=120s ZIPF_S=1.4`.

## Architecture

```
cmd/linkflow/          entrypoint: config → store → recorder → API → graceful shutdown
internal/config/       env-var config, defaults matching docker-compose
internal/store/        all Postgres access; owns the embedded schema
internal/shortcode/    base62 encode/decode
internal/analytics/    Recorder interface + phase 1 synchronous implementation
internal/httpapi/      routes, validation, the redirect hot path
bench/seed/            Go tool that populates links and writes codes.json
bench/redirect.js      k6 script, Zipfian key sampling
```

The pieces that are load-bearing across files:

**`analytics.Recorder` is the phase seam.** The redirect handler calls
`Record(ctx, ev)` and never learns whether that is a synchronous insert or a
non-blocking channel send. `Record` returns nothing on purpose — there is no
error for the redirect path to handle, because analytics failing is not a
redirect failure. Phase 2 is a new `Recorder` implementation plus a wiring
change in `main.go`, not a rewrite of the handler.

**`store` owns the schema, and it is embedded.** `internal/store/schema/*.sql`
is compiled into the binary and applied by `Migrate` on every boot. All
statements are `IF NOT EXISTS`, so this is idempotent; a multi-instance
deployment would need a versioned migration tool with advisory locking.

**`CreateLink` takes an `encode func(uint64) string`.** Base62 lives in
`shortcode`, not in the storage layer, so §5.4's Feistel bijection slots in by
passing a different function rather than by editing SQL.

**Shutdown order in `main.go` is deliberate**: stop accepting HTTP first, then
close the recorder. Phase 1's recorder has nothing to flush; the ordering
exists so phase 2's buffer drains with no new events arriving behind it.

## Conventions

- **stdlib routing** (`net/http` with Go 1.22+ patterns). No Gin, no chi
  wrapper — `PLAN.md` §3 rules this out explicitly.
- **pgx/v5 native pool**, not `database/sql`.
- Errors from `store` wrap with `%w` and are matched with `errors.Is`;
  `store.ErrNotFound` is the sentinel the HTTP layer maps to 404.
- Comments explain *why* a choice was made, especially where the naive-looking
  code is intentional. Do not add comments that restate the code.
- Redirect responses set `Cache-Control: no-store` — a cached 302 means clicks
  never reach the service and analytics silently under-counts.
- Only `http` and `https` destinations are accepted (`normalizeDestination`);
  anything else turns the redirect into a vector against whoever clicks it.

## Scope

`PLAN.md` §7 lists what this project deliberately does **not** build: user
accounts, auth, a management dashboard, custom domains, QR codes, link expiry
UI, a polished frontend. These are CRUD and add nothing. Do not add them
without being asked, even if they seem like obvious gaps.
