# PGP QR contact card — design

## Problem

Scanning someone's PGP QR code today only yields their name, fingerprint, and
armored public key (`GET /api/pgp/qr/key`). There's no way for the person
sharing the QR code to also hand over their basic contact details (email,
phone, org, etc.), and no place on the site for a user to enter that
information in the first place.

## Scope

This repo (backend + web frontend) only. The consuming side — turning a
scanned contact card into a saved Android contact — lives in the separate
llama-mobile repo and is a follow-up, not covered here. This design only
needs to make the data available over the existing QR key-exchange API and
give the user a place to author it.

## Data model — `backend/internal/contacts`

Add one field to `Contact`:

```go
IsSelf bool `json:"isSelf,omitempty"`
```

The user's own contact card is not a new storage concept — it's an ordinary
entry in their own address book, flagged as "this one is me." This means the
existing rich contact-edit form (already covering every field: name variants,
org, title, emails, phones, addresses, notes, birthday, IMs, websites,
relations, events, phonetic names, department, custom fields, pronouns)
requires no changes to support "full Contact model fields" for the card —
it's literally a `Contact`.

`Store.Upsert` must preserve `IsSelf` from the existing record on every
update, the same way it already preserves `CreatedAt` — this keeps the flag
independent of the normal edit-save path, so editing a phone number can never
silently unmark the card. Two new `Store` methods:

- `SetSelf(uid string, self bool) (Contact, bool, error)` — clears
  `IsSelf` on every other contact first (enforcing at most one self-contact
  per user), sets/clears it on the target, bumps `Rev`/`UpdatedAt`, persists.
  Returns `(Contact{}, false, nil)` if `uid` doesn't exist.
- `GetSelf() (Contact, bool)` — scans for the (at most one) contact with
  `IsSelf == true` and not deleted.

## API changes — `backend/internal/api`

- New endpoint `POST /api/contacts/{id}/self` (session-authed via
  `s.withAuth`, matching the existing `/api/contacts/{id}/photo` pattern).
  Body: `{"self": true|false}`. Calls `store.SetSelf`, returns the updated
  contact (200) or 404 if the id doesn't exist.

- `handlePGPQRKey` (`backend/internal/api/pgp_qr_handlers.go`) — the
  unauthenticated, token-gated endpoint a scanning device hits — additionally
  resolves the token owner's contacts store via the existing
  `s.userContactsStore(userID)` helper and calls `GetSelf()`. If a
  self-contact exists, the JSON response gains a `contactCard` object with
  the shareable subset of `Contact` fields, defined as a dedicated response
  type (all fields `omitempty`) rather than marshaling `Contact` directly:

  ```go
  type pgpQRContactCard struct {
      FormattedName      string                        `json:"fn,omitempty"`
      GivenName          string                        `json:"givenName,omitempty"`
      FamilyName         string                        `json:"familyName,omitempty"`
      MiddleName         string                        `json:"middleName,omitempty"`
      Prefix             string                        `json:"prefix,omitempty"`
      Suffix             string                        `json:"suffix,omitempty"`
      Nickname           string                        `json:"nickname,omitempty"`
      Org                string                        `json:"org,omitempty"`
      Title              string                        `json:"title,omitempty"`
      Emails             []contacts.ContactValue       `json:"emails,omitempty"`
      Phones             []contacts.ContactValue       `json:"phones,omitempty"`
      Addresses          []contacts.ContactAddress     `json:"addresses,omitempty"`
      Notes              string                        `json:"notes,omitempty"`
      Birthday           string                        `json:"birthday,omitempty"`
      IMs                []contacts.ContactIM          `json:"ims,omitempty"`
      Websites           []contacts.ContactURL         `json:"websites,omitempty"`
      Relations          []contacts.ContactRelation    `json:"relations,omitempty"`
      Events             []contacts.ContactEvent       `json:"events,omitempty"`
      PhoneticGivenName  string                        `json:"phoneticGivenName,omitempty"`
      PhoneticFamilyName string                        `json:"phoneticFamilyName,omitempty"`
      Department         string                        `json:"department,omitempty"`
      CustomFields       []contacts.ContactCustomField `json:"customFields,omitempty"`
      Pronouns           string                        `json:"pronouns,omitempty"`
  }
  ```

  Deliberately excluded: `photoRef` (the photo is served from an
  authenticated route the scanning device has no session for — out of scope
  for this pass), and bookkeeping/identity fields (`uid`, `rev`, `isSelf`,
  the contact's own `pgpKey` sub-field, `mergedUIDs`/`mergedInto`) — none are
  meaningful to a scanner, and the real PGP identity already comes from the
  existing `fingerprint`/`publicKey` fields on the response, not from
  whatever a contact's own `pgpKey` field happens to hold.

  If no contact is flagged `isSelf`, the response is byte-for-byte what it
  is today (`name`, `fingerprint`, `publicKey` only) — fully backward
  compatible with the existing mobile scan flow.

## Frontend changes

- `frontend/src/api/contacts.ts`: add `isSelf?: boolean` to the `Contact`
  type; add `setContactAsSelf(uid: string, self: boolean): Promise<Contact>`
  calling `POST /api/contacts/${uid}/self`.
- `frontend/src/pages/ContactsPage.tsx`: add a toggle in the contact
  detail/edit view — "Use as my contact card" when unset, "Remove as my
  contact card" when set — calling `setContactAsSelf` and updating local
  state (clearing `isSelf` on whichever contact previously held it, setting
  it on the new one, mirroring the backend's enforced uniqueness). Add a
  small badge in the list/detail view on whichever contact has `isSelf`
  true.
- `frontend/src/pages/SecurityPage.tsx`: add a short "Contact card" status
  line near the existing PGP identity section. Fetch the contact list
  (already how the page-adjacent Contacts flows work) and find the one with
  `isSelf`. If found: "Sharing: `<fn>`" with a link to `/contacts`. If not:
  "No contact card set — add one in Contacts and mark it as yours" with the
  same link. This is the "place to enter this information" the site was
  missing — it points at the existing contact form rather than duplicating
  it.

## Testing

- `backend/internal/contacts`: table tests for `SetSelf` (sets, clears
  previous, no-op on missing uid) and `GetSelf` (none set, one set, ignores
  deleted).
- `backend/internal/api`: extend `pgp_qr_test.go` to cover
  `handlePGPQRKey` with and without a self-contact present, verifying
  `contactCard` is present/absent and that `photoRef`/bookkeeping fields
  never leak into it. Add a test for the new
  `POST /api/contacts/{id}/self` handler (set, clear, 404 on unknown id,
  uniqueness across two contacts).
- Frontend: neither page has an existing test file (no `frontend/src`
  test harness covers `ContactsPage` or `SecurityPage` today), so this
  change follows suit — verify the toggle/badge/status-line manually in
  the browser rather than adding a first test file as a side effect of
  this feature.
