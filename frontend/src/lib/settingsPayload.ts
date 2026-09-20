const RESPONSE_CACHE_CONFIG_GENERATION = "response_cache_config_generation";
const CODEX_IMAGES_DEFAULT_MAIN_MODEL = "codex_images_default_main_model";
// 后端按当前 Resin 配置现算的只读摘要,回写没有意义。
const CODEX_EGRESS = "codex_egress";

export function buildWritableSettingsPayload<
  T extends object,
>(settings: T): Omit<T, typeof RESPONSE_CACHE_CONFIG_GENERATION | typeof CODEX_IMAGES_DEFAULT_MAIN_MODEL | typeof CODEX_EGRESS> {
  const payload = { ...settings };
  Reflect.deleteProperty(payload, RESPONSE_CACHE_CONFIG_GENERATION);
  if (Reflect.has(payload, "retry_policy")) {
    for (const key of ["max_retries", "max_rate_limit_retries", "continuous_retry_enabled", "continuous_retry_max_duration_seconds"]) {
      Reflect.deleteProperty(payload, key);
    }
  }
  // 飞书配置由机器人监控页维护，避免普通设置页回写旧快照。
  for (const key of Object.keys(payload)) {
    if (key.startsWith("feishu_")) Reflect.deleteProperty(payload, key);
  }
  Reflect.deleteProperty(payload, CODEX_IMAGES_DEFAULT_MAIN_MODEL);
  Reflect.deleteProperty(payload, CODEX_EGRESS);
  return payload;
}
