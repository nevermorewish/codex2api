import assert from "node:assert/strict";
import test from "node:test";

import { buildWritableSettingsPayload } from "./settingsPayload.ts";

test("general settings save does not overwrite bot monitor configuration", () => {
  const settings = {
    site_name: "Updated site",
    feishu_alert_enabled: false,
    feishu_app_id: "old-app-id",
    feishu_app_secret: "old-secret",
    feishu_app_secret_configured: false,
    feishu_chat_ids: "old-chat",
    feishu_alert_error_codes: "503",
    feishu_first_token_timeout_seconds: 30,
  };
  assert.deepEqual(buildWritableSettingsPayload(settings), { site_name: "Updated site" });
  assert.equal(settings.feishu_app_id, "old-app-id");
});

test("writable settings payload omits response cache generation regardless of value", () => {
  for (const generation of [7, 0, null, undefined]) {
    const settings = {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      response_cache_config_generation: generation,
      future_setting: "preserved",
    };

    const payload = buildWritableSettingsPayload(settings);

    assert.equal(
      Object.hasOwn(payload, "response_cache_config_generation"),
      false,
    );
    assert.deepEqual(payload, {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      future_setting: "preserved",
    });
    assert.equal(
      Object.hasOwn(settings, "response_cache_config_generation"),
      true,
      "the source settings object must not be mutated",
    );
    assert.equal(settings.response_cache_config_generation, generation);
  }
});
