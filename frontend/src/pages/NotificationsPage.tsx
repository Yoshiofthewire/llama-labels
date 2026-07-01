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
    mode: "all" | "folder" | "none";
    folder: string;
    publicKey: string;
    privateKeyPath: string;
  };
};

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
      folder: notifications.folder ?? "",
      publicKey: notifications.publicKey ?? "",
      privateKeyPath: notifications.privateKeyPath ?? ""
    }
  };
}

export function NotificationsPage() {
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [folderName, setFolderName] = useState("");
  const [status, setStatus] = useState("");

  useEffect(() => {
    let cancelled = false;

    async function load() {
      try {
        const nextConfig = await getJSON<unknown>("/api/config");
        if (cancelled) {
          return;
        }
        const normalized = normalizeConfig(nextConfig);
        setCfg(normalized);
        setFolderName(normalized.notifications.folder);
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
        folder: folderName.trim()
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

  if (!cfg) {
    return (
      <section className="panel">
        <h2>Notifications</h2>
        <p>{status || "Loading notification settings..."}</p>
      </section>
    );
  }

  return (
    <section className="panel">
      <h2>Notifications</h2>
      <p>Configure browser push notification preferences and the generated push key material.</p>

      <label>
        <div>Notification Mode</div>
        <select
          value={cfg.notifications.mode}
          onChange={(event) => setCfg((prev) => (prev ? { ...prev, notifications: { ...prev.notifications, mode: event.target.value as AppConfig["notifications"]["mode"] } } : prev))}
        >
          <option value="none">No email</option>
          <option value="all">All email</option>
          <option value="folder">Only emails in a folder</option>
        </select>
      </label>

      {cfg.notifications.mode === "folder" ? (
        <label>
          <div>Folder</div>
          <input value={folderName} onChange={(event) => setFolderName(event.target.value)} placeholder="INBOX/Alerts" />
        </label>
      ) : null}

      <label>
        <div>Push Public Key</div>
        <textarea readOnly value={cfg.notifications.publicKey} rows={4} />
      </label>

      <label>
        <div>Private Key File</div>
        <input readOnly value={cfg.notifications.privateKeyPath} />
      </label>

      <button type="button" onClick={() => void save()}>Save Notifications</button>
      {status ? <p>{status}</p> : null}
    </section>
  );
}