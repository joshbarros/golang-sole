import { test, expect } from '@playwright/test';
import { CUSTOMER_PASSWORD, mailLink, registerCustomer, totp } from './support';

const NEW_PASSWORD = 'Another-Passw0rd9';

test('a forgotten password is reset through the emailed link, and the old one stops working', async ({
  page,
  request,
}) => {
  const email = await registerCustomer(page);
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();

  await page.getByRole('button', { name: 'Forgot your password?' }).click();
  await page.getByLabel('Email').fill(email);
  await page.getByRole('button', { name: 'Send the link' }).click();
  await expect(page.getByRole('status')).toContainText('reset link is on its way');

  await page.goto(await mailLink(request, email));
  await expect(page.getByRole('heading', { name: 'Choose a new password' })).toBeVisible();
  await page.getByLabel('New password', { exact: true }).fill(NEW_PASSWORD);
  await page.getByRole('button', { name: 'Change password' }).click();
  await expect(page.getByRole('status')).toContainText('password was changed');

  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(CUSTOMER_PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await page.getByLabel('Password').fill(NEW_PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByTestId('whoami')).toContainText(email);
});

test('two-factor sign-in: set it up, then a code is needed', async ({ page }) => {
  const email = await registerCustomer(page);
  await page.getByRole('button', { name: 'Security' }).click();
  await page.getByRole('button', { name: 'Set up two-factor' }).click();
  const key = (await page.getByTestId('mfa-key').textContent())!.trim();
  await page.getByLabel('Code from the app').fill(totp(key));
  await page.getByRole('button', { name: 'Turn on' }).click();
  await expect(page.getByTestId('recovery-codes').locator('li')).toHaveCount(8);

  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(CUSTOMER_PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByLabel('Verification code')).toBeVisible();
  await expect(page.getByTestId('whoami')).toHaveCount(0);

  await page.getByLabel('Verification code').fill(totp(key));
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByTestId('whoami')).toContainText(email);
});

test('"Sign out everywhere" ends the session', async ({ page }) => {
  await registerCustomer(page);
  await page.getByRole('button', { name: 'Security' }).click();
  await page.getByRole('button', { name: 'Sign out everywhere' }).click();
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
});
