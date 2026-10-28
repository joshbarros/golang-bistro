import { test, expect } from '@playwright/test';
import { ADMIN, adminToken, signIn } from './support';

test('add a sitting, buy covers, see the confirmation, refund it', async ({ page }) => {
  const run = Date.now().toString(36).toUpperCase();
  const code = `E2E-${run}`;
  const reference = `RSV-${run}`;

  await signIn(page, ADMIN);
  await page.getByLabel('Sitting code').fill(code);
  await page.getByLabel('Covers in the sitting').fill('8');
  await page.getByLabel('Limit per guest').fill('8');
  await page.getByRole('button', { name: 'Add sitting' }).click();

  const remaining = page.getByTestId('selected-remaining');
  await expect(page.getByRole('heading', { name: `Book covers for ${code}` })).toBeVisible();
  await expect(remaining).toHaveText('8');

  await page.getByLabel('Guest reference').fill(reference);
  await page.getByLabel('Covers', { exact: true }).fill('3');
  await page.getByRole('button', { name: 'Book' }).click();

  const row = page.locator(`tr[data-reference="${reference}"]`);
  await expect(row.getByTestId('status')).toHaveText('Sold', { timeout: 20_000 });
  await expect(row.getByTestId('confirmation')).toHaveText(/BT-[0-9A-F]{12}/);
  await expect(remaining).toHaveText('5');

  // Nothing is kept in the browser: a fresh load reads the same state from the API.
  // Nothing sensitive is stored either: the refresh cookie is HttpOnly, so a reload signs back in through it.
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).toBe(
    '{}',
  );
  await page.reload();
  await expect(page.getByTestId('whoami')).toContainText(ADMIN.email);
  await page.getByRole('button', { name: new RegExp(`^${code}`) }).click();
  const reloaded = page.locator(`tr[data-reference="${reference}"]`);
  await expect(reloaded.getByTestId('status')).toHaveText('Sold');
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');

  await reloaded.getByRole('button', { name: 'Refund' }).click();
  await expect(reloaded.getByTestId('status')).toHaveText('Refunded');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');
  // The issued confirmation is not rolled back.
  await expect(reloaded.getByTestId('confirmation')).toHaveText(/BT-[0-9A-F]{12}/);
  const headers = { Authorization: `Bearer ${await adminToken(page.request)}` };
  const orders = await page.request.get(`/api/orders?sittingCode=${code}`, { headers });
  const [order] = await orders.json();
  const confirmation = await page.request.get(`/api/confirmations/${order.id}`, { headers });
  expect((await confirmation.json()).status).toBe('Issued');
});
