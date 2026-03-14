/**
 * Snowplow performance benchmark.
 *
 * Measures latency of typical Kubernetes API calls through snowplow
 * under two conditions: cache ENABLED and cache DISABLED.
 *
 * Usage:
 *   npx ts-node bench/perf.ts
 *
 * Environment variables:
 *   FRONTEND_URL    default http://localhost:30080
 *   SNOWPLOW_URL    default http://localhost:30081
 *   AUTHN_URL       default http://localhost:30082
 *   USERNAME        default admin
 *   PASSWORD        (required)
 *   ITERATIONS      number of requests per endpoint per run, default 30
 */

const FRONTEND_URL = process.env.FRONTEND_URL ?? 'http://localhost:30080';
const SNOWPLOW    = process.env.SNOWPLOW_URL  ?? 'http://localhost:30081';
const AUTHN       = process.env.AUTHN_URL     ?? 'http://localhost:30082';
const USERNAME    = process.env.USERNAME       ?? 'admin';
const PASSWORD    = process.env.PASSWORD       ?? 'agPEjgMStMYH';
const ITERATIONS  = parseInt(process.env.ITERATIONS ?? '30', 10);

// Endpoints to benchmark: representative mix of GET calls the portal makes.
const ENDPOINTS = [
  { label: 'Page (dashboard)',        path: '/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages&name=dashboard-page&namespace=krateo-system' },
  { label: 'Page (blueprints)',       path: '/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages&name=blueprints-page&namespace=krateo-system' },
  { label: 'Page (compositions)',     path: '/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages&name=compositions-page&namespace=krateo-system' },
  { label: 'NavMenu',                 path: '/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=navmenus&name=sidebar-nav-menu&namespace=krateo-system' },
  { label: 'RESTAction (all-routes)', path: '/call?apiVersion=templates.krateo.io/v1&resource=restactions&name=all-routes&namespace=krateo-system' },
  { label: 'RESTAction (bp-list)',    path: '/call?apiVersion=templates.krateo.io/v1&resource=restactions&name=blueprints-list&namespace=krateo-system' },
  { label: 'ConfigMap list (ns)',     path: '/call?apiVersion=v1&resource=configmaps&namespace=krateo-system' },
];

interface Stats {
  min: number; max: number; mean: number; p50: number; p90: number; p99: number;
  samples: number[];
}

function percentile(sorted: number[], p: number): number {
  const idx = Math.ceil((p / 100) * sorted.length) - 1;
  return sorted[Math.max(0, idx)];
}

function stats(samples: number[]): Stats {
  const sorted = [...samples].sort((a, b) => a - b);
  const mean = samples.reduce((a, b) => a + b, 0) / samples.length;
  return {
    min:     sorted[0],
    max:     sorted[sorted.length - 1],
    mean:    Math.round(mean),
    p50:     percentile(sorted, 50),
    p90:     percentile(sorted, 90),
    p99:     percentile(sorted, 99),
    samples,
  };
}

async function getToken(): Promise<string> {
  const creds = Buffer.from(`${USERNAME}:${PASSWORD}`).toString('base64');
  const r = await fetch(`${AUTHN}/basic/login`, {
    headers: { Authorization: `Basic ${creds}` },
  });
  if (!r.ok) throw new Error(`authn login failed: ${r.status}`);
  const body = await r.json() as { accessToken: string };
  return body.accessToken;
}

async function benchmarkEndpoint(
  path: string, token: string, iterations: number,
): Promise<number[]> {
  const samples: number[] = [];
  for (let i = 0; i < iterations; i++) {
    const start = performance.now();
    const r = await fetch(`${SNOWPLOW}${path}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    await r.text(); // drain body
    const elapsed = Math.round(performance.now() - start);
    samples.push(elapsed);
    // small gap to avoid saturating snowplow
    await new Promise(res => setTimeout(res, 20));
  }
  return samples;
}

interface RunResult {
  label: string;
  stats: Stats;
}

async function runBenchmark(label: string): Promise<RunResult[]> {
  console.log(`\n${'='.repeat(60)}`);
  console.log(`▶  Run: ${label}`);
  console.log('='.repeat(60));

  const token = await getToken();
  const results: RunResult[] = [];

  for (const ep of ENDPOINTS) {
    process.stdout.write(`  ${ep.label.padEnd(30)} ... `);
    const samples = await benchmarkEndpoint(ep.path, token, ITERATIONS);
    const s = stats(samples);
    results.push({ label: ep.label, stats: s });
    console.log(`mean=${s.mean}ms  p50=${s.p50}ms  p90=${s.p90}ms  p99=${s.p99}ms  [${s.min}–${s.max}]`);
  }
  return results;
}

function printComparison(withCache: RunResult[], withoutCache: RunResult[]) {
  const COL = [32, 8, 8, 8, 8, 8, 8, 8, 10];
  const H   = ['Endpoint', 'cached\nmin', 'cached\np50', 'cached\np90', 'cached\np99',
                'nocache\nmin', 'nocache\np50', 'nocache\np90', 'speedup\np50'];

  console.log(`\n${'━'.repeat(100)}`);
  console.log('  COMPARISON (all times in ms)');
  console.log('━'.repeat(100));

  const hdr = H.map((h, i) => h.split('\n').pop()!.padStart(COL[i])).join(' ');
  const hdr2 = H.map((h, i) => h.split('\n')[0].padStart(COL[i])).join(' ');
  console.log(hdr2);
  console.log(hdr);
  console.log('─'.repeat(100));

  for (let i = 0; i < withCache.length; i++) {
    const c  = withCache[i].stats;
    const nc = withoutCache[i].stats;
    const speedup = nc.p50 > 0 ? (nc.p50 / c.p50).toFixed(1) : 'N/A';
    const row = [
      withCache[i].label.padEnd(COL[0]),
      String(c.min).padStart(COL[1]),
      String(c.p50).padStart(COL[2]),
      String(c.p90).padStart(COL[3]),
      String(c.p99).padStart(COL[4]),
      String(nc.min).padStart(COL[5]),
      String(nc.p50).padStart(COL[6]),
      String(nc.p90).padStart(COL[7]),
      `${speedup}x`.padStart(COL[8]),
    ].join(' ');
    console.log(row);
  }
  console.log('─'.repeat(100));

  // Overall summary
  const cMean  = Math.round(withCache.reduce((s, r) => s + r.stats.mean, 0) / withCache.length);
  const ncMean = Math.round(withoutCache.reduce((s, r) => s + r.stats.mean, 0) / withoutCache.length);
  const totalSpeedup = (ncMean / cMean).toFixed(1);
  console.log(`\n  Overall mean latency:  CACHED=${cMean}ms  NO-CACHE=${ncMean}ms  SPEEDUP=${totalSpeedup}x`);
  console.log('━'.repeat(100));
}

async function waitForSnowplow(timeoutMs = 60_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`${SNOWPLOW}/health`);
      if (r.ok) return;
    } catch { /* ignore */ }
    await new Promise(res => setTimeout(res, 2000));
  }
  throw new Error('Snowplow did not become ready in time');
}

// ── Main ─────────────────────────────────────────────────────────────────────
(async () => {
  console.log(`\nSnowplow performance benchmark`);
  console.log(`  Snowplow: ${SNOWPLOW}`);
  console.log(`  Iterations per endpoint: ${ITERATIONS}`);
  console.log(`  Endpoints: ${ENDPOINTS.length}`);

  // ── Phase 1: with cache ───────────────────────────────────────────────────
  console.log('\n[1/2] Benchmarking with cache ENABLED...');
  const withCache = await runBenchmark('Cache ENABLED');

  // ── Phase 2: disable cache (remove Redis sidecar) ─────────────────────────
  console.log('\n[2/2] Disabling Redis sidecar and restarting snowplow...');
  const { execSync } = require('child_process') as typeof import('child_process');

  execSync(`kubectl patch deployment snowplow -n krateo-system --type=json -p='[
    {"op":"remove","path":"/spec/template/spec/initContainers"}
  ]'`, { stdio: 'inherit' });

  console.log('  Waiting for rollout...');
  execSync('kubectl rollout status deployment/snowplow -n krateo-system --timeout=120s', { stdio: 'inherit' });

  console.log('  Waiting for snowplow health...');
  await waitForSnowplow(60_000);
  // Extra wait to let snowplow settle (no warmup needed when cache is off)
  await new Promise(res => setTimeout(res, 3_000));

  const withoutCache = await runBenchmark('Cache DISABLED');

  // ── Phase 3: restore Redis sidecar ───────────────────────────────────────
  console.log('\n[3/3] Restoring Redis sidecar...');
  execSync(`kubectl patch deployment snowplow -n krateo-system --type=strategic -p='{
    "spec": {
      "template": {
        "spec": {
          "initContainers": [{
            "name": "redis",
            "image": "redis:7-alpine",
            "restartPolicy": "Always",
            "ports": [{"containerPort": 6379}],
            "readinessProbe": {
              "exec": {"command": ["redis-cli", "ping"]},
              "initialDelaySeconds": 2,
              "periodSeconds": 3
            },
            "resources": {
              "requests": {"cpu": "50m", "memory": "64Mi"},
              "limits": {"cpu": "200m", "memory": "128Mi"}
            }
          }]
        }
      }
    }
  }'`, { stdio: 'inherit' });

  // ── Results ───────────────────────────────────────────────────────────────
  printComparison(withCache, withoutCache);
})().catch(err => { console.error(err); process.exit(1); });
