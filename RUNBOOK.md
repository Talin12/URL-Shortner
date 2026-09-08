# Cloud benchmark runbook

How to run linkflow's benchmarks on two hosts instead of one, and how to close
the last open measurement gap: **does three instances serve more redirect
throughput than one?**

This procedure exists to fix two things at once, and they are the same fix:

1. **The disclosed methodological flaw.** Every number in the README was
   produced with k6 and the service fighting for the same ten cores
   (`README.md` § "Benchmark methodology", `PLAN.md` §5.5). The README says
   plainly that "moving load generation to a separate host is a prerequisite
   for any number that goes on a resume."
2. **The unverified scaling claim.** `README.md` § "What I would do
   differently" records that the 1-vs-3 throughput benchmark was started and
   stopped before it completed, so no scaling number is claimed anywhere.

Read `README.md` § "What I got wrong" before you start. Five separate harness
bugs on this project produced *plausible numbers rather than errors*, including
a stampede test that reported the correct answer with the feature disabled.
This runbook is written defensively because of that history.

---

## The mistake this benchmark is most likely to make

A single Go process will happily saturate every core you give it. Put one
replica on an 8-core box and it can use all 8; add two more replicas and they
share the same 8 cores. Throughput does not move, and you conclude "it does not
scale" — when what you actually measured was the host's ceiling.

**So the CPU budget per replica must be fixed, and the host must be big enough
that three constrained replicas still fit.** Every arm below gives each replica
exactly 2 cores. One replica gets 2, three replicas get 6, and the difference
between them is a property of the architecture rather than of the box.

This is the whole reason the benchmark needs deliberate setup rather than just
a bigger machine.

---

## Topology

```
        ┌────────────────────────┐          ┌────────────────────────┐
        │  LOAD HOST             │ private  │  SERVICE HOST          │
        │  8 vCPU / 8 GB         │  network │  8 vCPU / 16 GB        │
        │                        │─────────▶│                        │
        │  k6                    │  :8080   │  nginx                 │
        │  bench/seed            │          │  linkflow ×N (2 cpu ea)│
        │  bench/allocator       │          │  postgres, redis       │
        │  bench/stampede        │          │  clickhouse, prometheus│
        └────────────────────────┘          └────────────────────────┘
             no service code                   no load generator
```

Both hosts in the **same region and availability zone**, talking over **private
networking**. Cross-region latency would swamp a p99 measured in single-digit
milliseconds.

### Sizing rationale

| Host | Spec | Why |
|---|---|---|
| Service | 8 vCPU / 16 GB | 3 replicas × 2 cores = 6, leaving ~2 for Postgres, Redis, nginx and Prometheus |
| Load | 8 vCPU / 8 GB | Must be provably *not* the bottleneck. Control C2 checks this rather than assuming it |

Do not undersize the load host to save money. A load generator that saturates
first turns every number into a measurement of k6.

---

## Step 1 — Provision and lock down

Any provider with private networking works. Two Ubuntu 24.04 instances.

**The service host must not be reachable from the internet on 8080.** This is
not a general security nag — `README.md` § "What I would do differently" states
it directly: there is no auth on link creation, no rate limiting and no URL
reputation checking, so a publicly reachable instance becomes a phishing relay.
`PLAN.md` §7 rules that work out of scope on purpose.

Firewall the service host to:

- **22/tcp** from your IP only
- **8080/tcp** from the *load host's private IP only*
- everything else denied inbound

```bash
# On the SERVICE host. Replace 10.0.0.3 with the load host's PRIVATE ip.
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow from <YOUR.PUBLIC.IP> to any port 22 proto tcp
sudo ufw allow from 10.0.0.3 to any port 8080 proto tcp
sudo ufw enable
sudo ufw status verbose
```

Verify from your laptop that it is genuinely closed — a firewall you did not
test is a firewall you are guessing about:

```bash
curl -m 5 http://<SERVICE.PUBLIC.IP>:8080/healthz   # must time out or refuse
```

---

## Step 2 — Install

**Service host** — Docker only:

```bash
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER" && newgrp docker
git clone <your-repo-url> linkflow && cd linkflow
```

**Load host** — k6 and Go, no Docker needed:

```bash
sudo gpg -k
sudo gpg --no-default-keyring --keyring /usr/share/keyrings/k6-archive-keyring.gpg \
  --keyserver hkp://keyserver.ubuntu.com:80 --recv-keys C5AD17C747E3415A3642D57D77C6C491D6AC1D69
echo "deb [signed-by=/usr/share/keyrings/k6-archive-keyring.gpg] https://dl.k6.io/deb stable main" \
  | sudo tee /etc/apt/sources.list.d/k6.list
sudo apt-get update && sudo apt-get install -y k6 golang-go git
git clone <your-repo-url> linkflow && cd linkflow
```

Set the target once, on the load host, and use it everywhere below:

```bash
export SVC=http://10.0.0.2:8080     # the service host's PRIVATE ip
```

Every bench tool already takes this — `bench/redirect.js` reads `BASE_URL` from
the environment, and `seed`, `stampede` and `allocator` each take `-base-url`.
No source changes are needed to drive the stack remotely.

---

## Step 3 — Add the CPU-budget overlay

On the **service host**, create `docker-compose.bench.yml`. This is the file
that makes the scaling question answerable, per "The mistake this benchmark is
most likely to make" above.

```yaml
# docker-compose.bench.yml -- benchmark-only overlay.
#
# Fixes the CPU budget per replica so that 1-vs-3 measures the architecture
# rather than the host's core count. Without it, one replica expands to fill
# the whole box and three replicas show no gain for the wrong reason.
services:
  linkflow:
    environment:
      # The load-bearing limit: caps the Go runtime's parallelism regardless
      # of how the container's CPU quota is enforced.
      GOMAXPROCS: "${LINKFLOW_GOMAXPROCS:-2}"
    deploy:
      resources:
        limits:
          # Belt and braces against the cgroup, verified in step 4.
          cpus: "${LINKFLOW_CPUS:-2.0}"
```

---

## Step 4 — Bring up an arm, and verify the budget actually applied

```bash
# On the SERVICE host. N is 1 or 3 depending on the arm.
docker compose -f docker-compose.yml \
               -f docker-compose.cluster.yml \
               -f docker-compose.bench.yml \
               up --build -d --scale linkflow=3

# Grafana is not needed during a measured run; give the cores back.
docker compose stop grafana
```

**Verify rather than assume.** `deploy.resources` is silently ignored by some
compose versions, which would put you straight back into the failure this
overlay exists to prevent:

```bash
docker inspect $(docker compose ps -q linkflow) \
  --format '{{.Name}} NanoCpus={{.HostConfig.NanoCpus}}'
docker inspect $(docker compose ps -q linkflow) | grep -o 'GOMAXPROCS=[0-9]*'
```

Expect one line per replica, and the two limits to agree: `NanoCpus` of
`2000000000` alongside `GOMAXPROCS=2` for a 2-core arm, `6000000000` alongside
`GOMAXPROCS=6` for the 6-core **U** arm. If `NanoCpus` is `0`, the cgroup limit
did not apply — `GOMAXPROCS` alone still constrains the Go scheduler, so the arm
is usable, but say so when you report it.

---

## Step 5 — Run the control arms *first*

Do these before the measured arms. Their whole purpose is to fail: a control
that cannot fail is not evidence, which is the lesson `README.md` §"The pattern"
draws from the stampede test that passed with the feature switched off.

### C1 — Reproduce the co-hosting flaw (expected: clearly worse)

Run the 1-replica arm twice, once from each host.

```bash
# From the LOAD host (the real measurement)
make seed BASE_URL=$SVC SEED_COUNT=10000
make bench BASE_URL=$SVC VUS=100 DURATION=60s

# From the SERVICE host, k6 installed there temporarily, same VUs/duration
make bench BASE_URL=http://localhost:8080 VUS=100 DURATION=60s
```

**Expected:** the co-hosted run is materially slower. That delta *is* the
quantified version of the flaw the README discloses, and it belongs in the
write-up.

**If the two are within noise, stop.** Either the load host is the bottleneck
(C2 will say so) or the service is limited by something other than CPU —
Postgres, nginx, or the network. In that case the 1-vs-3 test is about to
measure that other thing instead of scaling. Find it before continuing.

Remove k6 from the service host afterwards.

### C2 — Prove the load generator is not the ceiling (expected: no change)

During the highest-throughput run, watch the load host:

```bash
# On the LOAD host, in a second session during the run
top -bn3 | grep -E "^%Cpu|k6"
```

If the load host is above ~70% CPU, it is a candidate bottleneck and your
result is a **floor, not a measurement.** The rigorous version is a second load
host splitting the same total VUs: if the combined number rises, one k6 host
was the limit and every arm needs re-running.

### C3 — Prove nginx is not the ceiling (expected: no change)

The 3-replica number cannot exceed what nginx can proxy, and this project has
already been bitten here once: `proxy_pass` through a variable bypassed the
`upstream` block's `keepalive`, producing 1,645 req/s and 3,120 sockets in
`TIME_WAIT` (`README.md` § "Two correctness bugs the tests caught").

Check for the regression while a run is in flight:

```bash
# On the SERVICE host, during the run
ss -s | grep -i timewait
```

A large and climbing `TIME_WAIT` count means connections are not being reused
and you are measuring socket churn. Then compare one replica through nginx
against the same replica hit directly, by temporarily publishing its port on
the private interface.

---

## Step 6 — The measured arms

Three arms. Between each one, drop the volume and re-seed, so that no arm
inherits another's table growth — the same discipline the README's methodology
section already applies between phases.

| Arm | Replicas | Cores each | Total service cores | What it answers |
|---|---|---:|---:|---|
| **A1** | 1 | 2 | 2 | The baseline |
| **A3** | 3 | 2 | 6 | Does it scale? |
| **U** | 1 | 6 | 6 | Upper bound: the same CPU in one process, with no cross-instance hops |

The **U** arm is what makes this result falsifiable. Read the outcome like
this:

- **A3 ≈ U** — it scales cleanly; distribution costs essentially nothing.
- **A3 < U** — it scales, but coordination (shared Postgres and Redis, the
  extra nginx hop) takes a measurable cut. Report the size of the cut; that is
  a more interesting finding than a bare speedup.
- **A3 > U** — **this is a measurement error.** Three constrained replicas
  cannot beat the same total CPU in one process with fewer network hops. Do not
  publish it; go and find the bug.

### Per-arm procedure

```bash
# --- SERVICE host: reset and start the arm -------------------------------
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
               -f docker-compose.bench.yml down -v

LINKFLOW_GOMAXPROCS=2 LINKFLOW_CPUS=2.0 \
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
               -f docker-compose.bench.yml up --build -d --scale linkflow=3
docker compose stop grafana
# Re-run the step 4 verification here. Every time.

# --- LOAD host: seed, discard a warm-up, then measure --------------------
make seed BASE_URL=$SVC SEED_COUNT=10000

# The k6 script uses a constant-vus executor with no warm-up stage, so the
# warm-up has to be a discarded pass. Phase 4 let a 138-second cold-start
# outlier into the results for exactly this reason.
make bench BASE_URL=$SVC VUS=100 DURATION=30s    # DISCARD this output

make bench-ramp BASE_URL=$SVC                    # 25 -> 800 VUs, the measured run
```

For the **U** arm, use `--scale linkflow=1` with `LINKFLOW_GOMAXPROCS=6
LINKFLOW_CPUS=6.0`. For **A1**, `--scale linkflow=1` at the standard 2 cores.
Keep every arm behind nginx so the proxy hop is constant and not a hidden
variable between arms.

Do not change `LINKFLOW_CODE_SEED` at any point. It is identical across
replicas by way of `docker-compose.cluster.yml`, and changing it invalidates
every code already in `bench/codes.json`.

### Report the knee, not the peak

`PLAN.md` §5.5 is explicit: ramp concurrency, plot p99 against throughput, and
report the inflection where throughput stops rising and p99 starts climbing.
"62k req/s at p99 under 10 ms" is a number with meaning; "peak 90k req/s" beside
a 400 ms p99 describes a system that is already broken.

---

## Step 7 — Collect, per arm

```bash
# From the LOAD host
curl -sS $SVC/debug/analytics | python3 -m json.tool          # drop counters
curl -sS $SVC/metrics | grep -E "^linkflow_(cache_lookups|origin_queries|singleflight|shared_cache)" | grep -v '^#'
```

Check the drop counters on every arm. Phase 4's finding was that throughput and
latency both looked healthy while Postgres was quietly dropping 1.08% of click
events — **only the drop counters exposed it.** A scaling number recorded
without them is incomplete.

While you have the cluster up, re-confirm the phase 5 allocator result now that
the replicas are on a properly sized host:

```bash
make allocator-check BASE_URL=$SVC CREATE_N=20000
```

---

## Step 8 — Record it

Fill in the block `PLAN.md` §5.5 prescribes, per arm, and update the two places
in `README.md` that currently disclose these gaps: the "Known flaw in the
current numbers" paragraph under Benchmark methodology, and the "Horizontal
scaling is unverified" bullet under What I would do differently.

```
Hardware:     <service host spec>, service and load generator on SEPARATE hosts
              <load host spec>, same region/AZ, private networking
Replicas:     <1 | 3> at GOMAXPROCS=2, cpus=2.0 each
Distribution: Zipf(s=1.2) over 10,000 keys
Concurrency:  <VUs at the knee>
Duration:     30 s per ramp step, one 30 s warm-up pass discarded
Throughput:   <req/s at the knee>
Latency:      p50 <> p95 <> p99 <> p99.9 <>
Errors:       <%>
Cache:        local <%> | redis <%> | postgres <%>
Drops:        <from /debug/analytics, all categories>
Controls:     C1 co-hosted delta <>, C2 load host peak CPU <%>, C3 TIME_WAIT <>
```

Report the control results alongside the headline number. On this project they
are not an appendix — the README's credibility rests on the fact that its
failures are visible.

---

## Step 9 — Destroy the hosts

Billing is hourly and the service has no auth. Do not leave it running.

```bash
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
               -f docker-compose.bench.yml down -v
```

Then destroy both instances at the provider. Copy `bench/results` and every k6
summary off the load host first.

**Cost:** two 8-vCPU instances for an afternoon runs to a few dollars on most
providers at current hourly rates — check your provider's pricing rather than
trusting this line. The dominant risk is forgetting to destroy them, not the
hourly rate.

---

## What this still will not prove

Stated up front so it does not get quietly overclaimed later:

- **Single region, single AZ.** Nothing here says anything about geographic
  distribution.
- **nginx remains a single point of failure**, unreplicated, and is now also a
  measured component of every arm.
- **Postgres and Redis are shared and unconstrained** across replicas. If the
  A3 arm falls short of U, a saturated shared dependency is the first
  hypothesis to test — that is a finding about this topology, not a law about
  the architecture.
- **Buffer, batch size and flush interval are still untuned** at PLAN.md's
  suggested values, and this procedure does not sweep them.
- **At-most-once analytics is unchanged.** A `SIGKILL` still loses the buffer
  without counting it, so every drop tally is a floor.
