import { createHash } from 'node:crypto';
import { test, expect, Page } from '@playwright/test';
import { ADMIN, CUSTOMER_PASSWORD, adminToken, raffleBody, signIn } from './support';

const run = () => Date.now().toString(36).toUpperCase() + Math.floor(Math.random() * 1000);
const sha256 = (text: string) => createHash('sha256').update(text).digest('hex');

async function openRaffle(page: Page, code: string, pairs: number, shown: string, drawIn = 8) {
  await signIn(page, ADMIN);
  await page.getByLabel('Raffle code').fill(code);
  await page.getByLabel('Pairs on sale').fill(String(pairs));
  await page.getByLabel('Draw in (seconds)').fill(String(drawIn));
  await page.getByRole('button', { name: 'Add raffle' }).click();
  await expect(page.getByRole('heading', { name: `Enter the raffle for ${code}` })).toBeVisible();
  await expect(page.getByTestId('selected-remaining')).toHaveText(shown);
}

async function submit(page: Page, reference: string, quantity: number) {
  await page.getByLabel('Entrant reference').fill(reference);
  await page.getByLabel('Pairs', { exact: true }).fill(String(quantity));
  await page.getByRole('button', { name: 'Enter', exact: true }).click();
}

const row = (page: Page, reference: string) => page.locator(`tr[data-reference="${reference}"]`);

test('the API refuses bad input and the page says why; one entry per person', async ({ page }) => {
  const id = run();
  await openRaffle(page, `UI-ERR-${id}`, 5, '5', 3600);

  await submit(page, `ENT-${id}`, 7);
  await expect(page.getByRole('alert')).toContainText('at most 6 pairs');

  await submit(page, ' ', 1);
  await expect(page.getByRole('alert')).toContainText(/reference is required/i);

  await submit(page, `ENT-${id}`, 1);
  await expect(row(page, `ENT-${id}`).getByTestId('status')).toHaveText('Entered', {
    timeout: 20_000,
  });
  await submit(page, `ENT-${id}`, 1);
  await expect(page.getByRole('alert')).toContainText('already');

  // The same person under another reference: accepted at the door, refused by the raffle.
  await submit(page, `ENT-SECOND-${id}`, 1);
  await expect(row(page, `ENT-SECOND-${id}`).getByTestId('status')).toHaveText('Rejected', {
    timeout: 20_000,
  });
  await expect(row(page, `ENT-SECOND-${id}`)).toContainText('one entry per person');
  await expect(page.locator('tbody tr[data-reference]')).toHaveCount(2);
});

test('a poison reference that wins is rejected, gets no document, and the pairs come back', async ({
  page,
}) => {
  const id = run();
  await openRaffle(page, `UI-POI-${id}`, 5, '5', 6);

  await submit(page, `ENT-POISON-${id}`, 2);
  const poisoned = row(page, `ENT-POISON-${id}`);
  await expect(poisoned.getByTestId('status')).toHaveText('Entered', { timeout: 20_000 });
  await expect(poisoned.getByTestId('status')).toHaveText('Rejected', { timeout: 40_000 });
  await expect(poisoned).toContainText('poison');
  await expect(poisoned.getByTestId('claim')).toHaveText('—');
  await expect(poisoned.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');
});

test('a draw you can check: the seed matches its commitment and the winner follows from it', async ({
  page,
  request,
}) => {
  const id = run();
  const code = `UI-FAIR-${id}`;
  const admin = { Authorization: `Bearer ${await adminToken(request)}` };
  expect(
    (await request.post('/api/raffles', { headers: admin, data: raffleBody(code, 1, 7) })).status(),
  ).toBe(201);
  const before = await (await request.get(`/api/raffles/${code}`, { headers: admin })).json();
  expect(before.seed).toBeNull(); // only the commitment is public before the draw

  // A second person enters through the API; the administrator enters through the page. One pair, two entrants.
  const registered = await request.post('/api/auth/register', {
    data: { email: `fair-${id}@example.com`, password: CUSTOMER_PASSWORD },
  });
  const other = { Authorization: `Bearer ${(await registered.json()).accessToken}` };
  const theirs = await (
    await request.post('/api/orders', {
      headers: other,
      data: { raffleCode: code, reference: `ENT-B-${id}`, pairs: 1 },
    })
  ).json();

  await signIn(page, ADMIN);
  await page.getByRole('button', { name: new RegExp(`^${code}`) }).click();
  await submit(page, `ENT-A-${id}`, 1);
  const mine = row(page, `ENT-A-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Entered', { timeout: 20_000 });

  // After the draw exactly one of them holds the pair and the other was not drawn.
  await expect(mine.getByTestId('status')).toHaveText(/Sold|Rejected/, { timeout: 40_000 });
  await expect
    .poll(
      async () =>
        (await (await request.get(`/api/orders/${theirs.id}`, { headers: other })).json()).status,
      { timeout: 40_000 },
    )
    .toMatch(/Sold|Rejected/);

  const after = await (await request.get(`/api/raffles/${code}`, { headers: admin })).json();
  expect(after.status).toBe('Drawn');
  expect(sha256(after.seed)).toBe(before.seedHash); // the seed is the one that was committed

  const draw = await (await request.get(`/api/raffles/${code}/draw`, { headers: admin })).json();
  expect(draw.seedMatchesHash).toBe(true);
  expect(draw.entries).toBe(2);
  const ordered = [...draw.mine].sort((a: { key: string }, b: { key: string }) =>
    a.key < b.key ? -1 : 1,
  );
  for (const entry of ordered) expect(sha256(`${after.seed}:${entry.orderId}`)).toBe(entry.key);
  expect(ordered[0].outcome).toBe('Won'); // the lowest key is drawn first and takes the only pair
  expect(ordered[1].outcome).toBe('Lost');
  expect(draw.winners).toBe(1);
  await expect(page.getByTestId('draw-proof')).toContainText(after.seed);
});

test('an entry can be withdrawn before the draw, and then it cannot win', async ({ page }) => {
  const id = run();
  await openRaffle(page, `UI-OUT-${id}`, 3, '3', 3600);

  await submit(page, `ENT-${id}`, 2);
  const mine = row(page, `ENT-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Entered', { timeout: 20_000 });
  await mine.getByRole('button', { name: 'Withdraw' }).click();
  await expect(mine.getByTestId('status')).toHaveText('Refunded', { timeout: 20_000 });
  await expect(mine.getByRole('button', { name: 'Withdraw' })).toHaveCount(0);
  await expect(page.getByTestId('selected-remaining')).toHaveText('3');
});

test('the event log shows the Kafka steps and the refund keeps the document', async ({ page }) => {
  const id = run();
  await openRaffle(page, `UI-LOG-${id}`, 4, '4', 6);

  await submit(page, `ENT-${id}`, 2);
  const mine = row(page, `ENT-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Sold', { timeout: 40_000 });

  await mine.getByRole('button', { name: 'Event log' }).click();
  const log = page.getByRole('region', { name: 'Kafka event log' });
  await expect(log).toContainText('pairs.reserve', { timeout: 20_000 });
  await expect(log).toContainText('pairs.entered');
  await expect(log).toContainText('pairs.held');
  await expect(log).toContainText('claim.issued');

  await mine.getByRole('button', { name: 'Refund' }).click();
  await expect(mine.getByTestId('status')).toHaveText('Refunded');
  await expect(mine.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(mine.getByTestId('claim')).toHaveText(/SL-[0-9A-F]{12}/);
  await expect(page.getByTestId('selected-remaining')).toHaveText('4');
  await expect(log).toContainText('pairs.released', { timeout: 20_000 });
});
