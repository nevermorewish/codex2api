const RESPONSE_CACHE_CONFIG_GENERATION = "response_cache_config_generation";

export function buildWritableSettingsPayload<
  T extends object,
>(settings: T): Omit<T, typeof RESPONSE_CACHE_CONFIG_GENERATION> {
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
  return payload;
}
