import assert from "node:assert/strict";
import { test } from "node:test";
import { chromium } from "playwright";
import { createServer } from "vite";

// Serve the actual page components on an insecure HTTP origin, not localhost.
// This reproduces the missing crypto.randomUUID capability without mocking it.
test("HTTP dashboard adds, saves and reloads custom audit models without ban controls", async () => {
  const server = await createServer({
    server: {
      host: "127.0.0.1",
      port: 0,
      hmr: false,
      allowedHosts: ["risk.test"],
    },
  });
  await server.listen();
  let browser;
  try {
    const port = server.httpServer.address().port;
    browser = await chromium.launch({
      headless: true,
      args: [
        "--host-resolver-rules=MAP risk.test 127.0.0.1",
        "--no-proxy-server",
      ],
    });
    const page = await browser.newPage();
    // Resolve fixture resources locally while retaining an untrusted HTTP
    // origin in Chromium (some CI machines proxy arbitrary hostnames).
    await page.route("http://risk.test:**/*", async (route) => {
      const response = await route.fetch({
        url: route.request().url().replace("risk.test", "127.0.0.1"),
      });
      await route.fulfill({ response });
    });
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (msg) => {
      if (msg.type() === "error") errors.push(msg.text());
    });
    page.on("requestfailed", (req) =>
      errors.push(req.url() + ": " + req.failure()?.errorText),
    );
    page.on("response", (response) => {
      if (response.status() >= 400)
        errors.push(response.url() + ": " + response.status());
    });
    await page.addInitScript(() => {
      localStorage.setItem("lang", "zh");
    });
    let saved = {
      enabled: true,
      audit_engine: "chat",
      mode: "pre_block",
      keyword_blocking_mode: "keyword_and_api",
      blocked_keywords: [],
      sample_rate: 100,
      fallback_on_block_enabled: true,
      pre_hash_check_enabled: false,
      record_non_hits: false,
      block_status: 403,
      block_message: "review failed",
      worker_count: 4,
      queue_size: 32768,
      hit_retention_days: 180,
      non_hit_retention_days: 3,
      model_audit: {
        nodes: [],
        system_prompt: "Return JSON",
        categorized: false,
        categories: [],
        block_threshold: 0.8,
        flag_threshold: 0.5,
        fail_open: true,
      },
    };
    let updates = 0;
    await page.route("**/api/admin/risk-control/config", async (route) => {
      if (route.request().method() === "PUT") {
        saved = route.request().postDataJSON();
        updates++;
      }
      await route.fulfill({
        json: {
          config: saved,
          audit_categories: [],
          audit_default_prompt: "",
          audit_category_prompt: "",
        },
      });
    });
    await page.route("**/api/admin/risk-control/status", (route) =>
      route.fulfill({
        json: {
          total: 0,
          hits: 0,
          blocked: 0,
          hashes: 0,
          queue_length: 0,
          active: 0,
          keys: [],
        },
      }),
    );
    const url = `http://risk.test:${port}/admin/tests/riskControl.fixture.html`;
    await page.goto(url);
    assert.deepEqual(
      await page.evaluate(() => [isSecureContext, typeof crypto.randomUUID]),
      [false, "undefined"],
    );
    await page
      .getByRole("button", { name: "新增节点", exact: true })
      .click({ timeout: 10000 })
      .catch(async (error) => {
        throw new Error(
          JSON.stringify({
            errors,
            body: await page.locator("body").innerText(),
          }),
          { cause: error },
        );
      });
    const editor = page.getByRole("region", { name: "编辑审计节点" });
    await editor
      .getByLabel("节点名称", { exact: true })
      .fill("HTTP 自定义模型");
    await editor
      .getByLabel("审核模型", { exact: true })
      .fill("vendor/custom-model-v42");
    await editor
      .getByLabel("Base URL", { exact: true })
      .fill("http://audit.example.test/v1");
    await editor
      .getByLabel("节点 API Key", { exact: true })
      .fill("test-credential");
    await editor
      .getByRole("button", { name: "应用到草稿", exact: true })
      .click();
    assert.equal(updates, 0);
    await page.getByRole("button", { name: "保存配置", exact: true }).click();
    await page.waitForFunction(() =>
      document.body.innerText.includes("风控配置已保存并生效"),
    );
    assert.equal(updates, 1);
    assert.equal(saved.model_audit.nodes[0].model, "vendor/custom-model-v42");
    assert.match(saved.model_audit.nodes[0].id, /^audit-[a-f0-9]{32}$/);
    assert.equal(saved.model_audit.nodes[0].api_key, "test-credential");
    assert.ok(!("auto_ban_enabled" in saved));
    await page.reload();
    await page.getByText("vendor/custom-model-v42", { exact: true }).waitFor();
    await page.getByRole("link", { name: "审核策略", exact: true }).click();
    await page.getByText("审核命中与兜底", { exact: true }).waitFor();
    const fallbackSwitch = page.getByRole("switch", {
      name: "审核未通过直接走兜底池",
      exact: true,
    });
    assert.equal(await fallbackSwitch.getAttribute("aria-checked"), "true");
    for (const enabled of [false, true]) {
      await fallbackSwitch.click();
      await Promise.all([
        page.waitForResponse(
          (response) =>
            response.url().endsWith("/risk-control/config") &&
            response.request().method() === "PUT",
        ),
        page.getByRole("button", { name: "保存配置", exact: true }).click(),
      ]);
      assert.equal(saved.fallback_on_block_enabled, enabled);
      await page.reload();
      await page.getByRole("link", { name: "审核策略", exact: true }).click();
      await fallbackSwitch.waitFor();
      assert.equal(
        await fallbackSwitch.getAttribute("aria-checked"),
        String(enabled),
      );
    }
    for (const title of ["邮件通知", "适用范围", "审计凭证", "分类阈值"]) {
      assert.equal(await page.getByText(title, { exact: true }).count(), 0);
    }
    await page
      .getByRole("link", { name: "自定义模型审计", exact: true })
      .click();
    assert.equal(await page.getByText("审核引擎", { exact: true }).count(), 0);
    await page.getByText("vendor/custom-model-v42", { exact: true }).waitFor();

    assert.equal(
      await page.getByText("自动封禁调用方 API Key", { exact: true }).count(),
      0,
    );
    assert.equal(
      await page
        .getByRole("link", { name: "封禁与 Hash", exact: true })
        .count(),
      0,
    );
    // Shared settings have one editor; navigating tabs preserves the draft.
    assert.equal(
      await page
        .getByRole("switch", { name: "审计失败时放行", exact: true })
        .count(),
      0,
    );
    await page.getByText("审计服务故障自动放行：", { exact: false }).waitFor();
    const policyLink = page.getByRole("region", { name: "统一审核配置" });
    assert.equal(await policyLink.getByRole("combobox").count(), 0);
    assert.equal(await policyLink.getByRole("switch").count(), 0);
    await page.getByRole("link", { name: "前往审核策略设置" }).click();
    await page.getByLabel("审核模式", { exact: true }).waitFor();
    assert.equal(await page.getByLabel("审核模式", { exact: true }).count(), 1);
    await page.getByLabel("审核模式", { exact: true }).click();
    await page.getByRole("option", { name: "异步观察", exact: true }).click();
    await page
      .getByRole("link", { name: "自定义模型审计", exact: true })
      .click();
    await policyLink.waitFor();
    assert.equal(await page.getByText("审核模式", { exact: true }).count(), 0);
    const prompt = page.getByRole("textbox", {
      name: "自定义审核提示词",
      exact: true,
    });
    const savePrompt = page.getByRole("button", {
      name: "保存审核提示词",
      exact: true,
    });
    await prompt.fill(
      "Return JSON only. Custom audit prompt persistence test.",
    );
    const previousUpdates = updates;
    await Promise.all([
      page.waitForResponse(
        (response) =>
          response.url().endsWith("/risk-control/config") &&
          response.request().method() === "PUT",
      ),
      savePrompt.click(),
    ]);
    await page.waitForFunction(() => {
      const button = [...document.querySelectorAll("button")].find((b) =>
        b.textContent.includes("保存审核提示词"),
      );
      return button?.disabled;
    });
    assert.equal(updates, previousUpdates + 1);
    assert.equal(saved.mode, "observe");
    assert.equal(
      saved.model_audit.system_prompt,
      "Return JSON only. Custom audit prompt persistence test.",
    );
    await page.reload();
    await policyLink.waitFor();
    assert.equal(await prompt.inputValue(), saved.model_audit.system_prompt);
    assert.equal(await savePrompt.isDisabled(), true);
    await policyLink.waitFor();
    assert.equal(await page.getByText("审核模式", { exact: true }).count(), 0);
    await page.getByRole("link", { name: "前往审核策略设置" }).click();
    assert.equal(
      await page.getByLabel("审核模式", { exact: true }).innerText(),
      "异步观察",
    );
    await page.getByRole("link", { name: "历史 Hash", exact: true }).click();
    assert.equal(
      await page.getByText("已封禁调用方 API Keys", { exact: true }).count(),
      0,
    );
    assert.deepEqual(errors, []);
  } finally {
    await browser?.close();
    await server.close();
  }
});
