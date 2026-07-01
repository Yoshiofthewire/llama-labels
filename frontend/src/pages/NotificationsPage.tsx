import { useEffect, useState } from "react";
import { getJSON, putJSON } from "../api/client";

type AppConfig = {
  timezone: string;
  logLevel: string;
  scan: { intervalSeconds: number };
  rateLimits: { perMinute: number; perHour: number };
  labels: { allowlist: string[]; keywordMappings: Record<string, string[]> };
  llama: { baseUrl: string; apiKey: string; classifyPath: string };
  notifications: {
    mode: "all" | "keywords" | "none";
    keywords: string[];
  };
};

type LabelsResponse = {
  configured: string[];
  imap: string[];
};

function uniqueLabels(labels: string[]): string[] {
  return Array.from(new Set(labels.map((label) => label.trim()).filter(Boolean)));
}

function collectNotificationKeywordOptions(cfg: AppConfig, labelsData: LabelsResponse): string[] {
  const configured = cfg.labels.allowlist ?? [];
  const mapped = Object.values(cfg.labels.keywordMappings ?? {}).flat();
  const imap = labelsData.imap ?? [];
  const selected = cfg.notifications.keywords ?? [];
  return uniqueLabels([...configured, ...mapped, ...imap, ...selected]);
}

function normalizeConfig(input: unknown): AppConfig {
  const source = (input ?? {}) as Record<string, any>;
  const notifications = source.notifications ?? {};

  return {
    timezone: source.timezone ?? "UTC",
    logLevel: source.logLevel ?? "info",
    scan: { intervalSeconds: source.scan?.intervalSeconds ?? 90 },
    rateLimits: {
      perMinute: source.rateLimits?.perMinute ?? 10,
      perHour: source.rateLimits?.perHour ?? 20
    },
    labels: {
      allowlist: source.labels?.allowlist ?? [],
      keywordMappings: source.labels?.keywordMappings ?? {}
    },
    llama: {
      baseUrl: source.llama?.baseUrl ?? "",
      apiKey: source.llama?.apiKey ?? "",
      classifyPath: source.llama?.classifyPath ?? "/"
    },
    notifications: {
      mode: notifications.mode ?? "none",
      keywords: Array.isArray(notifications.keywords) ? notifications.keywords.map(String) : []
    }
  };
}

export function NotificationsPage() {
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [availableKeywords, setAvailableKeywords] = useState<string[]>([]);
  const [status, setStatus] = useState("");

  const statusTone = status.toLowerCase().includes("failed") ? "notice notice-error" : "notice notice-success";

  useEffect(() => {
    let cancelled = false;

    async function load() {
      try {
        const [nextConfig, labelsData] = await Promise.all([
          getJSON<unknown>("/api/config"),
          getJSON<LabelsResponse>("/api/labels")
        ]);
        if (cancelled) {
          return;
        }
        const normalized = normalizeConfig(nextConfig);
        setCfg(normalized);
        setAvailableKeywords(collectNotificationKeywordOptions(normalized, labelsData));
      } catch {
        if (!cancelled) {
          setStatus("Failed to load notification settings.");
        }
      }
    }

    load();
    return () => {
      cancelled = true;
    };
  }, []);

  async function save() {
    if (!cfg) {
      return;
    }

    const next: AppConfig = {
      ...cfg,
      notifications: {
        ...cfg.notifications,
        keywords: uniqueLabels(cfg.notifications.keywords)
      }
    };

    try {
      await putJSON<{ ok: boolean }>("/api/config", next);
      setCfg(next);
      setStatus("Notification settings saved.");
    } catch {
      setStatus("Failed to save notification settings.");
    }
  }

  function setMode(mode: AppConfig["notifications"]["mode"]) {
    setCfg((prev) => (prev ? { ...prev, notifications: { ...prev.notifications, mode } } : prev));
  }

  function setAllKeywords() {
    setCfg((prev) => (prev ? { ...prev, notifications: { ...prev.notifications, keywords: uniqueLabels(availableKeywords) } } : prev));
  }

  function clearKeywords() {
    setCfg((prev) => (prev ? { ...prev, notifications: { ...prev.notifications, keywords: [] } } : prev));
  }

  function toggleKeyword(keyword: string, checked: boolean) {
    setCfg((prev) => {
      if (!prev) return prev;
      const nextKeywords = checked
        ? uniqueLabels([...prev.notifications.keywords, keyword])
        : prev.notifications.keywords.filter((item) => item !== keyword);
      return { ...prev, notifications: { ...prev.notifications, keywords: nextKeywords } };
    });
  }

  if (!cfg) {
    return (
      <section className="panel">
        <h2>Notifications</h2>
        <p>{status || "Loading notification settings..."}</p>
      </section>
    );
  }

  return (
    <section className="panel notifications-page">
      <div className="notifications-hero">
        <h2>Notifications</h2>
        <p>Choose how alerts are delivered and preselect IMAP keywords any time.</p>
      </div>

      <div className="notifications-layout">
        <section className="notifications-card">
          <h3>Delivery Mode</h3>
          <p className="notifications-muted">Switch between disabled alerts, all-email alerts, or keyword-only alerts.</p>

          <div className="notifications-mode-grid">
            <label className={`notifications-mode-option${cfg.notifications.mode === "none" ? " active" : ""}`}>
              <input
                className="notifications-mode-input"
                type="radio"
                checked={cfg.notifications.mode === "none"}
                onChange={() => setMode("none")}
              />
              <span className="notifications-mode-title">No email</span>
              <span className="notifications-mode-copy">Pause browser notifications.</span>
            </label>

            <label className={`notifications-mode-option${cfg.notifications.mode === "all" ? " active" : ""}`}>
              <input
                className="notifications-mode-input"
                type="radio"
                checked={cfg.notifications.mode === "all"}
                onChange={() => setMode("all")}
              />
              <span className="notifications-mode-title">All emails</span>
              <span className="notifications-mode-copy">Notify for every new message.</span>
            </label>

            <label className={`notifications-mode-option${cfg.notifications.mode === "keywords" ? " active" : ""}`}>
              <input
                className="notifications-mode-input"
                type="radio"
                checked={cfg.notifications.mode === "keywords"}
                onChange={() => setMode("keywords")}
              />
              <span className="notifications-mode-title">IMAP keywords</span>
              <span className="notifications-mode-copy">Notify only for selected keywords.</span>
            </label>
          </div>
        </section>

        <section className="notifications-card notifications-keywords-card">
          <div className="notifications-keywords-head">
            <div>
              <h3>IMAP Keywords</h3>
              <p className="notifications-muted">This list is always visible so you can prepare selections before enabling keyword mode.</p>
            </div>
            <span className="notifications-count">{cfg.notifications.keywords.length} selected</span>
          </div>

          <div className="notifications-keywords-tools">
            <button type="button" className="notifications-secondary" onClick={setAllKeywords} disabled={availableKeywords.length === 0}>
              Select All
            </button>
            <button type="button" className="notifications-ghost" onClick={clearKeywords} disabled={cfg.notifications.keywords.length === 0}>
              Clear
            </button>
          </div>

          {availableKeywords.length === 0 ? (
            <p className="notifications-empty">No IMAP keywords found yet. Configure labels in Configuration or sync labels from IMAP first.</p>
          ) : (
            <div className="notifications-keywords-grid">
              {availableKeywords.map((keyword) => (
                <label key={keyword} className={`notifications-keyword-option${cfg.notifications.keywords.includes(keyword) ? " selected" : ""}`}>
                  <input
                    type="checkbox"
                    checked={cfg.notifications.keywords.includes(keyword)}
                    onChange={(event) => toggleKeyword(keyword, event.target.checked)}
                  />
                  <span>{keyword}</span>
                </label>
              ))}
            </div>
          )}

          {cfg.notifications.mode !== "keywords" ? (
            <p className="notifications-hint">Selections are saved now and will be used when Delivery Mode is set to IMAP keywords.</p>
          ) : null}
        </section>
      </div>

      <div className="notifications-footer">
        <button type="button" className="notifications-save" onClick={() => void save()}>Save Notifications</button>
      </div>

      {status ? <p className={statusTone}>{status}</p> : null}
    </section>
  );
}