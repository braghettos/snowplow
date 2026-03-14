import { test, expect } from '@playwright/test';
import { login } from '../lib/login';

const USERNAME = process.env.KRATEO_USERNAME ?? 'admin';
const PASSWORD = process.env.KRATEO_PASSWORD ?? 'IbZqduEGA2Sr';

test.describe('Authentication', () => {
  test('redirects to /login when unauthenticated', async ({ page }) => {
    await page.goto('/');
    await page.waitForURL('**/login', { timeout: 10_000 });
    await expect(page).toHaveURL(/\/login/);
    await expect(page.getByRole('button', { name: /sign in/i })).toBeVisible();
  });

  test('login page shows the expected form elements', async ({ page }) => {
    await page.goto('/login');
    await expect(page.getByRole('textbox', { name: /username/i })).toBeVisible();
    await expect(page.getByRole('textbox', { name: /password/i })).toBeVisible();
    await expect(page.getByRole('button', { name: /sign in/i })).toBeVisible();
    await expect(page.getByRole('link', { name: /forgot password/i })).toBeVisible();
  });

  test('logs in successfully with valid credentials', async ({ page }) => {
    await login(page, { username: USERNAME, password: PASSWORD });

    // Dashboard renders the main navigation items.
    await expect(page.getByRole('menuitem', { name: /dashboard/i })).toBeVisible();
    await expect(page.getByRole('menuitem', { name: /blueprints/i })).toBeVisible();
    await expect(page.getByRole('menuitem', { name: /compositions/i })).toBeVisible();
  });

  test('stays on /login with wrong credentials', async ({ page }) => {
    await page.goto('/login');

    await page.getByRole('textbox', { name: /username/i }).fill('admin');
    await page.getByRole('textbox', { name: /password/i }).fill('wrong-password');
    await page.getByRole('button', { name: /sign in/i }).click();

    // Should not navigate away from /login on failure.
    await page.waitForTimeout(3_000);
    await expect(page).toHaveURL(/\/login/);
  });

  test('session persists after page reload', async ({ page }) => {
    await login(page, { username: USERNAME, password: PASSWORD });

    await page.reload();
    await expect(page).toHaveURL(/\/dashboard/);
    await expect(page.getByRole('menuitem', { name: /dashboard/i })).toBeVisible();
  });
});
