# Frontend

## Purpose

React 18 single-page application for configuration, health monitoring, classification decision auditing, and log streaming. Served as static files from the Go API server after build.

## Ownership

All code under `frontend/`. Produces a static bundle under `frontend/dist/` consumed by the container.

## Local Contracts

- React 18.3, React Router 6.30, TypeScript, Vite, Quill (WYSIWYG compose editor)
- All HTTP calls go through `src/api/client.ts` (`getJSON`, `postJSON`, `putJSON`, `deleteJSON`, `postFormData`) — never use `fetch` directly in page components
- Auth state is owned by `App.tsx`; pages read it via props, not via direct `/api/auth/me` calls
- All pages live under `src/pages/`; routing is defined in `App.tsx`
- Session cookie (`credentials: 'include'`) is required on every API call — this is handled by `client.ts`
- Compose window is owned by `App.tsx`; it always uses Quill WYSIWYG and sends via `POST /api/mail/send` (window auto-closes after successful SMTP send, including success-with-warning responses) and its surface colors follow the active theme tokens
- Push notifications use `public/sw.js`; `main.tsx` registers the service worker on page load so the Notifications page can subscribe devices and receive push events. The service worker also refreshes push subscriptions when the browser expires them.

### Page → API Mapping

| Page | Endpoints used |
|------|---------------|
| `LoginPage.tsx` | `POST /api/auth/login`, `POST /api/auth/password` (`/login` sign-in plus protected `/password` change-password mode) |
| `ReadPage.tsx` | `GET /api/inbox?limit=500&mailbox=<name>`, `POST /api/inbox/actions` (bulk inbox actions + read/unread state updates, includes current mailbox context; move actions are triggered by drag-drop from this page) |
| `HealthPage.tsx` | `GET /api/health`, `GET /api/status` (includes `emailsProcessedLastHour`), `POST /api/health/repair` |
| `ConfigPage.tsx` | `GET/POST /api/imap/config` (also carries SMTP host/port for sending), `POST /api/imap/test`, `GET|POST /api/llama/auth` |
| `NotificationsPage.tsx` | `GET /api/config`, `GET /api/labels`, `PUT /api/config`, `GET /api/notifications/vapid-public-key`, `POST /api/notifications/subscriptions`, `POST /api/notifications/test` (push notification mode, all-email toggle, IMAP keyword selection, device subscription, and test push broadcast to all user devices) |
| `TuningPage.tsx` | `GET/PUT /api/tuning` |
| `LabelsPage.tsx` | `GET /api/labels` |
| `DecisionsPage.tsx` | `GET /api/decisions?limit=10` |
| `LogsPage.tsx` | `GET /api/logs?file=<name>.log&lines=<n>`, `GET /api/logs/list` |

### App Shell → API Mapping

| Component | Endpoints used |
|-----------|----------------|
| `App.tsx` | `GET /api/auth/me`, `GET /api/inbox/folders`, `POST /api/inbox/folders` (create child folder under Inbox), `PUT /api/inbox/folders` (rename custom Inbox child folder), `DELETE /api/inbox/folders?folder=<path>` (delete custom Inbox child folder after moving messages to its parent), `GET /api/inbox/folders?parent=Archive`, `POST /api/inbox/actions` (drag-drop folder moves), `POST /api/auth/logout`, `POST /api/mail/send`, `POST /api/mail/draft` |

### Theme System

- Client theme selection is local-only and persisted in browser storage (`localStorage` key `llama-lab-theme`)
- Theme presets are owned by `src/theme.ts`
- Preset names include: Dark Matter, Light Matter, Tropics, Tropic Night, Ocean, Coffee, Cliffs, Cyber Punk, Neon Purple, Space, Sky, Forest, Sun
- Theme initialization runs in `main.tsx` before rendering via `applyStoredTheme()`
- Config page includes a Theme selector and Apply Theme button in Application settings

### Auth Flow

1. App mounts → `App.useEffect` calls `GET /api/auth/me`
2. 401 → redirect to `LoginPage`
3. Successful login → session cookie set → redirect to `ReadPage`
4. First login with temporary password → `mustChangePassword` flag → redirect to password-change form

## Work Guidance

- Build: `cd frontend && npm run build`
- Dev server: `cd frontend && npm run dev` (proxies API calls to `localhost:5866`)
- Do not add direct `fetch` calls outside `src/api/client.ts`
- Add new pages to the router in `App.tsx` and the nav layout in the same file
- Left nav inbox links are driven by `GET /api/inbox/folders` for top-level non-Archive folders and `GET /api/inbox/folders?parent=Archive` for archive buckets
- Inbox row uses a right-side `+` toggle to expand/collapse the create-folder form
- Inbox sidebar folder creation uses `POST /api/inbox/folders` with `parent=INBOX`; folder names are single-level only so the server can choose the correct IMAP hierarchy delimiter
- Custom folder controls are behind a three-dot menu with Rename and Delete; built-in IMAP folders must not render this menu
- Dragging an email row from ReadPage and dropping onto a sidebar folder (including Inbox and Archive buckets) sends `POST /api/inbox/actions` with `action=move` and refreshes mailbox views via a `mailbox-move-complete` window event
- ReadPage no longer shows a manual refresh button; it shows a centered clickable "Updated Just Now" label at the bottom of the inbox page and switches to a localized time once the last inbox refresh is older than 3 minutes
- Rendered email HTML in ReadPage forces all links to open in a new tab with `target="_blank"` and `rel="noopener noreferrer"`

## Verification

- `npm run build` must succeed with zero TypeScript errors
- Playwright E2E tests live in `scripts/tests/`; run via `scripts/`

## Child DOX Index

No child AGENTS.md files. All frontend code is flat under `src/`.
