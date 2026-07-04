# Mobile Client Agent Sync Guide

## Purpose

This file defines the required mobile client behavior after the relay pairing update.

The mobile app must no longer rely on direct Novu credential registration as the primary path. It should use the server relay endpoint from the pairing QR payload.

## Required QR Payload Support

The app must parse these `llamalabels://novu-pair` query parameters:

- `app`: Novu application identifier
- `sub`: subscriber ID
- `hash`: subscriber hash (HMAC from server)
- `api`: Novu API base URL (optional fallback/debug)
- `srv`: server base URL for this deployment (for example cloudflared URL)
- `relay`: full relay endpoint URL for token sync
- `pt`: short-lived pairing token (90 seconds)

## Required Mobile Settings

`Server URL` must be auto-populated from QR payload and persisted by the app.

Rules:

- If QR has `srv`, use it as the active server URL for this pairing session and store it for future token refresh calls.
- The app should not require manual server URL entry during normal pairing.
- If QR has no `relay`, derive relay URL as:
  - `{Server URL}/api/notifications/novu/relay/fcm`

## Token Sync Contract

Primary token sync request:

- Method: `POST`
- URL: `relay` from QR (or derived fallback above)
- Headers: `Content-Type: application/json`
- Body:

```json
{
  "subscriberId": "<sub>",
  "pairingToken": "<pt>",
  "subscriberHash": "<hash_optional>",
  "deviceToken": "<fcm_token>"
}
```

Success criteria:

- HTTP `200`
- JSON contains `ok: true` and `synced: true`

Failure handling:

- `400`: malformed request or missing fields
- `401`: invalid/expired pairing token (`pt`) or stale/incorrect QR payload
- `502`/`503`: relay or upstream Novu issue, retry with backoff

## Pairing State Rules

- Do not mark the device as paired on QR scan alone.
- Mark as paired only after relay token sync returns success.
- If token refresh occurs later, repeat the same relay token sync call.
- If token sync fails due to expired `pt`, rescan/reload pairing QR and retry.

## Fallback Strategy

1. Use `relay` from QR.
2. If missing, derive from `srv`.
3. If `srv` missing, use user-configured Server URL setting.
4. If no valid server URL is available, block pairing completion and prompt for Server URL.

## Security Notes

- Never embed or store `NOVU_SECRET_KEY` in mobile.
- Treat `hash` as pairing proof material and store securely.
- Redact token/hash values from app logs.

## Compatibility

- Existing QR fields (`app`, `sub`, `hash`, `api`) remain supported.
- New fields (`srv`, `relay`, `pt`) are additive and should be parsed when present.
