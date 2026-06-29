// Standalone screenshot tour of the AirLLM console for design review.
// Usage: ADMIN_PASSWORD=... BASE_URL=http://127.0.0.1:8080 node shoot.js
const { chromium } = require("@playwright/test");

const BASE = process.env.BASE_URL || "http://127.0.0.1:8080";
const PW = process.env.ADMIN_PASSWORD;
const OUT = __dirname + "/screenshots";

async function shoot(page, name) {
  await page.waitForTimeout(350);
  await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: true });
  console.log("shot:", name);
}

async function login(page) {
  await page.goto(BASE + "/");
  await page.fill('input[name="username"]', "admin");
  await page.fill('input[name="password"]', PW);
  await page.click('#login-form button[type="submit"]');
  await page.waitForSelector(".nav");
}

async function tour(page, prefix) {
  // login page (fresh context, logged out)
  await page.goto(BASE + "/");
  await page.waitForSelector(".login-card");
  await shoot(page, `${prefix}-01-login`);

  await login(page);
  await shoot(page, `${prefix}-02-dashboard`);

  await page.click('a[href="#/keys"]');
  await page.waitForSelector(".page-title");
  await shoot(page, `${prefix}-03-keys`);

  await page.click('a[href="#/usage"]');
  await page.waitForTimeout(300);
  await shoot(page, `${prefix}-04-usage`);

  await page.click('a[href="#/captures"]');
  await page.waitForTimeout(300);
  await shoot(page, `${prefix}-05-captures`);

  await page.click('a[href="#/review"]');
  await page.waitForTimeout(300);
  await shoot(page, `${prefix}-06-review`);

  // admin tabs
  await page.goto(BASE + "/#/admin/users");
  await page.waitForSelector(".tabs");
  const tabs = ["users", "keys", "roles", "aliases", "providers", "pricing", "dlp", "audit"];
  for (const t of tabs) {
    const btn = page.locator(`.tabs button:has-text("${t}")`).first();
    if (await btn.count()) {
      await btn.click();
      await page.waitForTimeout(300);
      await shoot(page, `${prefix}-07-admin-${t}`);
    } else {
      console.log("no tab:", t);
    }
  }

  // provider create modal (design of forms/modals)
  await page.locator('.tabs button:has-text("providers")').first().click();
  await page.waitForTimeout(200);
  const np = page.locator("#new-prov");
  if (await np.count()) {
    await np.click();
    await page.waitForTimeout(300);
    await shoot(page, `${prefix}-08-provider-modal`);
    const cancel = page.locator("#mf-cancel");
    if (await cancel.count()) await cancel.click();
  }
}

(async () => {
  if (!PW) { console.error("set ADMIN_PASSWORD"); process.exit(1); }
  const browser = await chromium.launch({ channel: "chrome", headless: true });

  const desktop = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const dp = await desktop.newPage();
  await tour(dp, "desktop");
  await desktop.close();

  const mobile = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true });
  const mp = await mobile.newPage();
  await tour(mp, "mobile");
  await mobile.close();

  await browser.close();
  console.log("done");
})();
