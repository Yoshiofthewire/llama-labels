import { useEffect, useState } from "react";
import QRCode from "qrcode";
import { deleteJSON, getJSON, postJSON, putJSON, toErrorMessage } from "../api/client";
import { normalizeConfig, uniqueLabels, type AppConfig } from "../api/config";

type LabelsResponse = {
  configured: string[];
  imap: string[];
};

type NotificationVapidResponse = {
  publicKey: string;
};

type NotificationTestResponse = {
  ok: boolean;
  subscriptions: number;
  sent: number;
  failed: number;
  removedStale?: number;
  activeSubscriptions?: number;
};

type NovuStatusResponse = {
  applicationIdentifier: string;
  subscriberId: string;
  apiBase: string;
  serverBaseUrl?: string;
  relayEndpoint?: string;
  subscriberHash?: string;
  pairingToken?: string;
  pairingExpiresAt?: string;
  pairingTtlSeconds?: number;
  configured: boolean;
};

const QR_CODE_WIDTH_PX = 220;
const DEFAULT_PAIRING_TTL_SECONDS = 90;
const PAIRING_RED_ZONE_SECONDS = 15;

function collectNotificationKeywordOptions(cfg: AppConfig, labelsData: LabelsResponse): string[] {
  const configured = cfg.labels.allowlist ?? [];
  const mapped = Object.values(cfg.labels.keywordMappings ?? {}).flat();
  const imap = labelsData.imap ?? [];
  const selected = cfg.notifications.keywords ?? [];
  return uniqueLabels([...configured, ...mapped, ...imap, ...selected]);
}

function buildNovuPairingLink(novu: NovuStatusResponse): string {
  const params = new URLSearchParams();
  params.set("app", novu.applicationIdentifier);
  params.set("sub", novu.subscriberId);
  if (novu.subscriberHash) {
    params.set("hash", novu.subscriberHash);
  }
  if (novu.apiBase) {
    params.set("api", novu.apiBase);
  }
  if (novu.serverBaseUrl) {
    params.set("srv", novu.serverBaseUrl);
  }
  if (novu.relayEndpoint) {
    params.set("relay", novu.relayEndpoint);
  }
  if (novu.pairingToken) {
    params.set("pt", novu.pairingToken);
  }
  return `llamalabels://novu-pair?${params.toString()}`;
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value));
}

function pairingBarColor(remainingMs: number, ttlMs: number): string {
  const redZoneMs = PAIRING_RED_ZONE_SECONDS * 1000;
  if (remainingMs <= redZoneMs) {
    return "hsl(0 88% 46%)";
  }
  const activeMs = Math.max(ttlMs - redZoneMs, 1);
  const elapsedMs = clamp(activeMs - (remainingMs - redZoneMs), 0, activeMs);
  const ratio = elapsedMs / activeMs;
  const hue = Math.round(120 - ratio * 120);
  return `hsl(${hue} 88% 44%)`;
}

export function NotificationsPage() {
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [availableKeywords, setAvailableKeywords] = useState<string[]>([]);
  const [settingsTab, setSettingsTab] = useState<"delivery" | "keywords">("delivery");
  const [status, setStatus] = useState("");
  const [testBusy, setTestBusy] = useState(false);
  const [unsubscribeBusy, setUnsubscribeBusy] = useState(false);
  const [novuStatus, setNovuStatus] = useState<NovuStatusResponse | null>(null);
  const [novuQrDataUrl, setNovuQrDataUrl] = useState("");
  const [unpairBusy, setUnpairBusy] = useState(false);
  const [pairingExpiresAtMs, setPairingExpiresAtMs] = useState<number | null>(null);
  const [pairingTtlMs, setPairingTtlMs] = useState(DEFAULT_PAIRING_TTL_SECONDS * 1000);
  const [pairingClockMs, setPairingClockMs] = useState<number>(() => Date.now());
  const [pairingRefreshBusy, setPairingRefreshBusy] = useState(false);

  const statusTone = status.toLowerCase().includes("failed") ? "notice notice-error" : "notice notice-success";

  function applyNovuStatus(next: NovuStatusResponse | null) {
    setNovuStatus(next);
    if (!next) {
      setPairingExpiresAtMs(null);
      return;
    }

    const ttlSeconds = typeof next.pairingTtlSeconds === "number" && next.pairingTtlSeconds > 0
      ? next.pairingTtlSeconds
      : DEFAULT_PAIRING_TTL_SECONDS;
    setPairingTtlMs(ttlSeconds * 1000);

    if (next.pairingExpiresAt) {
      const expiresMs = Date.parse(next.pairingExpiresAt);
      setPairingExpiresAtMs(Number.isFinite(expiresMs) ? expiresMs : Date.now() + ttlSeconds * 1000);
    } else if (next.pairingToken) {
      setPairingExpiresAtMs(Date.now() + ttlSeconds * 1000);
    } else {
      setPairingExpiresAtMs(null);
    }
    setPairingClockMs(Date.now());
  }

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
        try {
          const status = await getJSON<NovuStatusResponse>("/api/notifications/novu");
          if (!cancelled) {
            applyNovuStatus(status);
          }
        } catch {
          if (!cancelled) {
            applyNovuStatus(null);
          }
        }
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

  useEffect(() => {
    if (!pairingExpiresAtMs) {
      return;
    }
    let cancelled = false;
    let refreshTriggered = false;

    const tick = () => {
      if (cancelled) {
        return;
      }
      const now = Date.now();
      setPairingClockMs(now);
      if (now >= pairingExpiresAtMs && !refreshTriggered) {
        refreshTriggered = true;
        setPairingRefreshBusy(true);
        void refreshNovuStatus().finally(() => {
          if (!cancelled) {
            setPairingRefreshBusy(false);
          }
        });
      }
    };

    tick();
    const timer = window.setInterval(tick, 250);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [pairingExpiresAtMs]);

  useEffect(() => {
    let cancelled = false;
    if (!novuStatus?.configured || !novuStatus.applicationIdentifier || !novuStatus.subscriberId) {
      setNovuQrDataUrl("");
      return;
    }
    QRCode.toDataURL(buildNovuPairingLink(novuStatus), { errorCorrectionLevel: "M", margin: 2, width: 220 })
      .then((dataUrl) => {
        if (!cancelled) {
          setNovuQrDataUrl(dataUrl);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setNovuQrDataUrl("");
        }
      });
    return () => {
      cancelled = true;
    };
  }, [novuStatus]);

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

  function base64URLToUint8Array(base64URL: string): Uint8Array {
    const normalized = base64URL.replace(/-/g, "+").replace(/_/g, "/");
    const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=");
    const raw = window.atob(padded);
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i += 1) {
      out[i] = raw.charCodeAt(i);
    }
    return out;
  }

  async function registerDeviceForPush(): Promise<void> {
    if (!("Notification" in window)) {
      throw new Error("Notifications are not supported by this browser.");
    }
    if (!("serviceWorker" in navigator) || !("PushManager" in window)) {
      throw new Error("Push notifications are not supported by this browser.");
    }

    let permission = Notification.permission;
    if (permission === "default") {
      permission = await Notification.requestPermission();
    }
    if (permission !== "granted") {
      throw new Error("Notification permission was not granted.");
    }

    const vapid = await getJSON<NotificationVapidResponse>("/api/notifications/vapid-public-key");
    const registration = await navigator.serviceWorker.register("/sw.js");
    const readyRegistration = await navigator.serviceWorker.ready;
    const target = readyRegistration ?? registration;

    let subscription = await target.pushManager.getSubscription();
    if (!subscription) {
      subscription = await target.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: base64URLToUint8Array(vapid.publicKey)
      });
    }

    await postJSON<{ ok: boolean; subscriptions: number }>("/api/notifications/subscriptions", subscription.toJSON());
  }

  async function sendTestNotification() {
    setTestBusy(true);
    try {
      await registerDeviceForPush();
      const result = await postJSON<NotificationTestResponse>("/api/notifications/test", {
        title: "Llama Mail Test Notification",
        body: "This test notification was sent to all of your subscribed devices."
      });
      setStatus(`Test sent: ${result.sent}/${result.subscriptions} device(s) delivered.`);
    } catch (error: unknown) {
      const detail = toErrorMessage(error, "unknown error");
      setStatus(`Failed to send test notification: ${detail}`);
    } finally {
      setTestBusy(false);
    }
  }

  async function unsubscribeThisDevice() {
    if (!("serviceWorker" in navigator) || !("PushManager" in window)) {
      setStatus("Failed to unsubscribe this device: push notifications are not supported by this browser.");
      return;
    }

    setUnsubscribeBusy(true);
    try {
      const readyRegistration = await navigator.serviceWorker.ready;
      const subscription = await readyRegistration.pushManager.getSubscription();
      if (!subscription) {
        setStatus("This device is not currently subscribed.");
        return;
      }

      await deleteJSON<{ ok: boolean; removed: boolean; subscriptions: number }>("/api/notifications/subscriptions", {
        endpoint: subscription.endpoint
      });
      await subscription.unsubscribe();
      setStatus("Unsubscribed this device from push notifications.");
    } catch (error: unknown) {
      const detail = toErrorMessage(error, "unknown error");
      setStatus(`Failed to unsubscribe this device: ${detail}`);
    } finally {
      setUnsubscribeBusy(false);
    }
  }

  async function refreshNovuStatus() {
    try {
      const next = await getJSON<NovuStatusResponse>("/api/notifications/novu");
      applyNovuStatus(next);
    } catch {
      applyNovuStatus(null);
    }
  }

  const pairingRemainingMs = pairingExpiresAtMs ? Math.max(0, pairingExpiresAtMs - pairingClockMs) : 0;
  const pairingBarWidth = Math.round(QR_CODE_WIDTH_PX * clamp(pairingRemainingMs / Math.max(pairingTtlMs, 1), 0, 1));
  const showPairingBar = pairingRemainingMs > 0 && novuStatus?.configured;
  const pairingBarBg = pairingBarColor(pairingRemainingMs, pairingTtlMs);

  async function revokePairedDevices() {
    setUnpairBusy(true);
    try {
      await postJSON<{ ok: boolean }>("/api/notifications/novu/unpair", {});
      setStatus("Revoked paired Android devices.");
    } catch (error: unknown) {
      const detail = toErrorMessage(error, "unknown error");
      setStatus(`Failed to revoke paired devices: ${detail}`);
    } finally {
      setUnpairBusy(false);
    }
  }

  function setMode(mode: AppConfig["notifications"]["mode"]) {
    setCfg((prev) => {
      if (!prev) {
        return prev;
      }

      const isMobile = /Android|iPhone|iPad|iPod|Mobile/i.test(navigator.userAgent);
      if (prev.notifications.mode === "none" && mode !== "none" && isMobile) {
        window.alert("To help insure notifications work, please remove your browser from sleep state.");
      }

      if (mode === "keywords") {
        setSettingsTab("keywords");
      }

      return { ...prev, notifications: { ...prev.notifications, mode } };
    });
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
          <div className="notifications-settings-tabs" role="tablist" aria-label="Notification settings tabs">
            <button
              type="button"
              role="tab"
              className={`notifications-settings-tab${settingsTab === "delivery" ? " active" : ""}`}
              aria-selected={settingsTab === "delivery"}
              onClick={() => setSettingsTab("delivery")}
            >
              Delivery Mode
            </button>
            <button
              type="button"
              role="tab"
              className={`notifications-settings-tab${settingsTab === "keywords" ? " active" : ""}`}
              aria-selected={settingsTab === "keywords"}
              onClick={() => setSettingsTab("keywords")}
            >
              IMAP Keywords
            </button>
          </div>

          {settingsTab === "delivery" ? (
            <div role="tabpanel" className="notifications-settings-panel">
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
            </div>
          ) : (
            <div role="tabpanel" className="notifications-settings-panel">
              <div className="notifications-keywords-head">
                <div>
                  <h3>IMAP Keywords</h3>
                  <p className="notifications-muted">Select which IMAP keywords can trigger notifications.</p>
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
            </div>
          )}
        </section>

        <section className="notifications-card notifications-android-card">
          <div className="notifications-android-head">
            <div>
              <h3>Mobile App Pairing</h3>
              <p className="notifications-muted">Scan this QR code from the Llama Labels Android app to receive push notifications for keyword-labeled email.</p>
            </div>
            <button type="button" className="notifications-ghost" onClick={() => void refreshNovuStatus()}>
              Refresh
            </button>
          </div>

          {!novuStatus?.configured ? (
            <p className="notifications-empty">Novu is not configured on the server yet. Set NOVU_SECRET_KEY, NOVU_WORKFLOW_ID and NOVU_APPLICATION_IDENTIFIER first.</p>
          ) : (
            <>
              {novuQrDataUrl ? (
                <div className="notifications-qr">
                  <img className="notifications-qr-image" src={novuQrDataUrl} alt="Android pairing QR code" width={220} height={220} />
                  {showPairingBar ? (
                    <div className="notifications-qr-timer-track" style={{ width: `${QR_CODE_WIDTH_PX}px` }} aria-hidden="true">
                      <div
                        className="notifications-qr-timer-bar"
                        style={{ width: `${pairingBarWidth}px`, background: pairingBarBg }}
                      />
                    </div>
                  ) : null}
                </div>
              ) : (
                <p className="notifications-empty">Preparing pairing code…</p>
              )}

              {pairingRefreshBusy ? <p className="notifications-qr-hint">Refreshing pairing code...</p> : null}

              <div className="notifications-android-meta">
                <span>Subscriber ID</span>
                <strong>{novuStatus.subscriberId || "Not available"}</strong>
              </div>

              <div className="notifications-android-tools">
                <button type="button" className="notifications-ghost" onClick={() => void revokePairedDevices()} disabled={unpairBusy}>
                  {unpairBusy ? "Revoking..." : "Revoke Paired Devices"}
                </button>
              </div>

              <div className="notifications-store-links">
                <span className="notifications-store-disabled" title="Store link coming soon">Google Play (coming soon)</span>
                <span className="notifications-store-disabled" title="Store link coming soon">App Store (coming soon)</span>
              </div>
            </>
          )}
        </section>

      </div>

      <div className="notifications-footer">
        <button type="button" className="notifications-ghost" onClick={() => void unsubscribeThisDevice()} disabled={unsubscribeBusy || testBusy}>
          {unsubscribeBusy ? "Unsubscribing..." : "Unsubscribe This Device"}
        </button>
        <button type="button" className="notifications-ghost" onClick={() => void sendTestNotification()} disabled={testBusy}>
          {testBusy ? "Sending Test..." : "Send Test Notification"}
        </button>
        <button type="button" className="notifications-save" onClick={() => void save()}>Save Notifications</button>
      </div>

      {status ? <p className={statusTone}>{status}</p> : null}
    </section>
  );
}