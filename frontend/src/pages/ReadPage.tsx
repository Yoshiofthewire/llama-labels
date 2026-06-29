import { useEffect, useMemo, useState } from "react";
import { getJSON } from "../api/client";

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
  const [showImages, setShowImages] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

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
    } catch (e) {
      const message = e instanceof Error ? e.message : "failed to load inbox";
      setError(message);
      setTabs([]);
      setByTab({});
      setActiveTab("");
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

  return (
    <section className="panel">
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <div>
          <h2 style={{ marginTop: 0, marginBottom: 6 }}>Read</h2>
          <p style={{ margin: 0, opacity: 0.75 }}>Inbox grouped by allowed IMAP keywords.</p>
        </div>
        <button type="button" onClick={loadInbox} disabled={loading}>
          {loading ? "Loading..." : "Refresh"}
        </button>
      </div>

      {error ? <p className="notice notice-error">Failed to load inbox: {error}</p> : null}

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
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Subject</th>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Sender</th>
                <th style={{ textAlign: "left", borderBottom: "1px solid var(--line)", padding: "8px" }}>Time</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((item) => (
                <tr key={`${item.messageId}-${item.atUtc}`}>
                  <td style={{ borderBottom: "1px solid var(--line)", padding: "8px" }}>
                    <button
                      type="button"
                      onClick={() => {
                        setSelected(item);
                        setShowImages(false);
                      }}
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
              <div style={{ display: "flex", gap: 8 }}>
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
                {(() => {
                  const body = selected.body && selected.body.trim() !== "" ? selected.body : "No message body available.";
                  const isHtml = /<[^>]+>/.test(body);
                  
                  if (isHtml) {
                    return (
                      <div
                        style={{
                          margin: 0,
                          maxHeight: "40vh",
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
                        style={{
                          margin: 0,
                          maxHeight: "40vh",
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
                })()}
                <p style={{ margin: "6px 0 0", fontSize: "0.75rem", opacity: 0.7 }}>
                  Remote images are not loaded by default.
                </p>
              </div>
            </div>
          </div>
        </div>
      ) : null}
    </section>
  );
}
