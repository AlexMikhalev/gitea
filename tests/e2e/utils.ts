import {env} from 'node:process';
import {expect} from '@playwright/test';
import type {APIRequestContext, Locator, Page} from '@playwright/test';

export function randomString(length: number): string {
  const chars = 'abcdefghijklmnopqrstuvwxyz0123456789';
  let result = '';
  for (let index = 0; index < length; index++) {
    result += chars.charAt(Math.floor(Math.random() * chars.length));
  }
  return result;
}

export function apiBaseUrl() {
  return env.GITEA_TEST_E2E_URL?.replace(/\/$/g, '');
}

export function apiHeaders() {
  return {Authorization: `Basic ${globalThis.btoa(`${env.GITEA_TEST_E2E_USER}:${env.GITEA_TEST_E2E_PASSWORD}`)}`};
}

async function apiRetry(fn: () => Promise<{ok: () => boolean; status: () => number; text: () => Promise<string>}>, label: string) {
  const maxAttempts = 5;
  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    const response = await fn();
    if (response.ok()) return;
    if ([500, 502, 503].includes(response.status()) && attempt < maxAttempts - 1) {
      const jitter = Math.random() * 500;
      await new Promise((resolve) => globalThis.setTimeout(resolve, 1000 * (attempt + 1) + jitter));
      continue;
    }
    throw new Error(`${label} failed: ${response.status()} ${await response.text()}`);
  }
}

export async function apiCreateRepo(requestContext: APIRequestContext, {name, autoInit = true}: {name: string; autoInit?: boolean}) {
  await apiRetry(() => requestContext.post(`${apiBaseUrl()}/api/v1/user/repos`, {
    headers: apiHeaders(),
    data: {name, auto_init: autoInit},
  }), 'apiCreateRepo');
}

export async function apiCreateIssue(
  requestContext: APIRequestContext,
  {owner, repo, title, body}: {owner: string; repo: string; title: string; body?: string},
): Promise<{index: number}> {
  let index = 0;
  await apiRetry(async () => {
    const response = await requestContext.post(`${apiBaseUrl()}/api/v1/repos/${owner}/${repo}/issues`, {
      headers: apiHeaders(),
      data: {title, body: body || ''},
    });
    if (response.ok()) ({index} = await response.json());
    return response;
  }, 'apiCreateIssue');
  return {index};
}

export async function apiDeleteRepo(requestContext: APIRequestContext, owner: string, name: string) {
  await apiRetry(() => requestContext.delete(`${apiBaseUrl()}/api/v1/repos/${owner}/${name}`, {
    headers: apiHeaders(),
  }), 'apiDeleteRepo');
}

export async function apiDeleteOrg(requestContext: APIRequestContext, name: string) {
  await apiRetry(() => requestContext.delete(`${apiBaseUrl()}/api/v1/orgs/${name}`, {
    headers: apiHeaders(),
  }), 'apiDeleteOrg');
}

export async function assertNoJsError(page: Page) {
  await expect(page.locator('.js-global-error')).toHaveCount(0);
}

export async function clickDropdownItem(page: Page, trigger: Locator, itemText: string) {
  await trigger.click();
  await page.getByText(itemText).click();
}

export async function login(page: Page, username = env.GITEA_TEST_E2E_USER, password = env.GITEA_TEST_E2E_PASSWORD) {
  await page.goto('/user/login');
  await page.getByLabel('Username or Email Address').fill(username);
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', {name: 'Sign In'}).click();
  await expect(page.getByRole('link', {name: 'Sign In'})).toBeHidden();
}

export async function logout(page: Page) {
  await page.context().clearCookies(); // workaround issues related to fomantic dropdown
  await page.goto('/');
  await expect(page.getByRole('link', {name: 'Sign In'})).toBeVisible();
}
