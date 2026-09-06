const RESPONSE_CACHE_CONFIG_GENERATION = "response_cache_config_generation";

export function buildWritableSettingsPayload<
  T extends object,
>(settings: T): Omit<T, typeof RESPONSE_CACHE_CONFIG_GENERATION> {
  const payload = { ...settings };
  Reflect.deleteProperty(payload, RESPONSE_CACHE_CONFIG_GENERATION);
  // 飞书配置由机器人监控页维护，避免普通设置页回写旧快照。
  for (const key of Object.keys(payload)) {
    if (key.startsWith("feishu_")) Reflect.deleteProperty(payload, key);
  }
  return payload;
}
