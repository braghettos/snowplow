/**
 * Cache verification suite.
 *
 * Verifies that after a login + dashboard load, the snowplow /metrics/cache
 * endpoint shows cache hits and no cache misses on a second identical request
 * (i.e., the cache is warm and every repeat call is a hit).
 */
import { test, expect, request } from '@playwright/test';
import { login } from '../lib/login';

const USERNAME  = process.env.KRATEO_USERNAME  ?? 'admin';
const PASSWORD  = process.env.KRATEO_PASSWORD  ?? 'IbZqduEGA2Sr';
const SNOWPLOW  = process.env.SNOWPLOW_URL     ?? 'http://136.111.232.159:8081';

interface CacheMetrics {
  get_hits:         number;
  get_misses:       number;
  list_hits:        number;
  list_misses:      number;
  rbac_hits:        number;
  rbac_misses:      number;
  raw_hits:         number;
  raw_misses:       number;
  negative_hits:    number;
  expiry_refreshes: number;
  get_hit_rate:     number;
  list_hit_rate:    number;
  rbac_hit_rate:    number;
}

async function fetchMetrics(apiCtx: Awaited<ReturnType<typeof request.newContext>>): Promise<CacheMetrics> {
  const res = await apiCtx.get(`${SNOWPLOW}/metrics/cache`);
  expect(res.ok(), `GET ${SNOWPLOW}/metrics/cache returned ${res.status()}`).toBeTruthy();
  return res.json();
}

test.describe('Snowplow cache — login flow', () => {

  test('first login populates the cache (misses then hits on repeat)', async ({ page, playwright }) => {
    const api = await request.newContext();

    // ── Baseline: metrics must start at zero for a fresh snowplow ──────────
    const before = await fetchMetrics(api);
    console.log('Metrics BEFORE first login:', JSON.stringify(before, null, 2));

    // ── First login: populates the cache ───────────────────────────────────
    await login(page, { username: USERNAME, password: PASSWORD });
    // Wait for the dashboard to fully render (widgets make several API calls).
    await page.waitForTimeout(5_000);

    const afterFirstLogin = await fetchMetrics(api);
    console.log('Metrics AFTER first login:', JSON.stringify(afterFirstLogin, null, 2));

    const firstLoginTotal =
      afterFirstLogin.get_misses  + afterFirstLogin.list_misses +
      afterFirstLogin.rbac_misses + afterFirstLogin.raw_misses;

    console.log(`Total misses after first login: ${firstLoginTotal}`);

    // The first login WILL have cache misses (cold start) — that is expected.
    // Assert that at least some cache activity happened.
    const firstLoginActivity =
      afterFirstLogin.get_hits   + afterFirstLogin.get_misses  +
      afterFirstLogin.list_hits  + afterFirstLogin.list_misses +
      afterFirstLogin.rbac_hits  + afterFirstLogin.rbac_misses +
      afterFirstLogin.raw_hits   + afterFirstLogin.raw_misses;
    expect(firstLoginActivity, 'Expected cache activity after first login').toBeGreaterThan(0);

    // ── Second login: everything should come from cache (hits only) ─────────
    await page.goto('/login');
    await login(page, { username: USERNAME, password: PASSWORD });
    await page.waitForTimeout(5_000);

    const afterSecondLogin = await fetchMetrics(api);
    console.log('Metrics AFTER second login:', JSON.stringify(afterSecondLogin, null, 2));

    const newMisses =
      (afterSecondLogin.get_misses  - afterFirstLogin.get_misses) +
      (afterSecondLogin.list_misses - afterFirstLogin.list_misses) +
      (afterSecondLogin.rbac_misses - afterFirstLogin.rbac_misses) +
      (afterSecondLogin.raw_misses  - afterFirstLogin.raw_misses);

    const newHits =
      (afterSecondLogin.get_hits  - afterFirstLogin.get_hits) +
      (afterSecondLogin.list_hits - afterFirstLogin.list_hits) +
      (afterSecondLogin.rbac_hits - afterFirstLogin.rbac_hits) +
      (afterSecondLogin.raw_hits  - afterFirstLogin.raw_hits);

    console.log(`New hits on second login: ${newHits}`);
    console.log(`New misses on second login: ${newMisses}`);

    expect(newHits, 'Second login must generate cache hits').toBeGreaterThan(0);
    expect(newMisses, 'Second login must generate zero new cache misses').toBe(0);

    await api.dispose();
  });

  test('incremental hit rate is 100% on every login after cache is warm', async ({ page }) => {
    const api = await request.newContext();

    // First login — expected to produce cold misses (cache empty).
    await login(page, { username: USERNAME, password: PASSWORD });
    await page.waitForTimeout(5_000);
    const afterWarm = await fetchMetrics(api);
    console.log('Metrics after warm-up login:', JSON.stringify(afterWarm, null, 2));

    // Second login — cache should be fully warm; measure incremental activity.
    await page.goto('/login');
    await login(page, { username: USERNAME, password: PASSWORD });
    await page.waitForTimeout(5_000);
    const afterSecond = await fetchMetrics(api);
    console.log('Metrics after second login:', JSON.stringify(afterSecond, null, 2));

    const deltaHits =
      (afterSecond.get_hits  - afterWarm.get_hits) +
      (afterSecond.rbac_hits - afterWarm.rbac_hits) +
      (afterSecond.raw_hits  - afterWarm.raw_hits);

    const deltaMisses =
      (afterSecond.get_misses  - afterWarm.get_misses) +
      (afterSecond.rbac_misses - afterWarm.rbac_misses) +
      (afterSecond.raw_misses  - afterWarm.raw_misses);

    const deltaHitRate = deltaHits + deltaMisses > 0
      ? (deltaHits / (deltaHits + deltaMisses)) * 100
      : 0;

    console.log(`Incremental hits: ${deltaHits}, misses: ${deltaMisses}, hit rate: ${deltaHitRate.toFixed(1)}%`);

    expect(deltaHits,   'Second login must produce cache hits').toBeGreaterThan(0);
    expect(deltaMisses, 'Second login must produce zero new cache misses').toBe(0);
    expect(deltaHitRate,'Incremental hit rate on second login').toBe(100);

    await api.dispose();
  });
});
