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
- Compose window is owned by `App.tsx`; it always uses Quill WYSIWYG and sends via `POST /api/mail/send`

### Page → API Mapping

| Page | Endpoints used |
|------|---------------|
| `LoginPage.tsx` | `POST /api/auth/login`, `POST /api/auth/password` |
| `ReadPage.tsx` | `GET /api/inbox?limit=500&mailbox=<name>`, `POST /api/inbox/actions` (bulk inbox actions + read/unread state updates) |
| `StatusPage.tsx` | `GET /api/status` |
| `HealthPage.tsx` | `GET /api/health`, `GET /api/status`, `POST /api/health/repair` |
| `ConfigPage.tsx` | `GET/POST /api/imap/config`, `POST /api/imap/test`, `GET|POST /api/llama/auth` |
| `TuningPage.tsx` | `GET/PUT /api/tuning` |
| `LabelsPage.tsx` | `GET /api/labels` |
| `DecisionsPage.tsx` | `GET /api/decisions?limit=10` |
| `LogsPage.tsx` | `GET /api/logs?file=<name>.log&lines=<n>`, `GET /api/logs/list` |

### App Shell → API Mapping

| Component | Endpoints used |
|-----------|----------------|
| `App.tsx` | `GET /api/auth/me`, `GET /api/inbox/folders?parent=Archive`, `POST /api/auth/logout`, `POST /api/mail/send` |

### Auth Flow

1. App mounts → `App.useEffect` calls `GET /api/auth/me`
2. 401 → redirect to `LoginPage`
3. Successful login → session cookie set → redirect to `StatusPage`
4. First login with temporary password → `mustChangePassword` flag → redirect to password-change form

## Work Guidance

- Build: `cd frontend && npm run build`
- Dev server: `cd frontend && npm run dev` (proxies API calls to `localhost:5866`)
- Do not add direct `fetch` calls outside `src/api/client.ts`
- Add new pages to the router in `App.tsx` and the nav layout in the same file

## Verification

- `npm run build` must succeed with zero TypeScript errors
- Playwright E2E tests live in `scripts/tests/`; run via `scripts/`

## Child DOX Index

No child AGENTS.md files. All frontend code is flat under `src/`.
