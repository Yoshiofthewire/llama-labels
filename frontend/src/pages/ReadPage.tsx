import { useEffect, useMemo, useState } from "react";
import { getJSON, postJSON } from "../api/client";

type InboxEmail = {
  messageId: string;
  sender: string;
  sentTo?: string;
  subject: string;
  body?: string;
  label?: string;
  status: string;
  detail?: string;
  atUtc: string;
};

type InboxResponse = {
  tabs: string[];
  byTab: Record<string, InboxEmail[]>;
};

type InboxAction = "delete" | "archive" | "spam" | "read";

type InboxActionResponse = {
  ok: boolean;
  action: InboxAction;
  processed: number;
  failed: Array<{ messageId: string; error: string }>;
};

function formatTimestamp(value: string): string {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

function processEmailHtml(html: string, showImages: boolean): string {
  // Extract body content if it's a full HTML document
  const bodyMatch = html.match(/<body[^>]*>([\s\S]*)<\/body>/i);
  const content = bodyMatch ? bodyMatch[1] : html;
  
  // Replace img tags with [Image Blocked] if not showing images
  if (showImages) return content;
  return content.replace(/<img[^>]*>/gi, "[Image Blocked]");
}

export function ReadPage() {
  const [tabs, setTabs] = useState<string[]>([]);
  const [byTab, setByTab] = useState<Record<string, InboxEmail[]>>({});
  const [activeTab, setActiveTab] = useState<string>("");
  const [selected, setSelected] = useState<InboxEmail | null>(null);
  const [selectedMessageIds, setSelectedMessageIds] = useState<string[]>([]);
  const [showImages, setShowImages] = useState(false);
  const [showRawEmail, setShowRawEmail] = useState(false);
  const [loading, setLoading] = useState(false);
  const [actionLoading, setActionLoading] = useState(false);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");

  async function loadInbox() {
    setLoading(true);
    setError("");
    try {
      const data = await getJSON<InboxResponse>("/api/inbox?limit=500");
      const nextTabs = data.tabs ?? [];
      const nextByTab = data.byTab ?? {};
      setTabs(nextTabs);
      setByTab(nextByTab);
      setActiveTab((current) => {
        if (current && nextTabs.includes(current)) return current;
        return nextTabs[0] ?? "";
      });
      setSelectedMessageIds((current) => {
        if (current.length === 0) return current;
        const nextIDSet = new Set<string>();
        Object.values(nextByTab).forEach((items) => {
          items.forEach((item) => nextIDSet.add(item.messageId));
        });
        return current.filter((id) => nextIDSet.has(id));
      });
    } catch (e) {
      const message = e instanceof Error ? e.message : "failed to load inbox";
      setError(message);
      setTabs([]);
      setByTab({});
      setActiveTab("");
      setSelectedMessageIds([]);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    loadInbox();
    const timer = setInterval(loadInbox, 15_000);
    return () => clearInterval(timer);
  }, []);

  const rows = useMemo(() => {
    if (!activeTab) return [];
    return byTab[activeTab] ?? [];
  }, [activeTab, byTab]);

  const selectedInTab = useMemo(
    () => rows.filter((row) => selectedMessageIds.includes(row.messageId)),
    [rows, selectedMessageIds]
  );

  const allRowsSelected = rows.length > 0 && selectedInTab.length === rows.length;

  function removeMessageIDs(messageIds: string[]) {
    if (messageIds.length === 0) return;
    const removeSet = new Set(messageIds);
    setByTab((current) => {
      const next: Record<string, InboxEmail[]> = {};
      Object.entries(current).forEach(([tab, items]) => {
        next[tab] = items.filter((item) => !removeSet.has(item.messageId));
      });
      return next;
    });
    setSelectedMessageIds((current) => current.filter((id) => !removeSet.has(id)));
  }

  async function applyInboxAction(action: InboxAction, messageIds: string[], options?: { closeModal?: boolean }) {
    if (messageIds.length === 0 || actionLoading) return;
    setActionLoading(true);
    setActionError("");
    try {
      const response = await postJSON<InboxActionResponse>("/api/inbox/actions", {
        action,
        messageIds
      });
      if (response.failed.length > 0) {
        const first = response.failed[0];
        throw new Error(first?.error || "some messages could not be updated");
      }
      removeMessageIDs(messageIds);
      if (options?.closeModal) {
        setSelected(null);
      }
    } catch (e) {
      const message = e instanceof Error ? e.message : "failed to apply inbox action";
      setActionError(message);
    } finally {
      setActionLoading(false);
    }
  }

  async function openEmailDetails(item: InboxEmail) {
    setSelected(item);
    setShowImages(false);
    setShowRawEmail(false);
    setActionError("");
    await applyInboxAction("read", [item.messageId]);
  }

  function printEmails(items: InboxEmail[]) {
    if (items.length === 0 || typeof window === "undefined") return;
    const escapeHtml = (value: string) =>
      value
        .replaceAll("&", "&amp;")
        .replaceAll("<", "&lt;")
        .replaceAll(">", "&gt;")
        .replaceAll('"', "&quot;")
        .replaceAll("'", "&#39;");

    const sections = items
      .map((item) => {
        const body = item.body || "No message body available.";
        const isHtml = /<[^>]+>/.test(body);
        return `
          <article style="page-break-inside: avoid; border: 1px solid #bbb; border-radius: 8px; padding: 12px; margin-bottom: 14px;">
            <h2 style="margin: 0 0 8px; font-size: 18px;">${escapeHtml(item.subject || "(no subject)")}</h2>
            <p style="margin: 0 0 6px;"><strong>Sender:</strong> ${escapeHtml(item.sender || "-")}</p>
            <p style="margin: 0 0 10px;"><strong>Time:</strong> ${escapeHtml(formatTimestamp(item.atUtc))}</p>
            <div>${isHtml ? body : `<pre style="white-space: pre-wrap; margin: 0;">${escapeHtml(body)}</pre>`}</div>
          </article>
        `;
      })
      .join("\n");

    const printWindow = window.open("", "_blank", "noopener,noreferrer,width=900,height=700");
    if (!printWindow) {
      setActionError("Popup blocked by browser; allow popups to print selected emails.");
      return;
    }
    printWindow.document.open();
    printWindow.document.write(`
      <!doctype html>
      <html>
        <head>
          <meta charset="utf-8" />
          <title>Inbox Print</title>
          <style>
            body { font-family: Arial, sans-serif; color: #111; margin: 24px; }
          </style>
        </head>
        <body>
          ${sections}
        </body>
      </html>
    `);
    printWindow.document.close();
    printWindow.focus();
    printWindow.print();
  }

  return (
    <section className="panel">
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <div>
          <h2 style={{ marginTop: 0, marginBottom: 6 }}>Inbox</h2>
        </div>
        <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
          <button
            type="button"
            onClick={() => applyInboxAction("delete", selectedMessageIds)}
            disabled={selectedMessageIds.length === 0 || actionLoading}
          >
            Delete
          </button>
          <button
            type="button"
            onClick={() => applyInboxAction("archive", selectedMessageIds)}
            disabled={selectedMessageIds.length === 0 || actionLoading}
          >
            Archive
          </button>
          <button
            type="button"
            onClick={() => applyInboxAction("spam", selectedMessageIds)}
            disabled={selectedMessageIds.length === 0 || actionLoading}
          >
            Mark as Spam
          </button>
          <button
            type="button"
            onClick={() => applyInboxAction("read", selectedMessageIds)}
            disabled={selectedMessageIds.length === 0 || actionLoading}
          >
            Mark as Read
          </button>
          <button
            type="button"
            onClick={() => printEmails(selectedInTab)}
            disabled={selectedInTab.length === 0 || actionLoading}
          >
            Print
          </button>
          <button type="button" onClick={loadInbox} disabled={loading || actionLoading}>
            {loading ? "Loading..." : "Refresh"}
          </button>
        </div>
      </div>

      {error ? <p className="notice notice-error">Failed to load inbox: {error}</p> : null}
      {actionError ? <p className="notice notice-error">Inbox action failed: {actionError}</p> : null}

      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginTop: 14, marginBottom: 14 }}>
        {tabs.map((tab) => {
          const unreadCount = (byTab[tab] ?? []).length;
          const isActive = activeTab === tab;
          return (
            <button
              key={tab}
              type="button"
              onClick={() => setActiveTab(tab)}
              style={{
                background: isActive ? "var(--accent)" : "transparent",
                color: isActive ? "#2f3a00" : "var(--ink-strong)",
                border: "1px solid var(--line)",
                borderRadius: 999,
                padding: "0.38rem 0.78rem",
                fontSize: "0.82rem",
                display: "inline-flex",
                alignItems: "center",
                gap: 8
              }}
            >
              <span>{tab}</span>
              <span
                style={{
                  minWidth: 18,
                  height: 18,
                  borderRadius: 999,
                  border: "1px solid var(--line)",
                  background: isActive ? "rgba(255, 255, 255, 0.38)" : "var(--accent-soft)",
                  color: "var(--ink-strong)",
                  display: "inline-flex",
                  alignItems: "center",
                  justifyContent: "center",
                  padding: "0 6px",
                  fontSize: "0.72rem",
                  fontWeight: 700,
                  lineHeight: 1
                }}
              >
                {unreadCount}
              </span>
            </button>
          );
        })}
      </div>

      {rows.length === 0 ? (
        <p style={{ opacity: 0.75 }}>No emails in this tab yet.</p>
      ) : (
        <div style={{ overflowX: "auto" }}>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px", width: 42 }}>
                  <input
                    type="checkbox"
                    checked={allRowsSelected}
                    onChange={(e) => {
                      if (e.target.checked) {
                        const ids = rows.map((row) => row.messageId);
                        setSelectedMessageIds((current) => {
                          const merged = new Set(current);
                          ids.forEach((id) => merged.add(id));
                          return Array.from(merged);
                        });
                        return;
                      }
                      const tabIDs = new Set(rows.map((row) => row.messageId));
                      setSelectedMessageIds((current) => current.filter((id) => !tabIDs.has(id)));
                    }}
                    aria-label="Select all emails in tab"
                  />
                </th>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Subject</th>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Sender</th>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Time</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((item) => (
                <tr key={`${item.messageId}-${item.atUtc}`}>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>
                    <input
                      type="checkbox"
                      checked={selectedMessageIds.includes(item.messageId)}
                      onChange={(e) => {
                        if (e.target.checked) {
                          setSelectedMessageIds((current) => (current.includes(item.messageId) ? current : [...current, item.messageId]));
                          return;
                        }
                        setSelectedMessageIds((current) => current.filter((id) => id !== item.messageId));
                      }}
                      aria-label={`Select email ${item.subject || item.messageId}`}
                    />
                  </td>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>
                    <button
                      type="button"
                      onClick={() => void openEmailDetails(item)}
                      style={{
                        padding: 0,
                        border: 0,
                        background: "transparent",
                        color: "var(--ink-strong)",
                        textAlign: "left",
                        cursor: "pointer",
                        textDecoration: "underline"
                      }}
                    >
                      {item.subject || "(no subject)"}
                    </button>
                  </td>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>{item.sender || "-"}</td>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>{formatTimestamp(item.atUtc)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {selected ? (
        <div
          role="dialog"
          aria-modal="true"
          onClick={() => setSelected(null)}
          style={{
            position: "fixed",
            inset: 0,
            background: "rgba(124, 103, 127, 0.35)",
            display: "grid",
            placeItems: "center",
            padding: 16,
            zIndex: 2000
          }}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            style={{
              width: "80%",
              background: "var(--panel)",
              border: "1px solid var(--line)",
              borderRadius: 14,
              padding: 16,
              boxShadow: "0 14px 40px rgba(128, 118, 163, 0.28)"
            }}
          >
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 10 }}>
              <h3 style={{ margin: 0 }}>Email Details</h3>
              <div style={{ display: "flex", gap: 8, flexWrap: "wrap", justifyContent: "flex-end" }}>
                <button
                  type="button"
                  onClick={() => applyInboxAction("delete", [selected.messageId], { closeModal: true })}
                  disabled={actionLoading}
                >
                  Delete
                </button>
                <button
                  type="button"
                  onClick={() => applyInboxAction("archive", [selected.messageId], { closeModal: true })}
                  disabled={actionLoading}
                >
                  Archive
                </button>
                <button
                  type="button"
                  onClick={() => applyInboxAction("spam", [selected.messageId], { closeModal: true })}
                  disabled={actionLoading}
                >
                  Mark as Spam
                </button>
                <button
                  type="button"
                  onClick={() => applyInboxAction("read", [selected.messageId])}
                  disabled={actionLoading}
                >
                  Mark as Read
                </button>
                <button type="button" onClick={() => printEmails([selected])} disabled={actionLoading}>Print</button>
                <button type="button" onClick={() => { setShowImages(true); }}>Show Images</button>
                <button type="button" onClick={() => setSelected(null)}>Close</button>
              </div>
            </div>

            <div style={{ marginTop: 12, display: "grid", gap: 8 }}>
              <p style={{ margin: 0 }}><strong>Subject:</strong> {selected.subject || "(no subject)"}</p>
              <p style={{ margin: 0 }}><strong>Sender:</strong> {selected.sender || "-"}</p>
              <p style={{ margin: 0 }}><strong>Sent To:</strong> {selected.sentTo || "-"}</p>
              <p style={{ margin: 0 }}><strong>Keyword:</strong> {selected.label || "Uncategorized"}</p>
              <p style={{ margin: 0 }}><strong>Status:</strong> {selected.status || "-"}</p>
              <p style={{ margin: 0 }}><strong>Time:</strong> {formatTimestamp(selected.atUtc)}</p>
              {selected.detail ? <p style={{ margin: 0 }}><strong>Detail:</strong> {selected.detail}</p> : null}
              <div>
                {showRawEmail ? (
                  <pre
                    key="raw"
                    style={{
                      margin: 0,
                      maxHeight: "60vh",
                      overflowY: "auto",
                      border: "1px solid var(--line)",
                      borderRadius: 8,
                      padding: "10px 12px",
                      background: "var(--bg)",
                      color: "var(--ink-strong)",
                      whiteSpace: "pre-wrap",
                      wordBreak: "break-word",
                      fontFamily: "var(--mono)"
                    }}
                  >
                    {selected.body || "No message body available."}
                  </pre>
                ) : null}
                {!showRawEmail ? (() => {
                  const body = selected.body || "No message body available.";
                  const isHtml = /<[^>]+>/.test(body);
                  
                  if (isHtml) {
                    return (
                      <div
                        key="html"
                        style={{
                          margin: 0,
                          maxHeight: "60vh",
                          overflowY: "auto",
                          border: "1px solid var(--line)",
                          borderRadius: 8,
                          padding: "10px 12px",
                          background: "var(--bg)",
                          color: "var(--ink-strong)",
                          wordBreak: "break-word"
                        }}
                        dangerouslySetInnerHTML={{ __html: processEmailHtml(body, showImages) }}
                      />
                    );
                  } else {
                    return (
                      <pre
                        key="text"
                        style={{
                          margin: 0,
                          maxHeight: "60vh",
                          overflowY: "auto",
                          border: "1px solid var(--line)",
                          borderRadius: 8,
                          padding: "10px 12px",
                          background: "var(--bg)",
                          color: "var(--ink-strong)",
                          whiteSpace: "pre-wrap",
                          wordBreak: "break-word",
                          fontFamily: "var(--mono)"
                        }}
                      >
                        {body}
                      </pre>
                    );
                  }
                })() : null}
                <div style={{ marginTop: 8, display: "flex", gap: 12, fontSize: "0.75rem", opacity: 0.7 }}>
                  {!showRawEmail && (
                    <p style={{ margin: 0 }}>Remote images are not loaded by default.</p>
                  )}
                  <button
                    type="button"
                    onClick={() => setShowRawEmail(!showRawEmail)}
                    style={{
                      padding: 0,
                      border: 0,
                      background: "transparent",
                      color: "var(--accent)",
                      cursor: "pointer",
                      textDecoration: "underline",
                      font: "inherit"
                    }}
                  >
                    {showRawEmail ? "Hide raw email" : "View raw email"}
                  </button>
                </div>
              </div>
            </div>
          </div>
        </div>
      ) : null}
    </section>
  );
}
