import { test, expect } from '@playwright/test';
import { ADMIN, signIn } from './support';

test('open a raffle, enter it, the draw runs by itself, see the claim, refund it', async ({
  page,
}) => {
  const run = Date.now().toString(36).toUpperCase();
  const code = `E2E-${run}`;
  const reference = `ENT-${run}`;

  await signIn(page, ADMIN);
  await page.getByLabel('Raffle code').fill(code);
  await page.getByLabel('Pairs on sale').fill('8');
  await page.getByLabel('Draw in (seconds)').fill('8');
  await page.getByRole('button', { name: 'Add raffle' }).click();

  const remaining = page.getByTestId('selected-remaining');
  await expect(page.getByRole('heading', { name: `Enter the raffle for ${code}` })).toBeVisible();
  await expect(remaining).toHaveText('8');
  await expect(page.getByTestId('draw-proof')).toContainText('the seed is revealed at the draw');
  await expect(page.getByTestId('draw-proof')).toContainText(/[0-9a-f]{64}/);

  await page.getByLabel('Entrant reference').fill(reference);
  await page.getByLabel('Pairs', { exact: true }).fill('3');
  await page.getByRole('button', { name: 'Enter', exact: true }).click();

  const row = page.locator(`tr[data-reference="${reference}"]`);
  await expect(row.getByTestId('status')).toHaveText('Entered', { timeout: 20_000 });
  await expect(remaining).toHaveText('8'); // entering takes no pairs
  await expect(row.getByTestId('claim')).toHaveText('—');

  await expect(row.getByTestId('status')).toHaveText('Sold', { timeout: 40_000 });
  await expect(row.getByTestId('claim')).toHaveText(/SL-[0-9A-F]{12}/);
  await expect(remaining).toHaveText('5');
  await expect(page.getByTestId('draw-proof')).toContainText(/seed [0-9a-f]{32}/);

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
});
