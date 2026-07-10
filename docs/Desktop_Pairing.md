# Desktop Pairing Implementation Guide

## Overview

Desktop pairing allows users to pair their desktop/web application with other devices through a secure pairing mechanism. This document describes the API contract and implementation requirements for the desktop pairing receiver.

## API Endpoint

### POST `/api/notifications/desktop/pair`

Initiates a desktop pairing request and returns a pairing token or code.

**Request Body:**
```json
{}
```

**Response (Success - 200 OK):**
```json
{
  "ok": true,
  "pairingCode": "ABCD-1234-EFGH-5678"  // Optional: human-readable pairing code
}
```

**Response (Error):**
Returns appropriate HTTP status code with error details.

## Implementation Considerations

### Frontend Integration (Already Implemented)

The `NotificationsPage.tsx` now includes:
- A "Pair Desktop App" button in the Mobile App Pairing section (replacing the previous Revoke button location)
- State management for desktop pairing status (`desktopPairingBusy`)
- A handler function `pairDesktopApp()` that:
  - Calls POST `/api/notifications/desktop/pair`
  - Displays a pairing code or success message in the status area
  - Handles errors gracefully

### Backend Requirements

The backend should implement:

1. **Pairing Token Generation**
   - Create a unique, short-lived pairing token/code
   - Store pairing state with expiration (similar to mobile app pairing)
   - Consider reusing the existing `PAIRING_SECRET` mechanism or creating a parallel system

2. **API Handler**
   - Implement `POST /api/notifications/desktop/pair` 
   - Authenticate the request using the existing session/auth system
   - Generate and return a pairing token
   - Optionally return a human-readable code for display

3. **Pairing State Management**
   - Track pending desktop pairings
   - Associate pairings with user accounts
   - Implement expiration cleanup (typically 5-10 minutes)
   - Consider supporting multiple simultaneous pairings

4. **Integration Points**
   - Desktop applications will likely receive the pairing code via:
     - QR code (similar to mobile app pairing)
     - Deep link: `llamalabels://desktop-pair?code=ABCD-1234-EFGH-5678`
     - WebSocket connection with polling

### Security Considerations

- Pairing codes should be rate-limited to prevent brute force attacks
- Codes should be short-lived (expiration 5-15 minutes recommended)
- Each pairing should require explicit user confirmation on both ends
- Consider requiring additional verification (password, 2FA) for sensitive operations
- Log all pairing events for audit purposes
- Revoke pairing on suspicious activity patterns

### Desktop App Implementation

When a desktop application receives a pairing code, it should:

1. Exchange the pairing code for an access token
2. Store the token securely (keychain/credential manager)
3. Use the token for subsequent API requests
4. Implement token refresh logic if needed
5. Provide UI to manage paired sessions and revoke access

## Related Files

- Frontend implementation: `frontend/src/pages/NotificationsPage.tsx`
- Mobile pairing reference: See existing `/api/notifications/pairing` implementation
- Configuration: Check for `PAIRING_SECRET` and related env vars in backend
