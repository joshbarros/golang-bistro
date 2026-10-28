import { test, expect, Page } from '@playwright/test';
import { ADMIN, adminToken, signIn, sittingBody, CUSTOMER_PASSWORD } from './support';

const run = () => Date.now().toString(36).toUpperCase() + Math.floor(Math.random() * 1000);

async function openAccount(
  page: Page,
  code: string,
  capacity: number,
  shown: string,
  limit = capacity,
) {
  await signIn(page, ADMIN);
  await page.getByLabel('Sitting code').fill(code);
  await page.getByLabel('Covers in the sitting').fill(String(capacity));
  await page.getByLabel('Limit per guest').fill(String(limit));
  await page.getByRole('button', { name: 'Add sitting' }).click();
  await expect(page.getByRole('heading', { name: `Book covers for ${code}` })).toBeVisible();
  await expect(page.getByTestId('selected-remaining')).toHaveText(shown);
}

async function submit(page: Page, reference: string, quantity: number) {
  await page.getByLabel('Guest reference').fill(reference);
  await page.getByLabel('Covers', { exact: true }).fill(String(quantity));
  await page.getByRole('button', { name: 'Book' }).click();
}

const row = (page: Page, reference: string) => page.locator(`tr[data-reference="${reference}"]`);

test('the API refuses bad input and the page says why', async ({ page }) => {
  const id = run();
  await openAccount(page, `UI-ERR-${id}`, 5, '5');

  await submit(page, `RSV-${id}`, 7);
  await expect(page.getByRole('alert')).toContainText('at most 6 covers');

  await submit(page, ' ', 1);
  await expect(page.getByRole('alert')).toContainText(/reference is required/i);

  await submit(page, `RSV-${id}`, 1);
  await expect(row(page, `RSV-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await submit(page, `RSV-${id}`, 1);
  await expect(page.getByRole('alert')).toContainText('already');
  await expect(page.locator('tbody tr[data-reference]')).toHaveCount(1);
});

test('a poison reference is rejected, gets no document, and the amount comes back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-POI-${id}`, 5, '5');

  await submit(page, `RSV-POISON-${id}`, 2);
  const poisoned = row(page, `RSV-POISON-${id}`);
  await expect(poisoned.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(poisoned).toContainText('poison');
  await expect(poisoned.getByTestId('confirmation')).toHaveText('—');
  await expect(poisoned.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');
});

test('the last cover goes to one request and the same guest asking again is held to the limit', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LAST-${id}`, 1, '1');

  await submit(page, `RSV-A-${id}`, 1);
  await submit(page, `RSV-B-${id}`, 1);

  const statuses = page.locator('tbody tr[data-reference] [data-testid="status"]');
  await expect(statuses).toHaveCount(2);
  await expect
    .poll(async () => (await statuses.allTextContents()).map((s) => s.trim()).sort(), {
      timeout: 20_000,
    })
    .toEqual(['Sold', 'Rejected'].sort());
  await expect(page.locator('tbody')).toContainText('per guest');
  await expect(page.getByTestId('selected-remaining')).toHaveText('0');
});

test('the event log shows the Kafka steps and the reversal keeps the document', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LOG-${id}`, 4, '4');

  await submit(page, `RSV-${id}`, 2);
  const mine = row(page, `RSV-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Sold', { timeout: 20_000 });

  await mine.getByRole('button', { name: 'Event log' }).click();
  const log = page.getByRole('region', { name: 'Kafka event log' });
  await expect(log).toContainText('covers.reserve', { timeout: 20_000 });
  await expect(log).toContainText('covers.held');
  await expect(log).toContainText('confirmation.issued');

  await mine.getByRole('button', { name: 'Refund' }).click();
  await expect(mine.getByTestId('status')).toHaveText('Refunded');
  await expect(mine.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(mine.getByTestId('confirmation')).toHaveText(/BT-[0-9A-F]{12}/);
  await expect(page.getByTestId('selected-remaining')).toHaveText('4');
  await expect(log).toContainText('covers.released', { timeout: 20_000 });
});

test('one guest cannot go past the limit per guest, and a refund gives the allowance back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-CAP-${id}`, 10, '10', 2);
  await expect(page.getByTestId(`remaining-UI-CAP-${id}`)).toContainText('max 2 each');

  await submit(page, `RSV-ONE-${id}`, 2);
  await expect(row(page, `RSV-ONE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });

  await submit(page, `RSV-TWO-${id}`, 1);
  const capped = row(page, `RSV-TWO-${id}`);
  await expect(capped.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(capped).toContainText('at most 2 covers per guest');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');

  await row(page, `RSV-ONE-${id}`).getByRole('button', { name: 'Refund' }).click();
  await expect(row(page, `RSV-ONE-${id}`).getByTestId('status')).toHaveText('Refunded', {
    timeout: 20_000,
  });
  await expect(page.getByTestId('selected-remaining')).toHaveText('10');

  await submit(page, `RSV-THREE-${id}`, 2);
  await expect(row(page, `RSV-THREE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
});

test('a full sitting lets a guest join the waitlist, and a cancellation seats them', async ({
  page,
  request,
}) => {
  const id = run();
  const code = `UI-WAIT-${id}`;
  const admin = { Authorization: `Bearer ${await adminToken(request)}` };
  expect(
    (await request.post('/api/sittings', { headers: admin, data: sittingBody(code, 2) })).status(),
  ).toBe(201);

  // Another guest takes the whole sitting through the API.
  const registered = await request.post('/api/auth/register', {
    data: { email: `wait-${id}@example.com`, password: CUSTOMER_PASSWORD },
  });
  const other = { Authorization: `Bearer ${(await registered.json()).accessToken}` };
  const taken = await request.post('/api/orders', {
    headers: other,
    data: { sittingCode: code, reference: `RSV-FIRST-${id}`, covers: 2 },
  });
  expect(taken.status()).toBe(202);
  const takenId = (await taken.json()).id;
  await expect
    .poll(
      async () =>
        (await (await request.get(`/api/orders/${takenId}`, { headers: other })).json()).status,
      { timeout: 20_000 },
    )
    .toBe('Sold');

  // The administrator asks for the same two covers and joins the line.
  await signIn(page, ADMIN);
  await page.getByRole('button', { name: new RegExp(`^${code}`) }).click();
  await page.getByLabel('Join the waitlist if the sitting is full').check();
  await submit(page, `RSV-WAIT-${id}`, 2);
  await expect(row(page, `RSV-WAIT-${id}`).getByTestId('status')).toHaveText('Waitlisted', {
    timeout: 20_000,
  });
  await expect(row(page, `RSV-WAIT-${id}`)).toContainText('number 1 in line');

  // The first guest cancels: the waiting party is seated without doing anything.
  expect((await request.post(`/api/orders/${takenId}/refund`, { headers: other })).status()).toBe(
    202,
  );
  await expect(row(page, `RSV-WAIT-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 30_000,
  });
  await expect(page.getByTestId('selected-remaining')).toHaveText('0');
});
