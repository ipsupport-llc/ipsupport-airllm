const { test, expect } = require("@playwright/test");

const ADMIN_PW = process.env.ADMIN_PASSWORD;

test("AirLLM console: full click-through", async ({ page }) => {
  test.skip(!ADMIN_PW, "set ADMIN_PASSWORD (from compose logs) to run");

  await test.step("login page renders with brand", async () => {
    await page.goto("/");
    await expect(page.locator(".login-card")).toContainText("AirLLM");
    await expect(page.locator("#login-form")).toBeVisible();
  });

  await test.step("sign in as admin", async () => {
    await page.fill('input[name="username"]', "admin");
    await page.fill('input[name="password"]', ADMIN_PW);
    await page.click('#login-form button[type="submit"]');
    await expect(page.locator(".brand")).toContainText("AirLLM");
    await expect(page.locator(".nav")).toContainText("API Keys");
  });

  await test.step("dashboard: usage cards + Connect endpoints", async () => {
    await expect(page.locator(".page-title")).toContainText("Dashboard");
    await expect(page.locator(".cards")).toBeVisible();
    await expect(page.getByText("Connect").first()).toBeVisible();
    await expect(page.getByText("/v1/chat/completions").first()).toBeVisible();
  });

  let token;
  await test.step("create an API key (token shown once)", async () => {
    await page.click('a[href="#/keys"]');
    await expect(page.locator(".page-title")).toContainText("API Keys");
    await page.fill("#key-name", "playwright key");
    await page.click("#key-create");
    const box = page.locator("#reveal .token-box").first();
    await expect(box).toBeVisible();
    token = (await box.textContent()).trim();
    expect(token).toMatch(/^air_/);
  });

  await test.step("the new key works on the data-plane", async () => {
    const res = await page.request.post("/v1/chat/completions", {
      headers: { Authorization: `Bearer ${token}` },
      data: { model: "mock-gpt", messages: [{ role: "user", content: "hi from playwright" }] },
    });
    expect(res.status()).toBe(200);
  });

  await test.step("usage page renders", async () => {
    await page.click('a[href="#/usage"]');
    await expect(page.locator(".cards")).toBeVisible();
  });

  await test.step("admin tabs all render", async () => {
    await page.click('a[href="#/admin/users"]');
    for (const tab of ["users", "keys", "roles", "aliases", "providers", "pricing", "audit"]) {
      await page.click(`.tabs button:has-text("${tab}")`);
      await expect(page.locator("#atab .panel").first()).toBeVisible();
    }
  });

  await test.step("provider kind is a dropdown including ollama", async () => {
    await page.click('.tabs button:has-text("providers")');
    await page.click("#new-prov");
    const kind = page.locator('select[name="kind"]');
    await expect(kind).toBeVisible();
    await expect(kind.locator('option:has-text("ollama")')).toHaveCount(1);
    await page.click("#mf-cancel");
  });

  await test.step("alias editor picks provider from a dropdown", async () => {
    await page.click('.tabs button:has-text("aliases")');
    await page.click("#new-alias");
    await expect(page.locator("#al-targets select.t-prov").first()).toBeVisible();
    await page.click("#al-cancel");
  });

  await page.screenshot({ path: "screenshots/console-final.png", fullPage: true });
});
