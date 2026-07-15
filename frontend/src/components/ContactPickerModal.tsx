import { useEffect, useMemo, useRef, useState } from "react";
import { listContacts, type Contact } from "../api/contacts";
import { toErrorMessage } from "../api/client";
import { isDuplicateInField, type RecipientToken } from "../lib/recipients";

type RecipientFieldKey = "to" | "cc" | "bcc";

type ContactPickerModalProps = {
  isOpen: boolean;
  onClose: () => void;
  toTokens: RecipientToken[];
  ccTokens: RecipientToken[];
  bccTokens: RecipientToken[];
  onAdd: (field: RecipientFieldKey, contact: Contact) => void;
};

const FIELD_BUTTONS: ReadonlyArray<{ field: RecipientFieldKey; label: string }> = [
  { field: "to", label: "TO" },
  { field: "cc", label: "CC" },
  { field: "bcc", label: "BCC" }
];

export function ContactPickerModal({ isOpen, onClose, toTokens, ccTokens, bccTokens, onAdd }: ContactPickerModalProps) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [search, setSearch] = useState("");

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (isOpen && !dialog.open) {
      dialog.showModal();
    } else if (!isOpen && dialog.open) {
      dialog.close();
    }
  }, [isOpen]);

  // Fetch the full contact list once per modal open (not per keystroke, not
  // per render) — the picker filters client-side, matching ContactsPage.tsx.
  useEffect(() => {
    if (!isOpen) return;
    let cancelled = false;
    setLoading(true);
    setLoadError("");
    listContacts()
      .then((next) => {
        if (!cancelled) setContacts(next);
      })
      .catch((error: unknown) => {
        if (!cancelled) setLoadError(toErrorMessage(error, "Failed to load contacts"));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [isOpen]);

  const filteredContacts = useMemo(() => {
    if (!search.trim()) {
      return contacts;
    }
    const lowerQuery = search.toLowerCase();
    return contacts.filter((contact) => {
      const searchableText = [contact.fn, ...(contact.emails?.map((e) => e.value) ?? []), contact.department]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      return searchableText.includes(lowerQuery);
    });
  }, [contacts, search]);

  const tokensByField: Record<RecipientFieldKey, RecipientToken[]> = {
    to: toTokens,
    cc: ccTokens,
    bcc: bccTokens
  };

  return (
    <dialog
      ref={dialogRef}
      className="contact-picker-backdrop"
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
      onClick={(event) => {
        if (event.target === dialogRef.current) {
          onClose();
        }
      }}
    >
      <div className="contact-picker-window" onClick={(event) => event.stopPropagation()}>
        <div className="contact-picker-head">
          <h3>Contacts</h3>
          <button type="button" className="contacts-action" onClick={onClose}>
            Close
          </button>
        </div>

        <input
          type="text"
          className="contact-picker-search"
          placeholder="Search by name, email, or department..."
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          autoFocus
        />

        {loading ? <p className="contacts-muted">Loading contacts...</p> : null}
        {loadError ? <p className="notice notice-error">{loadError}</p> : null}
        {!loading && !loadError && filteredContacts.length === 0 ? (
          <div className="contacts-empty">No contacts match your search.</div>
        ) : null}

        {!loading && !loadError && filteredContacts.length > 0 ? (
          <div className="contact-picker-table-wrap">
            <table className="contacts-table contact-picker-table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Email</th>
                  <th>Department</th>
                  <th className="contacts-col-actions">Actions</th>
                </tr>
              </thead>
              <tbody>
                {filteredContacts.map((contact) => {
                  const email = contact.emails?.[0]?.value;
                  const hasEmail = Boolean(email);
                  return (
                    <tr key={contact.uid} className="contacts-row contact-picker-row">
                      <td>{contact.fn}</td>
                      <td>{email ?? "—"}</td>
                      <td>{contact.department || "—"}</td>
                      <td className="contacts-col-actions">
                        <div className="contacts-actions contact-picker-actions">
                          {FIELD_BUTTONS.map(({ field, label }) => {
                            const added = hasEmail && isDuplicateInField(tokensByField[field], email as string);
                            return (
                              <button
                                key={field}
                                type="button"
                                className={`contacts-action contact-picker-button${added ? " contact-picker-button-added" : ""}`}
                                onClick={() => onAdd(field, contact)}
                                disabled={!hasEmail}
                                title={hasEmail ? undefined : "This contact has no email address"}
                              >
                                {added ? `${label} ✓` : label}
                              </button>
                            );
                          })}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ) : null}
      </div>
    </dialog>
  );
}
