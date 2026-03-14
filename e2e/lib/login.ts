import { Page, expect } from '@playwright/test';

export interface LoginOptions {
  username: string;
  password: string;
}

/**
 * Navigates to the login page and performs a full sign-in flow.
 * Asserts that the browser lands on /dashboard after a successful login.
 */
export async function login(page: Page, { username, password }: LoginOptions): Promise<void> {
  await page.goto('/login');

  // Wait for the form to be interactive.
  const usernameInput = page.getByRole('textbox', { name: /username/i });
  const passwordInput = page.getByRole('textbox', { name: /password/i });
  const signInButton = page.getByRole('button', { name: /sign in/i });

  await expect(usernameInput).toBeVisible();
  await expect(signInButton).toBeVisible();

  await usernameInput.fill(username);
  await passwordInput.fill(password);
  await signInButton.click();

  // After a successful login the app redirects to /dashboard.
  await page.waitForURL('**/dashboard', { timeout: 15_000 });
  await expect(page).toHaveURL(/\/dashboard/);
}
