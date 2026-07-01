import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { getJSON, postJSON } from "../api/client";

type InboxEmail = {
  messageId: string;
  sender: string;
  sentTo?: string;
  cc?: string;
  bcc?: string;
  subject: string;
  body?: string;
  label?: string;
  status: string;
  detail?: string;
  atUtc: string;
};

type ReadPageProps = {
  onOpenDraft?: (payload: { sentTo?: string; cc?: string; bcc?: string; subject?: string; body?: string }) => void;
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

type SortKey = "time" | "subject" | "sender";
type SortDirection = "asc" | "desc";
const EMAILS_PER_PAGE = 20;

function formatTimestamp(value: string): string {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

function formatInboxListTime(value: string): string {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;

  const now = new Date();
  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const emailStart = new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const diffDays = Math.floor((todayStart.getTime() - emailStart.getTime()) / 86_400_000);

  if (diffDays === 0) {
    return date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }
  if (diffDays === 1) {
    return "Yesterday";
  }
  if (diffDays > 1 && diffDays <= 6) {
    return date.toLocaleDateString([], { weekday: "long" });
  }
  return date.toLocaleDateString();
}

function formatUpdatedLabel(lastLoadedAt: Date | null, now: number): string {
  if (!lastLoadedAt) return "Updated Never";
  const elapsedMs = now - lastLoadedAt.getTime();
  if (elapsedMs < 3 * 60 * 1000) {
    return "Updated Just Now";
  }
  return `Updated ${lastLoadedAt.toLocaleTimeString([], {
    hour: "numeric",
    minute: "2-digit"
  })}`;
}

function processEmailHtml(html: string, showImages: boolean): string {
  // Extract body content if it's a full HTML document
  const bodyMatch = html.match(/<body[^>]*>([\s\S]*)<\/body>/i);
  const content = bodyMatch ? bodyMatch[1] : html;

  if (typeof window === "undefined") {
    if (showImages) return content;
    return content.replace(/<img[^>]*>/gi, "[Image Blocked]");
  }

  const parser = new DOMParser();
  const document = parser.parseFromString(`<div>${content}</div>`, "text/html");
  const root = document.body.firstElementChild;
  if (!root) {
    return content;
  }

  root.querySelectorAll("a[href]").forEach((anchor) => {
    anchor.setAttribute("target", "_blank");
    anchor.setAttribute("rel", "noopener noreferrer");
  });

  if (!showImages) {
    root.querySelectorAll("img").forEach((image) => {
      image.replaceWith(document.createTextNode("[Image Blocked]"));
    });
  }

  return root.innerHTML;
}

export function ReadPage({ onOpenDraft }: ReadPageProps) {
  const [searchParams] = useSearchParams();
  const mailbox = (searchParams.get("mailbox") || "").trim();
  const isInboxMailbox = mailbox.length === 0;
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
  const [sortKey, setSortKey] = useState<SortKey>("time");
  const [sortDirection, setSortDirection] = useState<SortDirection>("desc");
  const [currentPage, setCurrentPage] = useState(1);
  const [lastLoadedAt, setLastLoadedAt] = useState<Date | null>(null);
  const [clockTick, setClockTick] = useState(0);
  const isDraftMailbox = mailbox.toLowerCase().includes("drafts");
  const sourceMailbox = mailbox || "INBOX";

  async function loadInbox() {
    setLoading(true);
    setError("");
    try {
      const mailboxQuery = mailbox ? `&mailbox=${encodeURIComponent(mailbox)}` : "";
      const data = await getJSON<InboxResponse>(`/api/inbox?limit=500${mailboxQuery}`);
      setLastLoadedAt(new Date());
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
    setSelected(null);
    setSelectedMessageIds([]);
    loadInbox();
    const timer = setInterval(loadInbox, 15_000);
    return () => clearInterval(timer);
  }, [mailbox]);

  useEffect(() => {
    const timer = setInterval(() => {
      setClockTick((current) => current + 1);
    }, 30_000);
    return () => clearInterval(timer);
  }, []);

  useEffect(() => {
    if (typeof window === "undefined") return;
    const handleMailboxMove = () => {
      void loadInbox();
    };
    window.addEventListener("mailbox-move-complete", handleMailboxMove as EventListener);
    return () => {
      window.removeEventListener("mailbox-move-complete", handleMailboxMove as EventListener);
    };
  }, [mailbox]);

  const rows = useMemo(() => {
    if (isInboxMailbox) {
      if (!activeTab) return [];
      return byTab[activeTab] ?? [];
    }
    return tabs.flatMap((tab) => byTab[tab] ?? []);
  }, [isInboxMailbox, activeTab, byTab, tabs]);

  const sortedRows = useMemo(() => {
    const next = [...rows];
    const compareText = (left: string | undefined, right: string | undefined) =>
      (left ?? "").localeCompare(right ?? "", undefined, { sensitivity: "base" });
    const compareTime = (left: string, right: string) => {
      const leftTime = Date.parse(left);
      const rightTime = Date.parse(right);
      const safeLeft = Number.isNaN(leftTime) ? 0 : leftTime;
      const safeRight = Number.isNaN(rightTime) ? 0 : rightTime;
      return safeLeft - safeRight;
    };

    next.sort((left, right) => {
      const base =
        sortKey === "subject"
          ? compareText(left.subject, right.subject)
          : sortKey === "sender"
            ? compareText(left.sender, right.sender)
            : compareTime(left.atUtc, right.atUtc);
      return sortDirection === "asc" ? base : -base;
    });

    return next;
  }, [rows, sortDirection, sortKey]);

  const selectedInTab = useMemo(
    () => sortedRows.filter((row) => selectedMessageIds.includes(row.messageId)),
    [sortedRows, selectedMessageIds]
  );

  const totalPages = Math.max(1, Math.ceil(sortedRows.length / EMAILS_PER_PAGE));
  const pageRows = useMemo(() => {
    const start = (currentPage - 1) * EMAILS_PER_PAGE;
    return sortedRows.slice(start, start + EMAILS_PER_PAGE);
  }, [currentPage, sortedRows]);

  const allRowsSelected = pageRows.length > 0 && pageRows.every((row) => selectedMessageIds.includes(row.messageId));
  const updatedLabel = useMemo(
    () => formatUpdatedLabel(lastLoadedAt, Date.now()),
    [clockTick, lastLoadedAt]
  );

  const batchActions = [
    {
      key: "delete",
      label: "Delete",
      icon: "🗑",
      onClick: () => applyInboxAction("delete", selectedMessageIds),
      disabled: selectedMessageIds.length === 0 || actionLoading
    },
    {
      key: "archive",
      label: "Archive",
      icon: "📥",
      onClick: () => applyInboxAction("archive", selectedMessageIds),
      disabled: selectedMessageIds.length === 0 || actionLoading
    },
    {
      key: "spam",
      label: "Spam",
      icon: "⚠",
      onClick: () => applyInboxAction("spam", selectedMessageIds),
      disabled: selectedMessageIds.length === 0 || actionLoading
    },
    {
      key: "read",
      label: "Read",
      icon: "✓",
      onClick: () => applyInboxAction("read", selectedMessageIds),
      disabled: selectedMessageIds.length === 0 || actionLoading
    },
    {
      key: "print",
      label: "Print",
      icon: "🖨",
      onClick: () => printEmails(selectedInTab),
      disabled: selectedInTab.length === 0 || actionLoading
    }
  ] as const;

  useEffect(() => {
    setCurrentPage(1);
  }, [mailbox, activeTab, sortKey, sortDirection]);

  useEffect(() => {
    if (currentPage > totalPages) {
      setCurrentPage(totalPages);
    }
  }, [currentPage, totalPages]);

  function updateSort(nextKey: SortKey) {
    if (sortKey === nextKey) {
      setSortDirection((current) => (current === "asc" ? "desc" : "asc"));
      return;
    }
    setSortKey(nextKey);
    setSortDirection(nextKey === "time" ? "desc" : "asc");
  }

  function sortLabel(column: SortKey, label: string): string {
    if (sortKey !== column) return label;
    return `${label} ${sortDirection === "asc" ? "↑" : "↓"}`;
  }

  function dragMessagePayload(item: InboxEmail): string {
    const dragged = selectedMessageIds.includes(item.messageId) ? selectedMessageIds : [item.messageId];
    return JSON.stringify({
      messageIds: dragged,
      mailbox: sourceMailbox
    });
  }

  async function applyInboxAction(action: InboxAction, messageIds: string[], options?: { closeModal?: boolean }) {
    if (messageIds.length === 0 || actionLoading) return;
    setActionLoading(true);
    setActionError("");
    try {
      const response = await postJSON<InboxActionResponse>("/api/inbox/actions", {
        action,
        messageIds,
        mailbox
      });
      if (response.failed.length > 0) {
        const first = response.failed[0];
        throw new Error(first?.error || "some messages could not be updated");
      }
      if (action === "read") {
        const updated = new Set(messageIds);
        setByTab((current) => {
          const next: Record<string, InboxEmail[]> = {};
          Object.entries(current).forEach(([tab, items]) => {
            next[tab] = items.map((item) =>
              updated.has(item.messageId) ? { ...item, status: "read" } : item
            );
          });
          return next;
        });
        setSelected((current) => {
          if (!current || !updated.has(current.messageId)) return current;
          return { ...current, status: "read" };
        });
      } else {
        setSelectedMessageIds((current) => current.filter((id) => !messageIds.includes(id)));
        await loadInbox();
      }
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
    if (isDraftMailbox && onOpenDraft) {
      onOpenDraft({
        sentTo: item.sentTo,
        cc: item.cc,
        bcc: item.bcc,
        subject: item.subject,
        body: item.body
      });
      return;
    }
    setSelected(item);
    setShowImages(false);
    setShowRawEmail(false);
    setActionError("");
    if (item.status !== "read") {
      await applyInboxAction("read", [item.messageId]);
    }
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
    <section className="panel read-page-panel">
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <div>
          <h2 style={{ marginTop: 0, marginBottom: 6 }}>{mailbox ? mailbox : "Inbox"}</h2>
        </div>
        <div className="inbox-action-bar">
          {batchActions.map((action) => (
            <button
              key={action.key}
              type="button"
              onClick={action.onClick}
              disabled={action.disabled}
              className="inbox-action-button"
              aria-label={action.label}
              title={action.label}
            >
              <span className="inbox-action-icon" aria-hidden="true">{action.icon}</span>
              <span className="inbox-action-text">{action.label}</span>
            </button>
          ))}
        </div>
      </div>

      {error ? <p className="notice notice-error">Failed to load inbox: {error}</p> : null}
      {actionError ? <p className="notice notice-error">Inbox action failed: {actionError}</p> : null}

      {isInboxMailbox ? (
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginTop: 14, marginBottom: 14 }}>
          {tabs.map((tab) => {
            const unreadCount = (byTab[tab] ?? []).filter((item) => item.status !== "read").length;
            const isActive = activeTab === tab;
            return (
              <button
                key={tab}
                type="button"
                onClick={() => setActiveTab(tab)}
                style={{
                  background: isActive ? "var(--accent)" : "transparent",
                  color: isActive ? "var(--accent-contrast)" : "var(--ink-strong)",
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
                    background: isActive ? "var(--chip-active-bg)" : "var(--accent-soft)",
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
      ) : null}

      {sortedRows.length === 0 ? (
        <p style={{ opacity: 0.75 }}>{isInboxMailbox ? "No emails in this tab yet." : "No emails yet."}</p>
      ) : (
        <div className="inbox-list-region">
          {totalPages > 1 ? (
            <div className="inbox-page-tabs" role="tablist" aria-label="Email pages">
              {Array.from({ length: totalPages }, (_, idx) => idx + 1).map((page) => (
                <button
                  key={page}
                  type="button"
                  role="tab"
                  aria-selected={currentPage === page}
                  onClick={() => setCurrentPage(page)}
                  className={`inbox-page-tab ${currentPage === page ? "active" : ""}`}
                >
                  {page}
                </button>
              ))}
            </div>
          ) : null}
          <div className="inbox-table-wrap">
            <div className="inbox-table-scroll">
              <table className="inbox-table">
                <thead>
                  <tr>
                    <th className="inbox-col-select inbox-col-heading">
                      <input
                        type="checkbox"
                        className="inbox-checkbox"
                        checked={allRowsSelected}
                        onChange={(e) => {
                          if (e.target.checked) {
                            const ids = pageRows.map((row) => row.messageId);
                            setSelectedMessageIds((current) => {
                              const merged = new Set(current);
                              ids.forEach((id) => merged.add(id));
                              return Array.from(merged);
                            });
                            return;
                          }
                          const pageIDs = new Set(pageRows.map((row) => row.messageId));
                          setSelectedMessageIds((current) => current.filter((id) => !pageIDs.has(id)));
                        }}
                        aria-label="Select all emails in page"
                      />
                    </th>
                    <th className="inbox-col-heading">
                      <button type="button" onClick={() => updateSort("subject")} className="inbox-sort-button">
                        {sortLabel("subject", "Subject")}
                      </button>
                    </th>
                    <th className="inbox-col-heading inbox-desktop-col">
                      <button type="button" onClick={() => updateSort("sender")} className="inbox-sort-button">
                        {sortLabel("sender", "Sender")}
                      </button>
                    </th>
                    <th className="inbox-col-heading inbox-col-time inbox-desktop-col">
                      <button type="button" onClick={() => updateSort("time")} className="inbox-sort-button">
                        {sortLabel("time", "Time")}
                      </button>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {pageRows.map((item) => {
                    const isRead = item.status === "read";
                    const displayTime = formatInboxListTime(item.atUtc);
                    return (
                    <tr
                      key={`${item.messageId}-${item.atUtc}`}
                      draggable
                      onDragStart={(event) => {
                        event.dataTransfer.setData("application/x-llama-mailbox", dragMessagePayload(item));
                        event.dataTransfer.effectAllowed = "move";
                      }}
                      className={`inbox-row ${isRead ? "" : "inbox-row-unread"}`}
                    >
                      <td className="inbox-cell inbox-col-select">
                        <input
                          type="checkbox"
                          className="inbox-checkbox"
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
                      <td className="inbox-cell">
                        <button
                          type="button"
                          onClick={() => void openEmailDetails(item)}
                          className={`inbox-subject-button ${isRead ? "" : "inbox-subject-unread"}`}
                        >
                          {item.subject || "(no subject)"}
                        </button>
                        <div className="inbox-row-meta">
                          <span>{item.sender || "-"}</span>
                          <span>{displayTime}</span>
                        </div>
                      </td>
                      <td className="inbox-cell inbox-sender-cell inbox-desktop-col">{item.sender || "-"}</td>
                      <td className="inbox-cell inbox-time-cell inbox-desktop-col">{displayTime}</td>
                    </tr>
                  )})}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      )}

      <div className="inbox-updated-footer">
        <button
          type="button"
          onClick={loadInbox}
          disabled={loading || actionLoading}
          className="inbox-updated-button"
          aria-label="Refresh inbox"
          title="Refresh inbox"
        >
          {updatedLabel}
        </button>
      </div>

      {selected ? (
        <div
          className="email-reader-backdrop"
          role="dialog"
          aria-modal="true"
          onClick={() => setSelected(null)}
        >
          <div
            className="email-reader-window"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="email-reader-head">
              <h3 style={{ margin: 0 }}>Email Details</h3>
              <div className="email-reader-actions">
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

            <div className="email-reader-content">
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
                    className="email-reader-body-block"
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
                        className="email-reader-body-block"
                        dangerouslySetInnerHTML={{ __html: processEmailHtml(body, showImages) }}
                      />
                    );
                  } else {
                    return (
                      <pre
                        key="text"
                        className="email-reader-body-block"
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
