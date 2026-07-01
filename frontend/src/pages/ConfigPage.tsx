import { useEffect, useMemo, useState } from "react";
import { deleteJSON, getJSON, postJSON, putJSON } from "../api/client";

type AppConfig = {
  timezone: string;
  logLevel: string;
  scan: { intervalSeconds: number };
  rateLimits: { perMinute: number; perHour: number };
  labels: { allowlist: string[]; keywordMappings: Record<string, string[]> };
  llama: { baseUrl: string; apiKey: string; classifyPath: string };
};

type LabelsResponse = {
  configured: string[];
  imap: string[];
};

type IMAPConfigStatus = {
  configured: boolean;
  path?: string;
  keyPath?: string;
  host?: string;
  port?: number;
  username?: string;
  mailbox?: string;
  smtpHost?: string;
  smtpPort?: number;
  updatedAt?: string;
  encryptedAtRest?: boolean;
};

type IMAPForm = {
  host: string;
  port: number;
  username: string;
  password: string;
  mailbox: string;
  smtpHost: string;
  smtpPort: number;
};

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

function normalizeConfig(input: unknown): AppConfig {
  const source = (input ?? {}) as Record<string, any>;
  const labels = source.labels ?? {};
  const llama = source.llama ?? {};
  const scan = source.scan ?? {};
  const rateLimits = source.rateLimits ?? {};
  const rawMappings = labels.keywordMappings;

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
      keywordMappings: normalizeKeywordMappings(rawMappings)
    },
    llama: {
      baseUrl: llama.baseUrl ?? "",
      apiKey: llama.apiKey ?? "",
      classifyPath: llama.classifyPath ?? "/"
    }
  };
}

function uniqueLabels(labels: string[]): string[] {
  return Array.from(new Set(labels.map((label) => label.trim()).filter(Boolean)));
}

function labelsToText(labels: string[]): string {
  return labels.join("\n");
}

function textToLabels(raw: string): string[] {
  return uniqueLabels(raw.split(/\r?\n/));
}

function mappingToText(mapping: Record<string, string[]>): string {
  return Object.keys(mapping)
    .sort((a, b) => a.localeCompare(b))
    .map((label) => `${label}: ${uniqueLabels(mapping[label] ?? []).join(", ")}`)
    .join("\n");
}

function textToMapping(raw: string): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const line of raw.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed) {
      continue;
    }
    const splitAt = trimmed.indexOf(":");
    if (splitAt <= 0) {
      continue;
    }
    const label = trimmed.slice(0, splitAt).trim();
    const values = uniqueLabels(trimmed.slice(splitAt + 1).split(","));
    if (label && values.length > 0) {
      out[label] = values;
    }
  }
  return out;
}

export function ConfigPage() {
  const testPrompt = "Email Address: test@example.com Subject Line: Llama connectivity test Return only the label Updates";

  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [allowlistText, setAllowlistText] = useState("");
  const [keywordMappingText, setKeywordMappingText] = useState("");
  const [labelsFromImap, setLabelsFromImap] = useState<string[]>([]);
  const [configStatus, setConfigStatus] = useState("");

  const [imapStatus, setImapStatus] = useState<IMAPConfigStatus | null>(null);
  const [imapForm, setImapForm] = useState<IMAPForm>({
    host: "",
    port: 993,
    username: "",
    password: "",
    mailbox: "INBOX",
    smtpHost: "",
    smtpPort: 587
  });
  const [imapMessage, setImapMessage] = useState("");
  const [imapBusy, setImapBusy] = useState(false);

  const [llamaTestBusy, setLlamaTestBusy] = useState(false);
  const [llamaTestResult, setLlamaTestResult] = useState("");

  const effectiveAllowlist = useMemo(() => {
    const cfgLabels = textToLabels(allowlistText);
    return uniqueLabels([...cfgLabels]);
  }, [allowlistText]);

  async function refreshLabels() {
    const labelsData = await getJSON<LabelsResponse>("/api/labels");
    setLabelsFromImap(uniqueLabels(labelsData.imap ?? []));
  }

  async function refreshIMAPStatus() {
    const status = await getJSON<IMAPConfigStatus>("/api/imap/config");
    setImapStatus(status);
    if (status.configured) {
      setImapForm((prev) => ({
        host: status.host ?? prev.host,
        port: status.port ?? prev.port,
        username: status.username ?? prev.username,
        password: "",
        mailbox: status.mailbox ?? prev.mailbox,
        smtpHost: status.smtpHost ?? prev.smtpHost,
        smtpPort: status.smtpPort ?? prev.smtpPort
      }));
    }
  }

  useEffect(() => {
    let cancelled = false;

    const load = async () => {
      try {
        const nextConfig = await getJSON<unknown>("/api/config");
        if (cancelled) {
          return;
        }
        const normalized = normalizeConfig(nextConfig);
        setCfg(normalized);
        setAllowlistText(labelsToText(normalized.labels.allowlist));
        setKeywordMappingText(mappingToText(normalized.labels.keywordMappings));
      } catch {
        if (!cancelled) {
          setConfigStatus("Failed to load configuration data.");
        }
        return;
      }

      // Load secondary panels independently so one failure does not block the entire page.
      await Promise.all([
        refreshLabels().catch(() => undefined),
        refreshIMAPStatus().catch(() => undefined)
      ]);
    };

    load();
    return () => {
      cancelled = true;
    };
  }, []);

  if (!cfg) {
    return (
      <section className="panel">
        <h2>Configuration</h2>
        <p>{configStatus || "Loading configuration..."}</p>
      </section>
    );
  }

  async function saveConfig() {
    const next: AppConfig = {
      ...cfg,
      labels: {
        ...cfg.labels,
        allowlist: effectiveAllowlist,
        keywordMappings: textToMapping(keywordMappingText)
      }
    };

    try {
      await putJSON<{ ok: boolean }>("/api/config", next);
      setCfg(next);
      setConfigStatus("Configuration saved.");
    } catch {
      setConfigStatus("Failed to save configuration.");
    }
  }

  function applyImapLabelsToAllowlist() {
    const merged = uniqueLabels([...effectiveAllowlist, ...labelsFromImap]);
    setAllowlistText(labelsToText(merged));
    setConfigStatus("Merged discovered IMAP labels into allowlist (not yet saved).");
  }

  async function saveIMAPConfig() {
    setImapBusy(true);
    setImapMessage("");
    try {
      const result = await postJSON<IMAPConfigStatus>("/api/imap/config", imapForm);
      setImapStatus(result);
      setImapForm((prev) => ({ ...prev, password: "" }));
      setImapMessage("IMAP configuration saved.");
      await refreshLabels();
    } catch (error: unknown) {
      const message = error instanceof Error ? error.message : "unknown error";
      setImapMessage(`Failed to save IMAP config: ${message}`);
    } finally {
      setImapBusy(false);
    }
  }

  async function testIMAPConfig() {
    setImapBusy(true);
    setImapMessage("");
    try {
      const result = await postJSON<{ ok: boolean; error?: string; host?: string; port?: number; mailbox?: string }>(
        "/api/imap/test",
        imapForm
      );
      if (result.ok) {
        setImapMessage(`IMAP test passed (${result.host}:${result.port} ${result.mailbox}).`);
      } else {
        setImapMessage(`IMAP test failed: ${result.error ?? "unknown error"}`);
      }
    } catch (error: unknown) {
      const message = error instanceof Error ? error.message : "unknown error";
      setImapMessage(`IMAP test failed: ${message}`);
    } finally {
      setImapBusy(false);
    }
  }

  async function deleteIMAPConfig() {
    setImapBusy(true);
    setImapMessage("");
    try {
      await deleteJSON<{ ok: boolean; configured: boolean }>("/api/imap/config");
      setImapStatus({ configured: false });
      setImapForm({ host: "", port: 993, username: "", password: "", mailbox: "INBOX", smtpHost: "", smtpPort: 587 });
      setImapMessage("Stored IMAP configuration removed.");
    } catch (error: unknown) {
      const message = error instanceof Error ? error.message : "unknown error";
      setImapMessage(`Failed to delete IMAP config: ${message}`);
    } finally {
      setImapBusy(false);
    }
  }

  async function runLlamaTest() {
    setLlamaTestBusy(true);
    setLlamaTestResult("");
    try {
      const result = await postJSON<{ ok: boolean; response?: string; error?: string; baseUrl?: string; path?: string }>(
        "/api/llama/test",
        { prompt: testPrompt }
      );
      if (!result.ok) {
        setLlamaTestResult(`Llama test failed: ${result.error ?? "unknown error"}`);
      } else {
        setLlamaTestResult(
          `Llama test passed\nBase URL: ${result.baseUrl ?? ""}\nPath: ${result.path ?? ""}\nResponse: ${result.response ?? ""}`
        );
      }
    } catch (error: unknown) {
      const message = error instanceof Error ? error.message : "unknown error";
      setLlamaTestResult(`Llama test failed: ${message}`);
    } finally {
      setLlamaTestBusy(false);
    }
  }

  function updateConfig<K extends keyof AppConfig>(key: K, value: AppConfig[K]) {
    setCfg((prev) => (prev ? { ...prev, [key]: value } : prev));
  }

  return (
    <section className="panel">
      <h2>Configuration</h2>
      <p>Manage app settings, IMAP credentials, labels, and Llama connectivity.</p>

      <hr />
      <h3>Application</h3>
      <label>
        <div>Timezone</div>
        <input value={cfg.timezone} onChange={(event) => updateConfig("timezone", event.target.value)} />
      </label>
      <label>
        <div>Log Level</div>
        <input value={cfg.logLevel} onChange={(event) => updateConfig("logLevel", event.target.value)} />
      </label>
      <label>
        <div>Scan Interval (seconds)</div>
        <input
          type="number"
          value={cfg.scan.intervalSeconds}
          onChange={(event) => updateConfig("scan", { intervalSeconds: Number(event.target.value) || 0 })}
        />
      </label>
      <label>
        <div>Rate Limit Per Minute</div>
        <input
          type="number"
          value={cfg.rateLimits.perMinute}
          onChange={(event) => updateConfig("rateLimits", { ...cfg.rateLimits, perMinute: Number(event.target.value) || 0 })}
        />
      </label>
      <label>
        <div>Rate Limit Per Hour</div>
        <input
          type="number"
          value={cfg.rateLimits.perHour}
          onChange={(event) => updateConfig("rateLimits", { ...cfg.rateLimits, perHour: Number(event.target.value) || 0 })}
        />
      </label>

      <hr />
      <h3>IMAP</h3>
      <p>Saved mail config is encrypted at rest. SMTP settings fall back to the IMAP-derived host when left blank.</p>
      <label>
        <div>Host</div>
        <input value={imapForm.host} onChange={(event) => setImapForm((prev) => ({ ...prev, host: event.target.value }))} />
      </label>
      <label>
        <div>Port</div>
        <input
          type="number"
          value={imapForm.port}
          onChange={(event) => setImapForm((prev) => ({ ...prev, port: Number(event.target.value) || 993 }))}
        />
      </label>
      <label>
        <div>Username</div>
        <input value={imapForm.username} onChange={(event) => setImapForm((prev) => ({ ...prev, username: event.target.value }))} />
      </label>
      <label>
        <div>Password or App Password</div>
        <input
          type="password"
          value={imapForm.password}
          onChange={(event) => setImapForm((prev) => ({ ...prev, password: event.target.value }))}
          placeholder="Required when saving changes"
        />
      </label>
      <label>
        <div>Mailbox</div>
        <input value={imapForm.mailbox} onChange={(event) => setImapForm((prev) => ({ ...prev, mailbox: event.target.value }))} />
      </label>
      <label>
        <div>SMTP Host (optional)</div>
        <input value={imapForm.smtpHost} onChange={(event) => setImapForm((prev) => ({ ...prev, smtpHost: event.target.value }))} placeholder="Defaults to IMAP-derived host" />
      </label>
      <label>
        <div>SMTP Port (optional)</div>
        <input
          type="number"
          value={imapForm.smtpPort}
          onChange={(event) => setImapForm((prev) => ({ ...prev, smtpPort: Number(event.target.value) || 587 }))}
        />
      </label>
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <button type="button" onClick={saveIMAPConfig} disabled={imapBusy}>
          {imapBusy ? "Saving..." : "Save IMAP Config"}
        </button>
        <button type="button" onClick={testIMAPConfig} disabled={imapBusy}>
          {imapBusy ? "Testing..." : "Test IMAP"}
        </button>
        <button type="button" onClick={deleteIMAPConfig} disabled={imapBusy}>
          Delete Stored IMAP Config
        </button>
      </div>
      {imapStatus ? (
        <div style={{ border: "1px solid var(--line)", borderRadius: 6, padding: 10, marginTop: 10 }}>
          <p>Configured: {imapStatus.configured ? "Yes" : "No"}</p>
          {imapStatus.path ? <p>Config Path: {imapStatus.path}</p> : null}
          {imapStatus.keyPath ? <p>Key Path: {imapStatus.keyPath}</p> : null}
          {imapStatus.host ? <p>Host: {imapStatus.host}</p> : null}
          {imapStatus.port ? <p>Port: {imapStatus.port}</p> : null}
          {imapStatus.username ? <p>Username: {imapStatus.username}</p> : null}
          {imapStatus.mailbox ? <p>Mailbox: {imapStatus.mailbox}</p> : null}
          {imapStatus.smtpHost ? <p>SMTP Host: {imapStatus.smtpHost}</p> : null}
          {imapStatus.smtpPort ? <p>SMTP Port: {imapStatus.smtpPort}</p> : null}
          {imapStatus.updatedAt ? <p>Updated: {imapStatus.updatedAt}</p> : null}
        </div>
      ) : null}
      {imapMessage ? <p>{imapMessage}</p> : null}

      <hr />
      <h3>Label Allowlist</h3>
      <p>One label per line. These names must match mailbox keywords you want to apply.</p>
      <label>
        <div>Allowlist</div>
        <textarea rows={10} value={allowlistText} onChange={(event) => setAllowlistText(event.target.value)} style={{ width: "100%" }} />
      </label>
      <label>
        <div>Keyword Mappings (one per line: Label: Keyword1, Keyword2)</div>
        <textarea
          rows={8}
          value={keywordMappingText}
          onChange={(event) => setKeywordMappingText(event.target.value)}
          style={{ width: "100%" }}
        />
      </label>
      <button type="button" onClick={applyImapLabelsToAllowlist}>Merge IMAP Labels</button>
      <button type="button" onClick={saveConfig}>Save Configuration</button>
      {labelsFromImap.length > 0 ? <p>Discovered IMAP labels: {labelsFromImap.join(", ")}</p> : <p>No IMAP labels discovered yet.</p>}

      {configStatus ? <p>{configStatus}</p> : null}

      <hr />
      <h3>Remote LLM Model</h3>
      <label>
        <div>Base URL</div>
        <input value={cfg.llama.baseUrl} onChange={(event) => updateConfig("llama", { ...cfg.llama, baseUrl: event.target.value })} />
      </label>
      <label>
        <div>API Key</div>
        <input
          type="password"
          value={cfg.llama.apiKey}
          onChange={(event) => updateConfig("llama", { ...cfg.llama, apiKey: event.target.value })}
        />
      </label>
      <label>
        <div>Classify Path</div>
        <input
          value={cfg.llama.classifyPath}
          onChange={(event) => updateConfig("llama", { ...cfg.llama, classifyPath: event.target.value })}
        />
      </label>

      <button type="button" onClick={runLlamaTest} disabled={llamaTestBusy}>
        {llamaTestBusy ? "Testing..." : "Run Llama Test"}
      </button>
      {llamaTestResult ? <pre>{llamaTestResult}</pre> : null}
    </section>
  );
}
