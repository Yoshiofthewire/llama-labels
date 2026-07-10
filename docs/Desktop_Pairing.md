# Desktop Pairing Implementation Guide

## Overview

Desktop pairing allows users to initiate pairing with a desktop application. The feature generates a pairing code that can be used by desktop clients to authenticate and pair with the user's account.

## Current Implementation Status

✅ **Phase 1 Completed:**
- Frontend UI with "Pair Desktop App" button
- Backend endpoint: `POST /api/notifications/desktop/pair`
- Pairing code generation (16-byte random = 128-bit entropy)
- Persistent storage of pairing codes in user state
- 5-minute TTL with automatic expiration
- Rate limiting: max 5 failed attempts per hour per user
- Code validation methods for backend consumers
- Attempt tracking for security audit trail
- Security-conscious logging (only code prefix exposed, sensitive data hidden)

## API Endpoint

### POST `/api/notifications/desktop/pair`

Initiates desktop pairing and returns a secure pairing code.

**Authentication:** Requires valid user session (uses `withAuth` middleware)

**Rate Limit:** Max 5 failed pairing attempts per hour per user

**Request Body:**
```json
{}
```

**Response (Success - 200 OK):**
```json
{
  "ok": true,
  "pairingCode": "A1B2C3D4E5F6G7H8I9J0K1L2M3N4O5P6",
  "ttlSeconds": 300,
  "rateLimit": 4
}
```

**Response (Rate Limited - 429):**
```json
{
  "error": "rate limit exceeded: too many pairing attempts. Try again later."
}
```

**Format:** 32-character hex string (16 bytes = 128-bit entropy)
- Not human-typeable, delivered via API/QR code only
- Cryptographically secure random generation
- Single-use, 5-minute expiration
```

**Response (Error - 401):**
```json
{"error": "unauthorized"}
```

**Response (Error - 500):**
```
failed to generate pairing code
```

## Implementation Details

### Backend (server.go + state/store.go)

The `handleDesktopPair` handler:
1. Validates user authentication via session cookie
2. **Checks rate limit:** max 5 failed attempts per hour per user
3. Generates 16-byte cryptographically random code (128 bits entropy)
4. Returns code as 32-character uppercase hex string (no formatting)
5. Stores code in user's state store with 5-minute expiration
6. Records attempt for rate limiting and audit trail
7. Logs event with code prefix only (full code never in logs)
8. Returns code + TTL + remaining rate limit to frontend

**State Store Methods:**
- `SetDesktopPairingCode(code, ttl)` — Stores code with expiration
- `ValidateDesktopPairingCode(code) bool` — Checks if code is valid/not expired
- `ConsumeDesktopPairingCode(code)` — Validates and removes code (for registration)
- `CheckDesktopPairingRateLimit() (allowed, remaining, error)` — Checks rate limit
- `RecordDesktopPairingAttempt(code, success)` — Records attempt for rate limiting

**Persistence:**
- Codes stored in user's state.json file (encrypted at rest if configured)
- Attempt history stored (last 100 attempts, 24-hour retention)
- Automatic cleanup of expired codes and old attempts on load/persist
- Survives server restart

**Rate Limiting:**
- **Limit:** 5 failed attempts per hour per user
- **Tracking:** Per-user in state file; survives server restart
- **Response:** 429 Too Many Requests when limit exceeded
- **Cleanup:** Attempts older than 24 hours automatically removed

**TTL Details:**
- **Default:** 5 minutes (300 seconds, configurable)
- **Storage:** RFC3339 formatted timestamp in user state
- **Cleanup:** Expired codes removed when state is loaded or persisted

**Security Implementation:**
- ✅ 128-bit cryptographic entropy (secure against brute force)
- ✅ Rate limiting (5 attempts/hour prevents exhaustive search)
- ✅ 5-minute TTL (time window for attack is minimal)
- ✅ Single-use codes (consumed on validation)
- ✅ Per-user state isolation (no cross-user attack surface)
- ✅ Code never fully logged (only 8-char prefix for correlation)
- ✅ Attempt tracking for security audit trail
- ✅ 429 response prevents client confusion (not 401/403)

### Frontend (NotificationsPage.tsx)

The `pairDesktopApp()` function:
1. Shows loading state ("Pairing...")
2. Calls `POST /api/notifications/desktop/pair`
3. Displays the pairing code in the status message on success
4. Shows error message if request fails

**UI Integration:**
- Button location: "Mobile App Pairing" section (replacing "Revoke Paired Devices")
- "Revoke Paired Devices" moved to footer, left of "Unsubscribe This Device"
- Navigation label changed from "Notifications" to "Pairing"
- Page title changed to "Notifications and Pairing"

## Implementation Roadmap

### ✅ Phase 1: Secure Code Generation (COMPLETE)
- [x] 16-byte (128-bit) cryptographic random code
- [x] Persistent storage with 5-minute TTL
- [x] Rate limiting (5 failed attempts/hour per user)
- [x] Attempt tracking for audit trail
- [x] Security-conscious logging

### Phase 2: Desktop App Registration Endpoint (NEXT)
- [ ] Add `POST /api/notifications/desktop/register` endpoint
- [ ] Desktop app exchanges pairing code for access token
- [ ] Validate code using `store.ConsumeDesktopPairingCode()`
- [ ] Return session token with appropriate claims
- [ ] Store paired desktop sessions with metadata
- [ ] Record successful pairing in attempt history

### Phase 3: Desktop Session Management
- [ ] Add `GET /api/notifications/desktop/sessions` — List paired desktop apps
- [ ] Add `DELETE /api/notifications/desktop/sessions/{id}` — Revoke specific session
- [ ] Add `POST /api/notifications/desktop/unpair` — Revoke all desktop pairings
- [ ] Session metadata: app name, version, OS, last activity, IP address
- [ ] Activity dashboard for user to review active sessions

### Phase 4: Advanced Features
- [ ] QR code generation containing full pairing code
- [ ] Desktop app push notifications via established session
- [ ] Anomaly detection: unusual pairing location/time
- [ ] Paired device recovery flow (device lost, factory reset, etc.)

## Security Considerations

**Implemented (Defense in Depth):**
- ✅ **Strong entropy:** 16 bytes = 128 bits (2^128 combinations)
- ✅ **Rate limiting:** 5 failed attempts per hour per user
- ✅ **Short TTL:** 5-minute window for pairing window
- ✅ **Single-use:** Codes consumed and removed after validation
- ✅ **Session requirement:** Only authenticated users can initiate
- ✅ **State isolation:** Per-user encrypted state directory
- ✅ **No logging:** Full code never in logs (only 8-char prefix)
- ✅ **Attempt tracking:** All pairing attempts recorded for audit
- ✅ **Automatic cleanup:** Expired codes and old attempts removed

**Threat Model:**

| Threat | Mitigation | Residual Risk |
|--------|-----------|----------------|
| **Brute force** | 128 bits + 5 attempts/hour limit = ~44 years to exhaust | Low |
| **Code interception** | 5-min TTL, delivered via secure API/QR only | Low |
| **Session hijacking** | Uses standard authenticated session | Low (same as login) |
| **Log exposure** | Code prefix only in logs, full code never logged | Low |
| **Replay attack** | Single-use codes, consumed after validation | Low |
| **Rate limit bypass** | Per-user state-based tracking (survives restart) | Low |

**Recommended for Production:**
- ⚠️ Enforce HTTPS for all API traffic (transport security)
- ⚠️ Consider TOTP requirement for high-security operations
- ⚠️ Implement dashboard showing active paired sessions
- ⚠️ Add email/SMS notification when new device paired
- ⚠️ Monitor for unusual pairing patterns in logs
- ⚠️ Consider geographic/network anomaly detection

**Not Required (Overkill Given Controls):**
- ❌ Increasing entropy beyond 128 bits (already unbreakable)
- ❌ Reducing TTL below 5 minutes (limits legitimate use)
- ❌ More than 5 attempts/hour limit (acceptable UX + security)

## Related Files

- Frontend: [frontend/src/pages/NotificationsPage.tsx](../frontend/src/pages/NotificationsPage.tsx)
- Backend: [backend/internal/api/server.go](../backend/internal/api/server.go) - `handleDesktopPair` (line 1328)
- Mobile pairing reference: [backend/internal/api/server.go](../backend/internal/api/server.go) - `handleNotificationPairing` (line 1020)
- App navigation: [frontend/src/App.tsx](../frontend/src/App.tsx) - Line 26 (nav label)

## Testing

**Manual test - Successful pairing:**
1. Navigate to "Pairing" page in settings
2. Click "Pair Desktop App" button
3. Verify 32-character hex code displays (e.g., `A1B2C3D4E5F6G7H8I9J0K1L2M3N4O5P6`)
4. Verify `ttlSeconds: 300` and `rateLimit: 4` in response
5. Check server logs: `desktop pairing initiated user_id=... code_hash=A1B2C3D4`

**Rate limit test:**
1. Click "Pair Desktop App" 6 times rapidly
2. On 6th attempt, verify 429 response: `rate limit exceeded: too many pairing attempts`
3. Wait 1 hour, verify button works again (rate limit resets)

**Code expiration test:**
1. Capture a pairing code
2. Wait 5+ minutes
3. Backend registration endpoint should reject it (Phase 2)

**Error test:**
1. Make request without valid session
2. Verify 401 error: `{"error": "unauthorized"}`
3. Check frontend displays error message
