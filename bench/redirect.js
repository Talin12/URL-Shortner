// k6 redirect benchmark.
//
// Two things here matter more than the throughput number they produce:
//
//   1. Keys are drawn from a Zipf distribution, not uniformly. Real link
//      traffic is heavily skewed, and a uniform draw over a large keyspace
//      makes every request a cache miss -- which would both understate the
//      system and make the cache tiers added in phase 3 look useless.
//   2. Redirects are not followed. We are measuring this service, not
//      example.com.
//
// Usage:
//   go run ./bench/seed -count 10000
//   cd bench && k6 run redirect.js   (or: make bench)
//
// Environment overrides: BASE_URL, VUS, DURATION, ZIPF_S, CODES.

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const CODES_FILE = __ENV.CODES || './codes.json';
const ZIPF_S = parseFloat(__ENV.ZIPF_S || '1.2');
const VUS = parseInt(__ENV.VUS || '100', 10);
const DURATION = __ENV.DURATION || '60s';

// Init context: this runs once per VU but the file is read and parsed by k6
// only once and shared, so the CDF build below is the only per-VU cost.
const codes = JSON.parse(open(CODES_FILE));

// Inverse-transform sampling needs a cumulative distribution. Zipf weight for
// rank i is 1/i^s; normalising gives the CDF we binary-search per request.
const cdf = buildZipfCDF(codes.length, ZIPF_S);

const redirectDuration = new Trend('redirect_duration', true);
const notFound = new Counter('redirect_not_found');

export const options = {
  scenarios: {
    redirect: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
    },
  },
  // p99 and p99.9 are not in k6's default trend stats, and PLAN.md asks for
  // both -- a p99 without a p99.9 next to it hides the tail that actually
  // hurts users.
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
  // Redirects are the product; following them would measure the wrong host.
  noVUConnectionReuse: false,
  discardResponseBodies: true,
  thresholds: {
    // A failing threshold marks the run non-zero-exit, so a regression in CI
    // is loud rather than something you have to eyeball in the summary.
    'http_req_failed{expected_response:true}': ['rate<0.01'],
    'redirect_duration': ['p(99)<50'],
  },
};

export default function () {
  const code = codes[sampleZipf()];
  const res = http.get(`${BASE_URL}/${code}`, {
    redirects: 0,
    tags: { name: 'redirect' },
  });

  redirectDuration.add(res.timings.duration);
  if (res.status === 404) {
    notFound.add(1);
  }
  check(res, { 'status is 302': (r) => r.status === 302 });
}

function buildZipfCDF(n, s) {
  const table = new Float64Array(n);
  let total = 0;
  for (let i = 0; i < n; i++) {
    total += 1 / Math.pow(i + 1, s);
    table[i] = total;
  }
  for (let i = 0; i < n; i++) {
    table[i] /= total;
  }
  return table;
}

// Binary search the CDF for a uniform draw. O(log n) per request, which stays
// well under the cost of the HTTP call itself.
function sampleZipf() {
  const u = Math.random();
  let lo = 0;
  let hi = cdf.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (cdf[mid] < u) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  return lo;
}

export function handleSummary(data) {
  // Print the context alongside the numbers. A latency figure without the
  // distribution, concurrency and duration it was taken under is not a result.
  const m = data.metrics;
  const p = (metric, stat) => {
    const v = m[metric] && m[metric].values[stat];
    return typeof v === 'number' ? v.toFixed(2) : 'n/a';
  };

  const report = [
    '',
    '=== linkflow redirect benchmark ===',
    `Keys:         ${codes.length} codes, Zipf(s=${ZIPF_S})`,
    `Concurrency:  ${VUS} VUs`,
    `Duration:     ${DURATION}`,
    `Throughput:   ${p('http_reqs', 'rate')} req/s`,
    `Latency:      p50 ${p('redirect_duration', 'med')}ms  p95 ${p('redirect_duration', 'p(95)')}ms  ` +
      `p99 ${p('redirect_duration', 'p(99)')}ms  p99.9 ${p('redirect_duration', 'p(99.9)')}ms  max ${p('redirect_duration', 'max')}ms`,
    `Errors:       ${m.http_req_failed ? (m.http_req_failed.values.rate * 100).toFixed(2) : 'n/a'}%`,
    `404s:         ${m.redirect_not_found ? m.redirect_not_found.values.count : 0}`,
    '',
    'Record the hardware and where the load generator ran alongside this.',
    '',
  ].join('\n');

  return {
    stdout: report,
    'results/latest.json': JSON.stringify(data, null, 2),
  };
}
