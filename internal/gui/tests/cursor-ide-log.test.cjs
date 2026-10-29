// The opened Cursor row shows the cursor-ide diagnostic lines, refreshes
// them, and copies them. Chromium and WebKit, English and Chinese.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const words = {
  en: { details: "Details", diag: "Diagnostics", refresh: "Refresh", copy: "Copy all", empty: "No cursor-ide lines yet." },
  zh: { details: "详情", diag: "诊断", refresh: "刷新", copy: "全部复制", empty: "还没有 cursor-ide 的记录。" },
};

const agent = {
  id: "cursor-ide", name: "Cursor", path: "/test/cursor-ide", wired: true,
  fields: [{ key: "provider", label: "provider", value: "magpie", options: [{ value: "magpie", label: "magpie", ref: "magpie" }] }],
};

function serve(lang, log) {
  return async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return route.fulfill({ json: { agents: [agent], profiles: [], settings: { lang, theme: "light" } } });
    if (url.pathname === "/api/plugins") return route.fulfill({ json: { plugins: [] } });
    if (url.pathname === "/api/usage/quotas") return route.fulfill({ json: [] });
    if (url.pathname === "/api/groups") return route.fulfill({ json: { groups: [] } });
    if (url.pathname === "/api/providers") return route.fulfill({ json: { providers: [], gateway: { running: true, window: true } } });
    if (url.pathname === "/api/agents/cursor-ide/log") return route.fulfill({ json: log });
    if (url.pathname === "/api/copy") {
      log.copied = JSON.parse(route.request().postData() || "{}").text;
      return route.fulfill({ json: {} });
    }
    if (url.pathname.startsWith("/api/")) return route.fulfill({ json: {} });
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": Cursor diagnostics refresh and copy", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh"]) {
      await t.test(lang, async () => {
        const log = { path: "/home/me/.config/magpie/cursor-ide/cursor-ide.log", text: "2026-10-29 12:00:00 route ok model=aicodings-openai/gpt-6-astra path=AgentService/Run local" };
        const page = await (await browser.newContext({ viewport: { width: 1240, height: 900 }, reducedMotion: "reduce" })).newPage();
        page.setDefaultTimeout(8000);
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", serve(lang, log));
        await page.goto("http://magpie.test/");
        const w = words[lang];
        await page.locator('.row.agent[data-id="cursor-ide"]').waitFor();
        const row = page.locator('.row.agent[data-id="cursor-ide"]');
        await row.getByRole("button", { name: w.details }).click();
        const box = page.locator(".ag-diag");
        await box.waitFor();
        assert.match(await box.innerText(), /route ok model=aicodings-openai\/gpt-6-astra/);
        assert.match(await page.locator(".ag-exp").innerText(), new RegExp(w.diag));
        assert.match(await page.locator(".ag-path").innerText(), /cursor-ide\.log/);
        log.text = "2026-10-29 12:00:01 route miss model=grok-4.7 path=AgentService/Run forward; Cursor would say Unknown model ID";
        await page.locator(".ag-exp").getByRole("button", { name: w.refresh }).click();
        await page.waitForFunction((s) => document.querySelector(".ag-diag").textContent.includes(s), "Unknown model ID");
        await page.locator(".ag-exp").getByRole("button", { name: w.copy }).click();
        for (let i = 0; i < 20 && !log.copied; i++) await new Promise((r) => setTimeout(r, 50));
        assert.match(log.copied || "", /Unknown model ID/);
        if (lang === "zh" && engine === "chromium") {
          await page.screenshot({ path: "/opt/cursor/artifacts/cursor-ide-diagnostics.png" });
        }
        assert.deepEqual(errors, []);
        log.text = "";
        log.copied = "";
        await page.locator(".ag-exp").getByRole("button", { name: w.refresh }).click();
        await page.waitForFunction((s) => document.querySelector(".ag-diag").textContent.includes(s), w.empty);
      });
    }
  });
}
