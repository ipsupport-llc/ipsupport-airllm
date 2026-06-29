const { defineConfig } = require("@playwright/test");

// Uses the system Google Chrome (channel: "chrome") so no browser download is
// needed. Point BASE_URL at a running gateway; pass ADMIN_PASSWORD (from
// `docker compose logs app | grep "mock login"`).
module.exports = defineConfig({
  testDir: "./tests",
  timeout: 30000,
  reporter: [["list"]],
  use: {
    baseURL: process.env.BASE_URL || "http://127.0.0.1:8080",
    channel: "chrome",
    headless: true,
    viewport: { width: 1280, height: 900 },
    screenshot: "only-on-failure",
  },
});
