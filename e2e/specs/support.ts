import { createHmac } from 'node:crypto';
import { APIRequestContext, Page, expect } from '@playwright/test';

export const ADMIN = {
  email: process.env.ADMIN_EMAIL ?? 'admin@sole.local',
  password: process.env.ADMIN_PASSWORD ?? 'SoleAdmin2026',
};

export const CUSTOMER_PASSWORD = 'Browser-Passw0rd1';

export async function signIn(page: Page, account: { email: string; password: string }) {
  await page.goto('/');
  await page.getByLabel('Email').fill(account.email);
  await page.getByLabel('Password').fill(account.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByTestId('whoami')).toContainText(account.email);
}

/** Registers a fresh customer through the form and leaves the page signed in as them. */
export async function registerCustomer(page: Page): Promise<string> {
  const email = `e2e-${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}@example.com`;
  await page.goto('/');
  await page.getByRole('button', { name: 'Need an account? Register' }).click();
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(CUSTOMER_PASSWORD);
  await page.getByRole('button', { name: 'Create account' }).click();
  await expect(page.getByTestId('whoami')).toContainText(email);
  return email;
}

/** A bearer token for direct API calls from a spec. */
export async function adminToken(request: APIRequestContext): Promise<string> {
  const response = await request.post('/api/auth/login', { data: ADMIN });
  expect(response.ok()).toBeTruthy();
  return (await response.json()).accessToken;
}

/** The body of POST /api/raffles: one place to adapt per system. */
export const raffleBody = (code: string, pairsTotal: number, drawInSeconds = 5) => ({
  code,
  name: 'Night Auth',
  colorway: 'Volt',
  brand: 'Nimbus',
  priceCents: 5000,
  pairsTotal,
  drawAt: new Date(Date.now() + drawInSeconds * 1000).toISOString(),
});

/** RFC 6238 (SHA-1, 30 seconds, 6 digits): enough to play the part of an authenticator app. */
export function totp(base32Key: string): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = 0;
  let value = 0;
  const bytes: number[] = [];
  for (const c of base32Key.replace(/=+$/, '').toUpperCase()) {
    value = (value << 5) | alphabet.indexOf(c);
    bits += 5;
    if (bits >= 8) {
      bytes.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 1000 / 30)));
  const hash = createHmac('sha1', Buffer.from(bytes)).update(counter).digest();
  const offset = hash[hash.length - 1] & 0x0f;
  const binary = hash.readUInt32BE(offset) & 0x7fffffff;
  return (binary % 1_000_000).toString().padStart(6, '0');
}

/** The newest link mailed to an address, read from the dev mailbox (the demo stack turns it on). */
export async function mailLink(request: APIRequestContext, email: string): Promise<string> {
  let link = '';
  await expect
    .poll(
      async () => {
        const response = await request.get(`/api/dev/mailbox?to=${encodeURIComponent(email)}`);
        const mails = response.ok() ? await response.json() : [];
        link = mails[0]?.link ?? '';
        return link;
      },
      { timeout: 15_000 },
    )
    .not.toBe('');
  return link;
}
