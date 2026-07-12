import { type DragEvent, useEffect, useRef, useState } from "react";
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import Quill from "quill";
import "quill/dist/quill.snow.css";
import { deleteJSON, getJSON, postJSON, putJSON, toErrorMessage } from "./api/client";
import { AuthContext, type AuthState } from "./auth";
import { ConfigPage } from "./pages/ConfigPage";
import { ContactsPage } from "./pages/ContactsPage";
import { HealthPage } from "./pages/HealthPage";
import { LoginPage } from "./pages/LoginPage";
import { LogsPage } from "./pages/LogsPage";
import { NotificationsPage } from "./pages/NotificationsPage";
import { ReadPage } from "./pages/ReadPage";
import { SecurityPage } from "./pages/SecurityPage";
import { TuningPage } from "./pages/TuningPage";
import { UsersPage } from "./pages/UsersPage";
import agplLicenseText from "./agpl-3.0.txt?raw";

// Bump this when releasing a new build. Shown in the license overlay.
const APP_VERSION = 1;

const settingsNavItems: ReadonlyArray<{ to: string; label: string; adminOnly?: boolean }> = [
  { to: "/login", label: "Login" },
  { to: "/health", label: "System Health" },
  { to: "/config", label: "Configuration" },
  { to: "/notifications", label: "Pairing" },
  { to: "/security", label: "Security" },
  { to: "/tuning", label: "Prompt Tuning" },
  { to: "/users", label: "Manage Users", adminOnly: true },
  { to: "/logs", label: "System Logs", adminOnly: true }
];

type BeforeInstallPromptEvent = Event & {
  prompt: () => Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed"; platform: string }>;
};

type InboxFolder = {
  path: string;
  deletable: boolean;
};

type InboxFoldersResponse = {
  parent: string;
  folders: InboxFolder[];
};

type CreateFolderResponse = {
  ok: boolean;
  parent: string;
  name: string;
  folder: string;
};

type DeleteFolderResponse = {
  ok: boolean;
  parent: string;
  folder: string;
};

type RenameFolderResponse = {
  ok: boolean;
  folder: string;
  renamed: string;
  parent: string;
};

type MoveInboxActionResponse = {
  ok: boolean;
  action: "move";
  processed: number;
  failed: Array<{ messageId: string; error: string }>;
  targetMailbox: string;
};

type DragMessagePayload = {
  messageIds: string[];
  mailbox: string;
};

type DraftComposePayload = {
  sentTo?: string;
  cc?: string;
  bcc?: string;
  subject?: string;
  body?: string;
};

// ComposeAttachment mirrors the backend's attachment wire shape
// ({name, mimeType, dataBase64}) accepted by /api/mail/send and /api/mail/draft.
// size is kept client-side only, for the chip label and the 25 MB total cap.
type ComposeAttachment = {
  name: string;
  mimeType: string;
  dataBase64: string;
  size: number;
};

// Mirror of the backend maxMailAttachmentBytes (25 MB total decoded).
const MAX_ATTACHMENT_BYTES = 25 * 1024 * 1024;

// readFileAsAttachment reads a File and strips the "data:...;base64," prefix
// that FileReader.readAsDataURL prepends, yielding the raw base64 the API wants.
function readFileAsAttachment(file: File): Promise<ComposeAttachment> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error(`failed to read ${file.name}`));
    reader.onload = () => {
      const result = typeof reader.result === "string" ? reader.result : "";
      const comma = result.indexOf(",");
      resolve({
        name: file.name,
        mimeType: file.type || "application/octet-stream",
        dataBase64: comma >= 0 ? result.slice(comma + 1) : result,
        size: file.size
      });
    };
    reader.readAsDataURL(file);
  });
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function App() {
  const location = useLocation();
  const navigate = useNavigate();
  const [auth, setAuth] = useState<AuthState | null>(null);
  const [mailboxFolders, setMailboxFolders] = useState<InboxFolder[]>([]);
  const [mailboxFoldersLoading, setMailboxFoldersLoading] = useState(false);
  const [inboxCreateOpen, setInboxCreateOpen] = useState(false);
  const [createFolderName, setCreateFolderName] = useState("");
  const [createFolderLoading, setCreateFolderLoading] = useState(false);
  const [createFolderError, setCreateFolderError] = useState("");
  const [archiveOpen, setArchiveOpen] = useState(false);
  const [archiveFolders, setArchiveFolders] = useState<InboxFolder[]>([]);
  const [archiveFoldersLoading, setArchiveFoldersLoading] = useState(false);
  const [folderMenuPath, setFolderMenuPath] = useState("");
  const [deleteFolderLoading, setDeleteFolderLoading] = useState("");
  const [renameFolderLoading, setRenameFolderLoading] = useState("");
  const [deleteFolderError, setDeleteFolderError] = useState("");
  const [dragOverFolder, setDragOverFolder] = useState("");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [pwaInstallPrompt, setPwaInstallPrompt] = useState<BeforeInstallPromptEvent | null>(null);
  const [pwaInstalled, setPwaInstalled] = useState(false);
  const [composeOpen, setComposeOpen] = useState(false);
  const [composeTo, setComposeTo] = useState("");
  const [composeCc, setComposeCc] = useState("");
  const [composeBcc, setComposeBcc] = useState("");
  const [composeSubject, setComposeSubject] = useState("");
  const [composeHtmlBody, setComposeHtmlBody] = useState("");
  const [composeSending, setComposeSending] = useState(false);
  const [composeSavingDraft, setComposeSavingDraft] = useState(false);
  const [composeError, setComposeError] = useState("");
  const [composeSuccess, setComposeSuccess] = useState("");
  const [composeAttachments, setComposeAttachments] = useState<ComposeAttachment[]>([]);
  const quillEditorRef = useRef<HTMLDivElement | null>(null);
  const quillInstanceRef = useRef<Quill | null>(null);
  const composeDialogRef = useRef<HTMLDialogElement | null>(null);
  const attachmentInputRef = useRef<HTMLInputElement | null>(null);
  const [licenseOpen, setLicenseOpen] = useState(false);
  const licenseDialogRef = useRef<HTMLDialogElement | null>(null);
  const currentMailbox = new URLSearchParams(location.search).get("mailbox")?.trim() ?? "";
  const onReadPage = location.pathname === "/read";

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

  useEffect(() => {
    if (typeof window === "undefined") {
      return;
    }

    const standalone = window.matchMedia("(display-mode: standalone)").matches ||
      (window.navigator as Navigator & { standalone?: boolean }).standalone === true;
    setPwaInstalled(standalone);

    function onBeforeInstallPrompt(event: Event) {
      event.preventDefault();
      setPwaInstallPrompt(event as BeforeInstallPromptEvent);
    }

    function onAppInstalled() {
      setPwaInstallPrompt(null);
      setPwaInstalled(true);
    }

    window.addEventListener("beforeinstallprompt", onBeforeInstallPrompt);
    window.addEventListener("appinstalled", onAppInstalled);
    return () => {
      window.removeEventListener("beforeinstallprompt", onBeforeInstallPrompt);
      window.removeEventListener("appinstalled", onAppInstalled);
    };
  }, []);

  async function logout() {
    try {
      await postJSON<{ ok: boolean }>("/api/auth/logout", {});
    } finally {
      setMailboxFolders([]);
      setArchiveFolders([]);
      setAuth({ authenticated: false });
    }
  }

  async function installPwa() {
    if (!pwaInstallPrompt) {
      return;
    }

    await pwaInstallPrompt.prompt();
    const choice = await pwaInstallPrompt.userChoice;
    setPwaInstallPrompt(null);
    if (choice.outcome === "accepted") {
      setPwaInstalled(true);
    }
  }

  async function loadMailboxFolders() {
    if (!auth?.authenticated) {
      setMailboxFolders([]);
      return;
    }
    setMailboxFoldersLoading(true);
    try {
      const data = await getJSON<InboxFoldersResponse>("/api/inbox/folders");
      setMailboxFolders(data.folders ?? []);
    } catch {
      setMailboxFolders([]);
    } finally {
      setMailboxFoldersLoading(false);
    }
  }

  async function createInboxFolder() {
    const name = createFolderName.trim();
    if (!name) {
      setCreateFolderError("Folder name is required.");
      return;
    }
    setCreateFolderLoading(true);
    setCreateFolderError("");
    setDeleteFolderError("");
    try {
      await postJSON<CreateFolderResponse>("/api/inbox/folders", {
        parent: "INBOX",
        name
      });
      setCreateFolderName("");
      setInboxCreateOpen(false);
      await loadMailboxFolders();
    } catch (e) {
      const message = toErrorMessage(e, "failed to create folder");
      setCreateFolderError(message);
    } finally {
      setCreateFolderLoading(false);
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

  async function deleteInboxFolder(folder: InboxFolder) {
    if (!folder.deletable || deleteFolderLoading || renameFolderLoading) return;
    const confirmed = typeof window === "undefined"
      ? true
      : window.confirm(`Delete ${mailboxLabel(folder.path)} and move its emails to ${mailboxLabel(folder.path.slice(0, Math.max(folder.path.lastIndexOf("/"), folder.path.lastIndexOf(".")))) || "the parent folder"}?`);
    if (!confirmed) return;

    setDeleteFolderLoading(folder.path);
    setFolderMenuPath("");
    setDeleteFolderError("");
    setCreateFolderError("");
    try {
      await deleteJSON<DeleteFolderResponse>(`/api/inbox/folders?folder=${encodeURIComponent(folder.path)}`);
      const params = new URLSearchParams(location.search);
      if (location.pathname === "/read" && params.get("mailbox") === folder.path) {
        navigate("/read", { replace: true });
      }
      await loadMailboxFolders();
    } catch (e) {
      const message = toErrorMessage(e, "failed to delete folder");
      setDeleteFolderError(message);
    } finally {
      setDeleteFolderLoading("");
    }
  }

  async function renameInboxFolder(folder: InboxFolder) {
    if (!folder.deletable || renameFolderLoading || deleteFolderLoading) return;
    const current = mailboxLabel(folder.path);
    const nextName = typeof window === "undefined" ? "" : window.prompt("Rename folder", current) ?? "";
    const name = nextName.trim();
    if (!name || name === current) {
      setFolderMenuPath("");
      return;
    }

    setRenameFolderLoading(folder.path);
    setFolderMenuPath("");
    setDeleteFolderError("");
    setCreateFolderError("");
    try {
      const response = await putJSON<RenameFolderResponse>("/api/inbox/folders", {
        folder: folder.path,
        name
      });
      const params = new URLSearchParams(location.search);
      if (location.pathname === "/read" && params.get("mailbox") === folder.path) {
        navigate(`/read?mailbox=${encodeURIComponent(response.renamed)}`, { replace: true });
      }
      await loadMailboxFolders();
    } catch (e) {
      const message = toErrorMessage(e, "failed to rename folder");
      setDeleteFolderError(message);
    } finally {
      setRenameFolderLoading("");
    }
  }

  function parseDragPayload(raw: string): DragMessagePayload | null {
    try {
      const parsed = JSON.parse(raw) as DragMessagePayload;
      if (!Array.isArray(parsed.messageIds) || parsed.messageIds.length === 0) return null;
      const messageIds = parsed.messageIds.map((value) => String(value).trim()).filter(Boolean);
      const mailbox = String(parsed.mailbox || "").trim();
      if (messageIds.length === 0 || mailbox === "") return null;
      return { messageIds, mailbox };
    } catch {
      return null;
    }
  }

  async function moveDraggedMessages(targetMailbox: string, event: DragEvent<HTMLElement>) {
    event.preventDefault();
    setDragOverFolder("");
    const payload = parseDragPayload(event.dataTransfer.getData("application/x-llama-mailbox"));
    if (!payload) return;
    if (payload.mailbox.toLowerCase() === targetMailbox.toLowerCase()) return;

    setDeleteFolderError("");
    setCreateFolderError("");
    try {
      const response = await postJSON<MoveInboxActionResponse>("/api/inbox/actions", {
        action: "move",
        mailbox: payload.mailbox,
        targetMailbox,
        messageIds: payload.messageIds
      });
      if (response.failed.length > 0) {
        throw new Error(response.failed[0]?.error || "some emails could not be moved");
      }
      if (typeof window !== "undefined") {
        window.dispatchEvent(new CustomEvent("mailbox-move-complete", {
          detail: {
            sourceMailbox: payload.mailbox,
            targetMailbox
          }
        }));
      }
    } catch (e) {
      const message = toErrorMessage(e, "failed to move email");
      setDeleteFolderError(message);
    }
  }

  useEffect(() => {
    if (!auth?.authenticated) {
      setMailboxFolders([]);
      setInboxCreateOpen(false);
      setCreateFolderError("");
      setCreateFolderName("");
      setFolderMenuPath("");
      setDeleteFolderError("");
      setDragOverFolder("");
      return;
    }
    void loadMailboxFolders();
  }, [auth?.authenticated]);

  useEffect(() => {
    if (!archiveOpen) return;
    void loadArchiveFolders();
  }, [archiveOpen, auth?.authenticated]);

  useEffect(() => {
    if (!composeOpen) return;
    if (!quillEditorRef.current) return;

    if (quillInstanceRef.current && quillInstanceRef.current.container !== quillEditorRef.current) {
      quillInstanceRef.current = null;
    }

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

  useEffect(() => {
    const dialog = composeDialogRef.current;
    if (!dialog) return;
    if (composeOpen && !dialog.open) {
      dialog.showModal();
    } else if (!composeOpen && dialog.open) {
      dialog.close();
    }
  }, [composeOpen]);

  useEffect(() => {
    const dialog = licenseDialogRef.current;
    if (!dialog) return;
    if (licenseOpen && !dialog.open) {
      dialog.showModal();
    } else if (!licenseOpen && dialog.open) {
      dialog.close();
    }
  }, [licenseOpen]);

  function resetComposeForm() {
    setComposeTo("");
    setComposeCc("");
    setComposeBcc("");
    setComposeSubject("");
    setComposeHtmlBody("");
    setComposeSending(false);
    setComposeError("");
    setComposeSuccess("");
    setComposeAttachments([]);
    if (attachmentInputRef.current) {
      attachmentInputRef.current.value = "";
    }
    if (quillInstanceRef.current) {
      quillInstanceRef.current.setText("");
    }
  }

  async function handleAttachmentPick(event: React.ChangeEvent<HTMLInputElement>) {
    const files = Array.from(event.target.files ?? []);
    event.target.value = ""; // allow re-picking the same file
    if (files.length === 0) return;
    setComposeError("");
    try {
      const picked = await Promise.all(files.map(readFileAsAttachment));
      setComposeAttachments((current) => {
        const next = [...current, ...picked];
        const total = next.reduce((sum, a) => sum + a.size, 0);
        if (total > MAX_ATTACHMENT_BYTES) {
          setComposeError(`Attachments too large (max ${formatBytes(MAX_ATTACHMENT_BYTES)} total).`);
          return current;
        }
        return next;
      });
    } catch (e) {
      setComposeError(toErrorMessage(e, "failed to read attachment"));
    }
  }

  function removeComposeAttachment(index: number) {
    setComposeAttachments((current) => current.filter((_, i) => i !== index));
  }

  function openComposeWindow() {
    resetComposeForm();
    setComposeError("");
    setComposeSuccess("");
    setComposeOpen(true);
  }

  function openDraftInCompose(payload: DraftComposePayload) {
    setComposeTo(payload.sentTo ?? "");
    setComposeCc(payload.cc ?? "");
    setComposeBcc(payload.bcc ?? "");
    setComposeSubject(payload.subject ?? "");
    setComposeHtmlBody(payload.body ?? "");
    setComposeError("");
    setComposeSuccess("");
    setComposeOpen(true);
  }

  function trashComposeDraft() {
    resetComposeForm();
    setComposeOpen(false);
  }

  function closeComposeWindow() {
    setComposeOpen(false);
    resetComposeForm();
  }

  function mailboxLabel(path: string): string {
    const clean = path.trim();
    if (!clean) return "";
    const parts = clean.replace(/^INBOX[/.]/i, "").split(/[/.]/).filter(Boolean);
    return parts[parts.length - 1] ?? clean;
  }

  function standardMailboxKey(path: string): string {
    const value = mailboxLabel(path).trim().toLowerCase();
    if (!value) return "custom";
    if (["inbox", "draft", "drafts", "junk", "spam", "sent", "trash"].includes(value)) {
      return value;
    }
    return "custom";
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
      await postJSON<{ ok: boolean; sentSaved?: boolean; warning?: string }>("/api/mail/send", {
        to,
        cc: composeCc.trim(),
        bcc: composeBcc.trim(),
        subject: composeSubject,
        body,
        mode: "html",
        attachments: composeAttachments.map(({ name, mimeType, dataBase64 }) => ({ name, mimeType, dataBase64 }))
      });
      setComposeOpen(false);
      resetComposeForm();
    } catch (e) {
      const message = toErrorMessage(e, "failed to send email");
      setComposeError(message);
    } finally {
      setComposeSending(false);
    }
  }

  async function saveComposeDraft() {
    const to = composeTo.trim();
    if (!to) {
      setComposeError("TO is required.");
      return;
    }
    setComposeSavingDraft(true);
    setComposeError("");
    setComposeSuccess("");
    const body = quillInstanceRef.current?.root.innerHTML ?? composeHtmlBody;
    try {
      await postJSON<{ ok: boolean }>("/api/mail/draft", {
        to,
        cc: composeCc.trim(),
        bcc: composeBcc.trim(),
        subject: composeSubject,
        body,
        mode: "html",
        attachments: composeAttachments.map(({ name, mimeType, dataBase64 }) => ({ name, mimeType, dataBase64 }))
      });
      setComposeSuccess("Draft saved.");
    } catch (e) {
      const message = toErrorMessage(e, "failed to save draft");
      setComposeError(message);
    } finally {
      setComposeSavingDraft(false);
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

  const isAdmin = auth.role === "admin";

  function protect(element: JSX.Element, adminOnly = false) {
    if (!auth?.authenticated) {
      return <Navigate to="/login" replace />;
    }
    if (adminOnly && !isAdmin) {
      return <Navigate to="/read" replace />;
    }
    return element;
  }

  return (
    <AuthContext.Provider value={auth}>
    <div className="shell">
      <aside className="sidebar">
        <div className="sidebar-logo">
          <img className="sidebar-llama-logo" src="/llamalabel.png" alt="Llama Labels" style={{ width: "100%", maxWidth: 180, display: "block", margin: "0 auto 0.75rem" }} />
        </div>
        <button type="button" className="new-email-button" onClick={openComposeWindow}>
          New Email
        </button>
        <nav>
          <p className="sidebar-section-label">Mailboxes</p>
          <div className="mobile-quick-nav" aria-label="Mobile mailboxes">
            <Link className={onReadPage && currentMailbox === "" ? "sidebar-link-active" : ""} to="/read">Inbox</Link>
            <Link className={onReadPage && currentMailbox.toLowerCase() === "drafts" ? "sidebar-link-active" : ""} to="/read?mailbox=Drafts">Drafts</Link>
            <Link className={onReadPage && currentMailbox.toLowerCase() === "junk" ? "sidebar-link-active" : ""} to="/read?mailbox=Junk">Junk</Link>
            <Link className={onReadPage && currentMailbox.toLowerCase() === "sent" ? "sidebar-link-active" : ""} to="/read?mailbox=Sent">Sent</Link>
            <Link className={onReadPage && currentMailbox.toLowerCase() === "trash" ? "sidebar-link-active" : ""} to="/read?mailbox=Trash">Trash</Link>
            <button
              type="button"
              className="mobile-settings-toggle"
              aria-label="Toggle settings"
              title="Settings"
              onClick={() => setSettingsOpen((open) => !open)}
            >
              Settings
            </button>
          </div>
          <div className="inbox-nav-row">
            <Link
              to="/read"
              className={[dragOverFolder === "INBOX" ? "drop-target-active" : "", onReadPage && currentMailbox === "" ? "sidebar-link-active" : ""].filter(Boolean).join(" ")}
              onDragOver={(event) => {
                event.preventDefault();
                setDragOverFolder("INBOX");
              }}
              onDragLeave={() => setDragOverFolder("")}
              onDrop={(event) => {
                void moveDraggedMessages("INBOX", event);
              }}
            >
              Inbox
            </Link>
            <button
              type="button"
              className="inbox-expand-button"
              aria-expanded={inboxCreateOpen}
              onClick={() => {
                setInboxCreateOpen((open) => !open);
                setCreateFolderError("");
              }}
            >
              +
            </button>
          </div>
          <div className="nav-group">
            {inboxCreateOpen ? (
              <form
                className="sidebar-folder-form"
                onSubmit={(event) => {
                  event.preventDefault();
                  void createInboxFolder();
                }}
              >
                <input
                  type="text"
                  value={createFolderName}
                  onChange={(event) => setCreateFolderName(event.target.value)}
                  placeholder="New folder under Inbox"
                  disabled={createFolderLoading}
                />
                <button type="submit" disabled={createFolderLoading}>
                  {createFolderLoading ? "Creating..." : "Create Folder"}
                </button>
              </form>
            ) : null}
            {createFolderError ? <span className="sidebar-folder-error">{createFolderError}</span> : null}
            {deleteFolderError ? <span className="sidebar-folder-error">{deleteFolderError}</span> : null}
            {mailboxFoldersLoading ? <span>Loading folders...</span> : null}
            {!mailboxFoldersLoading
              ? mailboxFolders.map((folder) => (
                  <div key={folder.path} className="sidebar-folder-row" data-folder-kind={standardMailboxKey(folder.path)}>
                    <Link
                      to={`/read?mailbox=${encodeURIComponent(folder.path)}`}
                      className={[
                        dragOverFolder === folder.path ? "drop-target-active" : "",
                        onReadPage && currentMailbox.toLowerCase() === folder.path.toLowerCase() ? "sidebar-link-active" : ""
                      ].filter(Boolean).join(" ")}
                      onDragOver={(event) => {
                        event.preventDefault();
                        setDragOverFolder(folder.path);
                      }}
                      onDragLeave={() => setDragOverFolder("")}
                      onDrop={(event) => {
                        void moveDraggedMessages(folder.path, event);
                      }}
                    >
                      {mailboxLabel(folder.path)}
                    </Link>
                    {folder.deletable ? (
                      <div className="sidebar-folder-menu-wrap">
                        <button
                          type="button"
                          className="sidebar-folder-menu-button"
                          aria-label={`Folder options for ${mailboxLabel(folder.path)}`}
                          onClick={() => setFolderMenuPath((current) => (current === folder.path ? "" : folder.path))}
                          disabled={deleteFolderLoading === folder.path || renameFolderLoading === folder.path}
                        >
                          ...
                        </button>
                        {folderMenuPath === folder.path ? (
                          <div className="sidebar-folder-menu">
                            <button
                              type="button"
                              onClick={() => void renameInboxFolder(folder)}
                              disabled={renameFolderLoading === folder.path}
                            >
                              {renameFolderLoading === folder.path ? "Renaming..." : "Rename"}
                            </button>
                            <button
                              type="button"
                              onClick={() => void deleteInboxFolder(folder)}
                              disabled={deleteFolderLoading === folder.path}
                            >
                              {deleteFolderLoading === folder.path ? "Deleting..." : "Delete"}
                            </button>
                          </div>
                        ) : null}
                      </div>
                    ) : null}
                  </div>
                ))
              : null}
          </div>

          <button
            type="button"
            className="nav-heading archive-toggle"
            aria-expanded={archiveOpen}
            onClick={() => setArchiveOpen((open) => !open)}
          >
            Archive {archiveOpen ? "-" : "+"}
          </button>

          {archiveOpen ? (
            <div className="nav-group archive-group">
              {archiveFoldersLoading ? <span>Loading folders...</span> : null}
              {!archiveFoldersLoading && archiveFolders.length === 0 ? <span>No archive folders</span> : null}
              {!archiveFoldersLoading
                ? archiveFolders.map((folder) => (
                    <Link
                      key={folder.path}
                      to={`/read?mailbox=${encodeURIComponent(folder.path)}`}
                      className={[
                        dragOverFolder === folder.path ? "drop-target-active" : "",
                        onReadPage && currentMailbox.toLowerCase() === folder.path.toLowerCase() ? "sidebar-link-active" : ""
                      ].filter(Boolean).join(" ")}
                      onDragOver={(event) => {
                        event.preventDefault();
                        setDragOverFolder(folder.path);
                      }}
                      onDragLeave={() => setDragOverFolder("")}
                      onDrop={(event) => {
                        void moveDraggedMessages(folder.path, event);
                      }}
                    >
                      {mailboxLabel(folder.path)}
                    </Link>
                  ))
                : null}
            </div>
          ) : null}

          <Link
            to="/contacts"
            className={["nav-heading", location.pathname === "/contacts" ? "sidebar-link-active" : ""].filter(Boolean).join(" ")}
          >
            Contacts
          </Link>

          <button
            type="button"
            className="nav-heading settings-heading"
            aria-expanded={settingsOpen}
            onClick={() => setSettingsOpen((open) => !open)}
          >
            Settings {settingsOpen ? "-" : "+"}
          </button>

          {settingsOpen ? (
            <div className="nav-group">
              {settingsNavItems
                .filter(({ adminOnly }) => !adminOnly || isAdmin)
                .map(({ to, label }) => (
                <Link
                  key={to}
                  className={(to === "/login" && auth.authenticated ? "/password" : to) === location.pathname ? "sidebar-link-active" : ""}
                  to={to === "/login" && auth.authenticated ? "/password" : to}
                >
                  {to === "/login" && auth.authenticated ? "Change Password" : label}
                </Link>
              ))}
              {!pwaInstalled ? (
                <button
                  type="button"
                  className="nav-link-button"
                  onClick={() => void installPwa()}
                  disabled={!pwaInstallPrompt}
                  title={pwaInstallPrompt ? "Install this site as a PWA" : "Wait for browser install support"}
                >
                  Install PWA
                </button>
              ) : (
                <span title="This site is already installed as a PWA">PWA Installed</span>
              )}
            </div>
          ) : null}
          {auth.authenticated ? (
            <button type="button" className="nav-link-button nav-heading" onClick={logout}>
              Logout
            </button>
          ) : null}
        </nav>
        <div className="sidebar-footer">
          <p>
            <button type="button" className="license-link" onClick={() => setLicenseOpen(true)}>
              &copy; {new Date().getFullYear()} &ndash; Licensed Under AGPL&nbsp;V3
            </button>
          </p>
        </div>
      </aside>
      <main className="content">
        <Routes>
            <Route path="/" element={<Navigate to={auth.authenticated ? "/read" : "/login"} replace />} />
          <Route path="/login" element={<LoginPage auth={auth} onAuthChanged={refreshAuth} />} />
          <Route path="/password" element={protect(<LoginPage auth={auth} onAuthChanged={refreshAuth} mode="password" />)} />
              <Route path="/read" element={protect(<ReadPage onOpenDraft={openDraftInCompose} />)} />
          <Route path="/health" element={protect(<HealthPage />)} />
          <Route path="/config" element={protect(<ConfigPage />)} />
          <Route path="/notifications" element={protect(<NotificationsPage />)} />
          <Route path="/security" element={protect(<SecurityPage />)} />
          <Route path="/contacts" element={protect(<ContactsPage />)} />
          <Route path="/tuning" element={protect(<TuningPage />)} />
          <Route path="/users" element={protect(<UsersPage />, true)} />
          <Route path="/logs" element={protect(<LogsPage />, true)} />
        </Routes>
      </main>
      <dialog
        ref={licenseDialogRef}
        className="compose-backdrop"
        onCancel={() => setLicenseOpen(false)}
        onClick={(event) => {
          if (event.target === licenseDialogRef.current) {
            setLicenseOpen(false);
          }
        }}
      >
        <div className="license-window">
          <div className="license-window-header">
            <div className="license-window-title">
              <div className="license-title-main">
                <span className="license-app-name">llama Mail</span>
                <span className="license-version-badge">v{APP_VERSION}</span>
              </div>
              <p className="license-title-sub">Developed by Busnes Games</p>
              <p className="license-title-sub">
                &copy; {new Date().getFullYear()} &middot; Licensed under AGPL&nbsp;v3
              </p>
            </div>
            <button type="button" className="nav-link-button" onClick={() => setLicenseOpen(false)}>
              Close
            </button>
          </div>
          <textarea className="license-text" readOnly value={agplLicenseText} />
        </div>
      </dialog>
      <dialog
        ref={composeDialogRef}
        className="compose-backdrop"
        onCancel={(event) => {
          if (composeSending) {
            event.preventDefault();
            return;
          }
          closeComposeWindow();
        }}
        onClick={(event) => {
          if (event.target === composeDialogRef.current && !composeSending) {
            closeComposeWindow();
          }
        }}
      >
          <section
            className={`compose-window${composeSending ? " compose-window-sending" : ""}`}
            onClick={(event) => event.stopPropagation()}
          >
            <div className="compose-topbar">
              <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
                <button type="button" className="compose-send" onClick={() => void sendComposeEmail()} disabled={composeSending || composeSavingDraft}>{composeSending ? "Sending..." : "Send"}</button>
                <button type="button" className="compose-save-draft" onClick={() => void saveComposeDraft()} disabled={composeSending || composeSavingDraft}>Save Draft</button>
                <button type="button" className="compose-attach" onClick={() => attachmentInputRef.current?.click()} disabled={composeSending || composeSavingDraft}>📎 Attach</button>
                <button type="button" className="compose-trash" onClick={trashComposeDraft} disabled={composeSending || composeSavingDraft}>Trash</button>
                <input
                  ref={attachmentInputRef}
                  type="file"
                  multiple
                  style={{ display: "none" }}
                  onChange={(event) => void handleAttachmentPick(event)}
                />
              </div>
              <button type="button" className="compose-close" onClick={closeComposeWindow} disabled={composeSending || composeSavingDraft}>Close</button>
            </div>

            {composeError ? <p className="notice notice-error" style={{ margin: 0 }}>Send failed: {composeError}</p> : null}
            {composeSuccess ? <p className="notice notice-success" style={{ margin: 0 }}>{composeSuccess}</p> : null}

            <div className="compose-form-grid">
              <label className="compose-field-row">
                <span>TO:</span>
                <input type="text" value={composeTo} onChange={(event) => setComposeTo(event.target.value)} placeholder="recipient@example.com" disabled={composeSending || composeSavingDraft} />
              </label>
              <label className="compose-field-row">
                <span>CC:</span>
                <input type="text" value={composeCc} onChange={(event) => setComposeCc(event.target.value)} placeholder="cc@example.com" disabled={composeSending || composeSavingDraft} />
              </label>
              <label className="compose-field-row">
                <span>BCC:</span>
                <input type="text" value={composeBcc} onChange={(event) => setComposeBcc(event.target.value)} placeholder="bcc@example.com" disabled={composeSending || composeSavingDraft} />
              </label>
              <label className="compose-field-row">
                <span>Subject:</span>
                <input type="text" value={composeSubject} onChange={(event) => setComposeSubject(event.target.value)} placeholder="Subject" disabled={composeSending || composeSavingDraft} />
              </label>
            </div>

            {composeAttachments.length > 0 ? (
              <div className="compose-attachments">
                {composeAttachments.map((attachment, index) => (
                  <span key={`${attachment.name}-${index}`} className="compose-attachment-chip">
                    <span className="compose-attachment-name">{attachment.name}</span>
                    <span className="compose-attachment-size">({formatBytes(attachment.size)})</span>
                    <button
                      type="button"
                      className="compose-attachment-remove"
                      aria-label={`Remove ${attachment.name}`}
                      onClick={() => removeComposeAttachment(index)}
                      disabled={composeSending || composeSavingDraft}
                    >
                      ✕
                    </button>
                  </span>
                ))}
              </div>
            ) : null}

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
      </dialog>
    </div>
    </AuthContext.Provider>
  );
}
