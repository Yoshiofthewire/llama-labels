// Global/system config document (admin-editable). Per-user notification
// delivery preferences moved to /api/notifications/preferences.
export type AppConfig = {
  timezone: string;
  logLevel: string;
  scan: { intervalSeconds: number };
  rateLimits: { perMinute: number; perHour: number };
  labels: { allowlist: string[]; keywordMappings: Record<string, string[]> };
  llama: { baseUrl: string; apiKey: string; classifyPath: string };
};

export function uniqueLabels(labels: string[]): string[] {
  return Array.from(new Set(labels.map((label) => label.trim()).filter(Boolean)));
}

function normalizeKeywordMappings(input: unknown): Record<string, string[]> {
  if (!input || typeof input !== "object") return {};
  const source = input as Record<string, unknown>;
  const out: Record<string, string[]> = {};

  for (const [label, rawValues] of Object.entries(source)) {
    const cleanLabel = String(label).trim();
    if (!cleanLabel) continue;

    const values = Array.isArray(rawValues)
      ? uniqueLabels(rawValues.map(String))
      : typeof rawValues === "string"
        ? uniqueLabels(rawValues.split(","))
        : [];

    if (values.length > 0) out[cleanLabel] = values;
  }
  return out;
}

export function normalizeConfig(input: unknown): AppConfig {
  const source = (input ?? {}) as Record<string, any>;
  const labels = source.labels ?? {};
  const llama = source.llama ?? {};
  const scan = source.scan ?? {};
  const rateLimits = source.rateLimits ?? {};

  return {
    timezone: source.timezone ?? "UTC",
    logLevel: source.logLevel ?? "info",
    scan: { intervalSeconds: scan.intervalSeconds ?? 90 },
    rateLimits: {
      perMinute: rateLimits.perMinute ?? 10,
      perHour: rateLimits.perHour ?? 20
    },
    labels: {
      allowlist: labels.allowlist ?? [],
      keywordMappings: normalizeKeywordMappings(labels.keywordMappings)
    },
    llama: {
      baseUrl: llama.baseUrl ?? "",
      apiKey: llama.apiKey ?? "",
      classifyPath: llama.classifyPath ?? ""
    }
  };
}
