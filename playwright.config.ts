import { defineConfig, devices } from '@playwright/test';

// The stack under test is the one in the kind cluster: make cluster-up
export default defineConfig({
  testDir: './e2e/specs',
  timeout: 60_000,
  fullyParallel: false,
  use: {
    baseURL: process.env.WEB_URL ?? 'http://127.0.0.1:8108',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
