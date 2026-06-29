import { useEffect, useRef, useState } from "react";
import { Link, Navigate, Route, Routes } from "react-router-dom";
import Quill from "quill";
import "quill/dist/quill.snow.css";
import { getJSON, postJSON } from "./api/client";
import { ConfigPage } from "./pages/ConfigPage";
import { DecisionsPage } from "./pages/DecisionsPage";
import { LoginPage } from "./pages/LoginPage";
import { LogsPage } from "./pages/LogsPage";
import { LabelsPage } from "./pages/LabelsPage";
import { ReadPage } from "./pages/ReadPage";
import { StatusPage } from "./pages/StatusPage";
import { TuningPage } from "./pages/TuningPage";

const primaryNavItems = [
  ["/read", "Inbox"]
] as const;
const mailboxNavItems = [
  ["/read?mailbox=Drafts", "Drafts"],
  ["/read?mailbox=Sent", "Sent"],
  ["/read?mailbox=Spam", "Spam"],
  ["/read?mailbox=Trash", "Trash"]
] as const;
const settingsNavItems = [
  ["/login", "Login"],
  ["/status", "Status"],
  ["/config", "Config"],
  ["/tuning", "Tuning"],
  ["/logs", "Logs"]
] as const;

type AuthState = {
  authenticated: boolean;
  username?: string;
  mustChangePassword?: boolean;
};

type InboxFoldersResponse = {
  parent: string;
  folders: string[];
};

export function App() {
  const [auth, setAuth] = useState<AuthState | null>(null);
  const [archiveOpen, setArchiveOpen] = useState(false);
  const [archiveFolders, setArchiveFolders] = useState<string[]>([]);
  const [archiveFoldersLoading, setArchiveFoldersLoading] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [composeOpen, setComposeOpen] = useState(false);
  const [composeTo, setComposeTo] = useState("");
  const [composeCc, setComposeCc] = useState("");
  const [composeBcc, setComposeBcc] = useState("");
  const [composeSubject, setComposeSubject] = useState("");
  const [composeHtmlBody, setComposeHtmlBody] = useState("");
  const [composeSending, setComposeSending] = useState(false);
  const [composeError, setComposeError] = useState("");
  const [composeSuccess, setComposeSuccess] = useState("");
  const quillEditorRef = useRef<HTMLDivElement | null>(null);
  const quillInstanceRef = useRef<Quill | null>(null);

  async function refreshAuth() {
    try {
      const next = await getJSON<AuthState>("/api/auth/me");
      setAuth(next);
    } catch {
      setAuth({ authenticated: false });
    }
  }

  useEffect(() => {
    refreshAuth();
  }, []);

  async function logout() {
    try {
      await postJSON<{ ok: boolean }>("/api/auth/logout", {});
    } finally {
      setAuth({ authenticated: false });
    }
  }

  async function loadArchiveFolders() {
    if (!auth?.authenticated) {
      setArchiveFolders([]);
      return;
    }
    setArchiveFoldersLoading(true);
    try {
      const data = await getJSON<InboxFoldersResponse>("/api/inbox/folders?parent=Archive");
      setArchiveFolders(data.folders ?? []);
    } catch {
      setArchiveFolders([]);
    } finally {
      setArchiveFoldersLoading(false);
    }
  }

  useEffect(() => {
    if (!archiveOpen) return;
    void loadArchiveFolders();
  }, [archiveOpen, auth?.authenticated]);

  useEffect(() => {
    if (!composeOpen) return;
    if (!quillEditorRef.current) return;

    if (!quillInstanceRef.current) {
      const quill = new Quill(quillEditorRef.current, {
        theme: "snow"
      });
      quill.on("text-change", () => {
        setComposeHtmlBody(quill.root.innerHTML);
      });
      quillInstanceRef.current = quill;
    }

    const editor = quillInstanceRef.current;
    if (editor && editor.root.innerHTML !== composeHtmlBody) {
      editor.root.innerHTML = composeHtmlBody;
    }
  }, [composeOpen, composeHtmlBody]);

  function resetComposeForm() {
    setComposeTo("");
    setComposeCc("");
    setComposeBcc("");
    setComposeSubject("");
    setComposeHtmlBody("");
    setComposeSending(false);
    setComposeError("");
    setComposeSuccess("");
    if (quillInstanceRef.current) {
      quillInstanceRef.current.setText("");
    }
  }

  function openComposeWindow() {
    setComposeError("");
    setComposeSuccess("");
    setComposeOpen(true);
  }

  function trashComposeDraft() {
    resetComposeForm();
    setComposeOpen(false);
  }

  async function sendComposeEmail() {
    const to = composeTo.trim();
    if (!to) {
      setComposeError("TO is required.");
      return;
    }
    setComposeSending(true);
    setComposeError("");
    setComposeSuccess("");
    const body = quillInstanceRef.current?.root.innerHTML ?? composeHtmlBody;
    try {
      await postJSON<{ ok: boolean }>("/api/mail/send", {
        to,
        cc: composeCc.trim(),
        bcc: composeBcc.trim(),
        subject: composeSubject,
        body,
        mode: "html"
      });
      setComposeSuccess("Email sent.");
      setComposeOpen(false);
      resetComposeForm();
    } catch (e) {
      const message = e instanceof Error ? e.message : "failed to send email";
      setComposeError(message);
    } finally {
      setComposeSending(false);
    }
  }

  if (auth === null) {
    return (
      <div className="shell">
        <main className="content">
          <section className="panel">
            <h2>Loading</h2>
            <p>Checking session...</p>
          </section>
        </main>
      </div>
    );
  }

  function protect(element: JSX.Element) {
    if (!auth.authenticated) {
      return <Navigate to="/login" replace />;
    }
    return element;
  }

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="sidebar-logo">
          <img src="/llamalabel.png" alt="Llama Labels" style={{ width: "100%", maxWidth: 180, display: "block", margin: "0 auto 0.75rem" }} />
        </div>
        <button type="button" className="new-email-button" onClick={openComposeWindow}>
          New Email
        </button>
        <nav>
          {primaryNavItems.map(([to, label]) => (
            <Link key={to} to={to}>
              {label}
            </Link>
          ))}

          {mailboxNavItems.map(([to, label]) => (
            <Link key={to} to={to}>
              {label}
            </Link>
          ))}

          <button
            type="button"
            className="nav-heading"
            aria-expanded={archiveOpen}
            onClick={() => setArchiveOpen((open) => !open)}
          >
            Archive {archiveOpen ? "-" : "+"}
          </button>

          {archiveOpen ? (
            <div className="nav-group">
              {archiveFoldersLoading ? <span>Loading folders...</span> : null}
              {!archiveFoldersLoading && archiveFolders.length === 0 ? <span>No archive folders</span> : null}
              {!archiveFoldersLoading
                ? archiveFolders.map((folder) => {
                    const mailboxPath = `Archive/${folder}`;
                    return (
                      <Link key={mailboxPath} to={`/read?mailbox=${encodeURIComponent(mailboxPath)}`}>
                        {folder}
                      </Link>
                    );
                  })
                : null}
            </div>
          ) : null}

          <button
            type="button"
            className="nav-heading"
            aria-expanded={settingsOpen}
            onClick={() => setSettingsOpen((open) => !open)}
          >
            Settings {settingsOpen ? "-" : "+"}
          </button>

          {settingsOpen ? (
            <div className="nav-group">
              {settingsNavItems.map(([to, label]) => (
                <Link key={to} to={to}>
                  {to === "/login" && auth.authenticated ? "Change Password" : label}
                </Link>
              ))}
              {auth.authenticated ? (
                <button type="button" className="nav-link-button" onClick={logout}>
                  Logout
                </button>
              ) : null}
            </div>
          ) : null}
        </nav>
        <div className="sidebar-footer">
          <p>&copy; 2026 &ndash; Licensed Under AGPL&nbsp;V3</p>
        </div>
      </aside>
      <main className="content">
        <Routes>
            <Route path="/" element={<Navigate to={auth.authenticated ? "/read" : "/login"} replace />} />
          <Route path="/login" element={<LoginPage auth={auth} onAuthChanged={refreshAuth} />} />
            <Route path="/read" element={protect(<ReadPage />)} />
          <Route path="/status" element={protect(<StatusPage />)} />
          <Route path="/config" element={protect(<ConfigPage />)} />
          <Route path="/tuning" element={protect(<TuningPage />)} />
          <Route path="/labels" element={protect(<LabelsPage />)} />
          <Route path="/decisions" element={protect(<DecisionsPage />)} />
          <Route path="/logs" element={protect(<LogsPage />)} />
        </Routes>
      </main>
      {composeOpen ? (
        <div className="compose-backdrop" role="dialog" aria-modal="true" onClick={() => setComposeOpen(false)}>
          <section className="compose-window" onClick={(event) => event.stopPropagation()}>
            <div className="compose-topbar">
              <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
                <button type="button" className="compose-send" onClick={() => void sendComposeEmail()} disabled={composeSending}>Send</button>
                <button type="button" className="compose-trash" onClick={trashComposeDraft} disabled={composeSending}>Trash</button>
              </div>
              <button type="button" className="compose-close" onClick={() => setComposeOpen(false)} disabled={composeSending}>Close</button>
            </div>

            {composeError ? <p className="notice notice-error" style={{ margin: 0 }}>Send failed: {composeError}</p> : null}
            {composeSuccess ? <p className="notice notice-success" style={{ margin: 0 }}>{composeSuccess}</p> : null}

            <div className="compose-form-grid">
              <label className="compose-field-row">
                <span>TO:</span>
                <input type="text" value={composeTo} onChange={(event) => setComposeTo(event.target.value)} placeholder="recipient@example.com" />
              </label>
              <label className="compose-field-row">
                <span>CC:</span>
                <input type="text" value={composeCc} onChange={(event) => setComposeCc(event.target.value)} placeholder="cc@example.com" />
              </label>
              <label className="compose-field-row">
                <span>BCC:</span>
                <input type="text" value={composeBcc} onChange={(event) => setComposeBcc(event.target.value)} placeholder="bcc@example.com" />
              </label>
              <label className="compose-field-row">
                <span>Subject:</span>
                <input type="text" value={composeSubject} onChange={(event) => setComposeSubject(event.target.value)} placeholder="Subject" />
              </label>
            </div>

            <div
              ref={quillEditorRef}
              className="compose-editor compose-editor-html"
              onKeyDown={(event) => {
                if (event.key === "Escape") {
                  event.preventDefault();
                }
              }}
            />
          </section>
        </div>
      ) : null}
    </div>
  );
}
