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
    await expect(page.locator(".cards").first()).toBeVisible();
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
    await expect(page.locator(".cards").first()).toBeVisible();
  });

  await test.step("admin tabs all render", async () => {
    await page.click('a[href="#/admin/users"]');
    for (const tab of ["users", "keys", "roles", "aliases", "providers", "pricing", "audit"]) {
      await page.click(`.tabs button:has-text("${tab}")`);
      await expect(page.locator("#atab .panel").first()).toBeVisible();
    }
  });

  await test.step("provider kind is a dropdown including ollama and vertex", async () => {
    await page.click('.tabs button:has-text("providers")');
    await page.click("#new-prov");
    const kind = page.locator('select[name="kind"]');
    await expect(kind).toBeVisible();
    await expect(kind.locator('option:has-text("ollama")')).toHaveCount(1);
    await expect(kind.locator('option[value="vertex"]')).toHaveCount(1);
    await page.click("#mf-cancel");
  });

  await test.step("choosing vertex reveals project, location and its own credential field", async () => {
    await page.click("#new-prov");
    // The Vertex fields belong to that kind alone, so they stay out of the
    // way until it is chosen.
    await expect(page.locator('input[name="project"]')).toBeHidden();
    await page.selectOption('select[name="kind"]', "vertex");
    await expect(page.locator('input[name="project"]')).toBeVisible();
    await expect(page.locator('input[name="location"]')).toBeVisible();

    // A service-account credential is its own field: the API key is not
    // repurposed for it, and the label says what leaving it blank does.
    const cred = page.locator('textarea[name="credential_json"]');
    await expect(cred).toBeVisible();
    await expect(page.locator('label:has(textarea[name="credential_json"]) .lab'))
      .toContainText(/blank.*pod|pod.*blank/i);
    await expect(page.locator('input[name="api_key"]')).toBeHidden();
    await page.click("#mf-cancel");
  });

  await test.step("a credential-less vertex provider reads as federated, not as missing a key", async () => {
    await page.click("#new-prov");
    await page.fill('input[name="name"]', "playwright-vertex");
    await page.selectOption('select[name="kind"]', "vertex");
    await page.fill('input[name="project"]', "playwright-project");
    await page.fill('input[name="location"]', "us-central1");
    await page.click('#mf button[type="submit"]');

    const row = page.locator("tr", { hasText: "playwright-vertex" }).first();
    await expect(row).toContainText("federated");
    await expect(row).not.toContainText("none");

    // Project and location survive the save and come back into the form.
    await row.locator("button:has-text('Edit')").click();
    await expect(page.locator('input[name="project"]')).toHaveValue("playwright-project");
    await expect(page.locator('input[name="location"]')).toHaveValue("us-central1");
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
