import { FormEvent, useEffect, useMemo, useRef, useState } from "react";
import { toErrorMessage } from "../api/client";
import {
  bulkDeleteContacts,
  createContact,
  dedupeContacts,
  deleteContact,
  exportContactsUrl,
  importContacts,
  listContacts,
  updateContact,
  type Contact,
  type ContactInput
} from "../api/contacts";
import { usePagination } from "../hooks/usePagination";
import { PageTabs } from "../components/PageTabs";

type FormState = {
  fn: string;
  givenName: string;
  familyName: string;
  org: string;
  email: string;
  phone: string;
  notes: string;
};

const emptyFormState: FormState = {
  fn: "",
  givenName: "",
  familyName: "",
  org: "",
  email: "",
  phone: "",
  notes: ""
};

const CONTACTS_PER_PAGE = 20;

function contactToFormState(contact: Contact): FormState {
  return {
    fn: contact.fn,
    givenName: contact.givenName ?? "",
    familyName: contact.familyName ?? "",
    org: contact.org ?? "",
    email: contact.emails?.[0]?.value ?? "",
    phone: contact.phones?.[0]?.value ?? "",
    notes: contact.notes ?? ""
  };
}

function formStateToInput(form: FormState, original?: Contact | null): ContactInput {
  const input: ContactInput = {
    fn: form.fn.trim(),
    givenName: form.givenName.trim() || undefined,
    familyName: form.familyName.trim() || undefined,
    org: form.org.trim() || undefined,
    notes: form.notes.trim() || undefined,
    middleName: original?.middleName,
    prefix: original?.prefix,
    suffix: original?.suffix,
    nickname: original?.nickname,
    title: original?.title,
    birthday: original?.birthday,
    addresses: original?.addresses?.length ? original.addresses : undefined
  };

  // Preserve any emails/phones beyond the first — the form only edits index 0.
  const extraEmails = original?.emails?.slice(1) ?? [];
  if (form.email.trim()) {
    input.emails = [{ value: form.email.trim(), label: original?.emails?.[0]?.label }, ...extraEmails];
  } else if (extraEmails.length) {
    input.emails = extraEmails;
  }

  const extraPhones = original?.phones?.slice(1) ?? [];
  if (form.phone.trim()) {
    input.phones = [{ value: form.phone.trim(), label: original?.phones?.[0]?.label }, ...extraPhones];
  } else if (extraPhones.length) {
    input.phones = extraPhones;
  }

  return input;
}

function hasExtraDetails(contact: Contact): boolean {
  return Boolean(
    contact.middleName ||
      contact.prefix ||
      contact.suffix ||
      contact.nickname ||
      contact.title ||
      contact.birthday ||
      (contact.emails && contact.emails.length > 1) ||
      (contact.phones && contact.phones.length > 1) ||
      (contact.addresses && contact.addresses.length > 0)
  );
}

function contactDisplayLine(contact: Contact): string {
  return contact.emails?.[0]?.value ?? contact.phones?.[0]?.value ?? "";
}

export function ContactsPage() {
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("");
  const [busyId, setBusyId] = useState("");
  const [deduping, setDeduping] = useState(false);
  const [search, setSearch] = useState("");

  const [form, setForm] = useState<FormState>(emptyFormState);
  const [editingUid, setEditingUid] = useState("");
  const [saving, setSaving] = useState(false);

  const [selectedContact, setSelectedContact] = useState<Contact | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [selectedUids, setSelectedUids] = useState<string[]>([]);
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [importing, setImporting] = useState(false);

  const contactDialogRef = useRef<HTMLDialogElement | null>(null);
  const contactFormDialogRef = useRef<HTMLDialogElement | null>(null);
  const importFileInputRef = useRef<HTMLInputElement | null>(null);

  const statusTone = status.toLowerCase().includes("failed") ? "notice notice-error" : "notice notice-success";
  const editingContact = editingUid ? contacts.find((c) => c.uid === editingUid) ?? null : null;

  const filteredContacts = useMemo(() => {
    if (!search.trim()) {
      return contacts;
    }
    const lowerQuery = search.toLowerCase();
    return contacts.filter((contact) => {
      const searchableText = [
        contact.fn,
        contact.givenName,
        contact.familyName,
        contact.org,
        ...(contact.emails?.map((e) => e.value) ?? []),
        ...(contact.phones?.map((p) => p.value) ?? [])
      ]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      return searchableText.includes(lowerQuery);
    });
  }, [contacts, search]);

  const { currentPage, setCurrentPage, totalPages, pageItems: pageContacts } = usePagination(
    filteredContacts,
    CONTACTS_PER_PAGE
  );

  useEffect(() => {
    setCurrentPage(1);
  }, [search, setCurrentPage]);

  async function loadContacts(): Promise<Contact[]> {
    const next = await listContacts();
    next.sort((a, b) => a.fn.localeCompare(b.fn));
    setContacts(next);
    return next;
  }

  async function refresh() {
    try {
      await loadContacts();
    } catch (error: unknown) {
      setStatus(`Failed to load contacts: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void refresh();
  }, []);

  useEffect(() => {
    const dialog = contactDialogRef.current;
    if (!dialog) return;
    if (selectedContact && !dialog.open) {
      dialog.showModal();
    } else if (!selectedContact && dialog.open) {
      dialog.close();
    }
  }, [selectedContact]);

  useEffect(() => {
    const dialog = contactFormDialogRef.current;
    if (!dialog) return;
    if (formOpen && !dialog.open) {
      dialog.showModal();
    } else if (!formOpen && dialog.open) {
      dialog.close();
    }
  }, [formOpen]);

  function startCreate() {
    setEditingUid("");
    setForm(emptyFormState);
  }

  function startEdit(contact: Contact) {
    setEditingUid(contact.uid);
    setForm(contactToFormState(contact));
  }

  function openCreateForm() {
    startCreate();
    setFormOpen(true);
  }

  function openEditForm(contact: Contact) {
    startEdit(contact);
    setFormOpen(true);
  }

  function closeForm() {
    startCreate();
    setFormOpen(false);
  }

  async function submitForm(e: FormEvent) {
    e.preventDefault();
    if (!form.fn.trim()) {
      setStatus("Failed: full name is required.");
      return;
    }
    setSaving(true);
    setStatus("");
    try {
      const input = formStateToInput(form, editingContact);
      if (editingUid) {
        await updateContact(editingUid, input);
        setStatus(`${input.fn} updated.`);
        closeForm();
        await refresh();
      } else {
        const created = await createContact(input);
        setStatus(`${input.fn} added.`);
        closeForm();
        const next = await loadContacts();
        const idx = next.findIndex((c) => c.uid === created.uid);
        if (idx >= 0) {
          setCurrentPage(Math.floor(idx / CONTACTS_PER_PAGE) + 1);
        }
      }
    } catch (error: unknown) {
      setStatus(`Failed to save contact: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setSaving(false);
    }
  }

  async function mergeDuplicates() {
    if (!window.confirm("Scan for duplicate contacts and merge them? This can't be undone.")) {
      return;
    }
    setDeduping(true);
    setStatus("");
    try {
      const report = await dedupeContacts();
      if (report.mergedCount === 0) {
        setStatus("No duplicate contacts found.");
      } else {
        setStatus(`Merged ${report.mergedCount} duplicate contact${report.mergedCount === 1 ? "" : "s"}.`);
        await refresh();
      }
    } catch (error: unknown) {
      setStatus(`Failed to merge duplicates: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setDeduping(false);
    }
  }

  async function removeContact(contact: Contact) {
    if (!window.confirm(`Delete ${contact.fn}?`)) {
      return;
    }
    setBusyId(contact.uid);
    setStatus("");
    try {
      await deleteContact(contact.uid);
      setStatus(`${contact.fn} deleted.`);
      if (editingUid === contact.uid) {
        closeForm();
      }
      if (selectedContact?.uid === contact.uid) {
        setSelectedContact(null);
      }
      await refresh();
    } catch (error: unknown) {
      setStatus(`Failed to delete contact: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusyId("");
    }
  }

  async function deleteBulk() {
    if (!window.confirm(`Delete ${selectedUids.length} contact${selectedUids.length === 1 ? "" : "s"}?`)) {
      return;
    }
    setBulkDeleting(true);
    setStatus("");
    try {
      const result = await bulkDeleteContacts(selectedUids);
      setSelectedUids([]);
      setStatus(`Deleted ${result.processed} contact${result.processed === 1 ? "" : "s"}.`);
      await refresh();
    } catch (error: unknown) {
      setStatus(`Failed to delete contacts: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBulkDeleting(false);
    }
  }

  async function handleImportFile() {
    const file = importFileInputRef.current?.files?.[0];
    if (!file) return;

    setImporting(true);
    setStatus("");
    try {
      const result = await importContacts(file);
      setStatus(`Imported ${result.imported} contact${result.imported === 1 ? "" : "s"}.${result.errors.length > 0 ? ` (${result.errors.length} error${result.errors.length === 1 ? "" : "s"})` : ""}`);
      await refresh();
      if (importFileInputRef.current) {
        importFileInputRef.current.value = "";
      }
    } catch (error: unknown) {
      setStatus(`Failed to import contacts: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setImporting(false);
    }
  }

  const allPageSelected = pageContacts.length > 0 && pageContacts.every((c) => selectedUids.includes(c.uid));
  const somePageSelected = pageContacts.some((c) => selectedUids.includes(c.uid));

  function toggleAllOnPage() {
    if (allPageSelected) {
      setSelectedUids(selectedUids.filter((uid) => !pageContacts.some((c) => c.uid === uid)));
    } else {
      const newUids = new Set(selectedUids);
      pageContacts.forEach((c) => newUids.add(c.uid));
      setSelectedUids(Array.from(newUids));
    }
  }

  function toggleContact(uid: string) {
    setSelectedUids(
      selectedUids.includes(uid) ? selectedUids.filter((u) => u !== uid) : [...selectedUids, uid]
    );
  }

  return (
    <section className="panel contacts-page">
      <header className="contacts-header">
        <div>
          <h2>Contacts</h2>
        </div>
        <div className="contacts-header-actions">
          {!loading && contacts.length > 0 ? (
            <div className="contacts-stats">
              <span className="contacts-stat">
                <strong>{contacts.length}</strong> contact{contacts.length === 1 ? "" : "s"}
              </span>
            </div>
          ) : null}
          {!loading && contacts.length > 1 ? (
            <button
              type="button"
              className="contacts-action"
              onClick={() => void mergeDuplicates()}
              disabled={deduping}
            >
              {deduping ? "Merging..." : "Merge Duplicates"}
            </button>
          ) : null}
          {!loading && selectedUids.length > 0 ? (
            <button
              type="button"
              className="contacts-action contacts-action-danger"
              onClick={() => void deleteBulk()}
              disabled={bulkDeleting}
            >
              {bulkDeleting ? "Deleting..." : `Delete Selected (${selectedUids.length})`}
            </button>
          ) : null}
          {!loading && contacts.length > 0 ? (
            <>
              <a href={exportContactsUrl("vcard")} className="contacts-action" download="contacts.vcf">
                Export vCard
              </a>
              <a href={exportContactsUrl("csv")} className="contacts-action" download="contacts.csv">
                Export CSV
              </a>
              <button
                type="button"
                className="contacts-action"
                onClick={() => importFileInputRef.current?.click()}
                disabled={importing}
              >
                {importing ? "Importing..." : "Import"}
              </button>
              <input
                ref={importFileInputRef}
                type="file"
                accept=".vcf"
                onChange={() => void handleImportFile()}
                style={{ display: "none" }}
              />
            </>
          ) : null}
          <button type="button" onClick={openCreateForm}>
            New Contact
          </button>
        </div>
      </header>

      <div className="contacts-card contacts-list-card">
        <div className="contacts-list-head">
          <h3>Address Book</h3>
          {!loading && contacts.length > 0 ? (
            <span className="contacts-count">
              {filteredContacts.length}
              {search.trim() ? ` of ${contacts.length}` : ""}
            </span>
          ) : null}
        </div>

        {!loading && contacts.length > 0 ? (
          <div style={{ marginBottom: "12px" }}>
            <input
              type="text"
              placeholder="Search by name, email, phone, or organization..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              style={{
                width: "100%",
                padding: "8px",
                borderRadius: "4px",
                border: "1px solid var(--border-color)",
                backgroundColor: "var(--bg-color)"
              }}
            />
          </div>
        ) : null}

        {loading ? <p className="contacts-muted">Loading contacts...</p> : null}
        {!loading && contacts.length === 0 ? <div className="contacts-empty">No contacts yet.</div> : null}
        {!loading && contacts.length > 0 && filteredContacts.length === 0 ? (
          <div className="contacts-empty">No contacts match your search.</div>
        ) : null}

        {!loading && contacts.length > 0 ? (
          <>
            <PageTabs
              totalPages={totalPages}
              currentPage={currentPage}
              onSelect={setCurrentPage}
              classPrefix="contacts"
              ariaLabel="Contact pages"
            />

            <div className="contacts-table-wrap">
              <div className="contacts-table-scroll">
                <table className="contacts-table">
                  <thead>
                    <tr>
                      <th style={{ width: "40px", textAlign: "center" }}>
                        <input
                          type="checkbox"
                          checked={allPageSelected}
                          indeterminate={somePageSelected && !allPageSelected}
                          onChange={() => toggleAllOnPage()}
                          aria-label="Select all on page"
                        />
                      </th>
                      <th>Name</th>
                      <th>Contact Info</th>
                      <th className="contacts-col-actions">Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pageContacts.map((contact) => {
                      const busy = busyId === contact.uid;
                      const isSelected = selectedUids.includes(contact.uid);
                      return (
                        <tr
                          key={contact.uid}
                          className={busy ? "contacts-row contacts-row-busy" : "contacts-row"}
                          onClick={() => setSelectedContact(contact)}
                        >
                          <td style={{ width: "40px", textAlign: "center" }} onClick={(e) => e.stopPropagation()}>
                            <input
                              type="checkbox"
                              checked={isSelected}
                              onChange={() => toggleContact(contact.uid)}
                              aria-label={`Select ${contact.fn}`}
                            />
                          </td>
                          <td>
                            <button
                              type="button"
                              className="contacts-identity contacts-row-open"
                              onClick={() => setSelectedContact(contact)}
                            >
                              <span className="contacts-avatar" aria-hidden="true">
                                {contact.fn.slice(0, 1).toUpperCase() || "?"}
                              </span>
                              <div className="contacts-identity-text">
                                <span className="contacts-name">{contact.fn}</span>
                                {contact.org ? <span className="contacts-sub">{contact.org}</span> : null}
                              </div>
                              <span className="contacts-row-chevron" aria-hidden="true">
                                &rsaquo;
                              </span>
                            </button>
                          </td>
                          <td>{contactDisplayLine(contact) || <span className="contacts-muted">—</span>}</td>
                          <td className="contacts-col-actions">
                            <div className="contacts-actions" onClick={(e) => e.stopPropagation()}>
                              <button
                                type="button"
                                className="contacts-action"
                                onClick={() => openEditForm(contact)}
                                disabled={busy}
                              >
                                Edit
                              </button>
                              <button
                                type="button"
                                className="contacts-action contacts-action-danger"
                                onClick={() => void removeContact(contact)}
                                disabled={busy}
                              >
                                {busy ? "Deleting..." : "Delete"}
                              </button>
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </div>
          </>
        ) : null}
      </div>

      {status ? <p className={statusTone}>{status}</p> : null}

      <dialog
        ref={contactFormDialogRef}
        className="contact-details-backdrop"
        onCancel={(event) => {
          event.preventDefault();
          closeForm();
        }}
        onClick={(event) => {
          if (event.target === contactFormDialogRef.current) {
            closeForm();
          }
        }}
      >
        <div className="contact-form-window" onClick={(e) => e.stopPropagation()}>
          <form onSubmit={submitForm} className="contacts-form-card">
            <h3>{editingUid ? "Edit Contact" : "Add Contact"}</h3>
            <label>
              <div>Full Name</div>
              <input value={form.fn} onChange={(e) => setForm({ ...form, fn: e.target.value })} autoComplete="off" />
            </label>
            <label>
              <div>Given Name</div>
              <input
                value={form.givenName}
                onChange={(e) => setForm({ ...form, givenName: e.target.value })}
                autoComplete="off"
              />
            </label>
            <label>
              <div>Family Name</div>
              <input
                value={form.familyName}
                onChange={(e) => setForm({ ...form, familyName: e.target.value })}
                autoComplete="off"
              />
            </label>
            <label>
              <div>Organization</div>
              <input value={form.org} onChange={(e) => setForm({ ...form, org: e.target.value })} autoComplete="off" />
            </label>
            <label>
              <div>Email</div>
              <input
                type="email"
                value={form.email}
                onChange={(e) => setForm({ ...form, email: e.target.value })}
                autoComplete="off"
              />
            </label>
            <label>
              <div>Phone</div>
              <input
                value={form.phone}
                onChange={(e) => setForm({ ...form, phone: e.target.value })}
                autoComplete="off"
              />
            </label>
            <label>
              <div>Notes</div>
              <textarea value={form.notes} onChange={(e) => setForm({ ...form, notes: e.target.value })} rows={3} />
            </label>

            {editingContact && hasExtraDetails(editingContact) ? (
              <div className="contacts-extra-details">
                <h4>Other details on file</h4>
                <p className="contacts-muted">Not editable here — preserved automatically when you save.</p>
                {editingContact.title ? (
                  <div className="contact-details-field">
                    <span>Title</span>
                    <span>{editingContact.title}</span>
                  </div>
                ) : null}
                {[editingContact.prefix, editingContact.middleName, editingContact.suffix, editingContact.nickname].some(
                  Boolean
                ) ? (
                  <div className="contact-details-field">
                    <span>Name</span>
                    <span>
                      {[editingContact.prefix, editingContact.middleName, editingContact.suffix, editingContact.nickname]
                        .filter(Boolean)
                        .join(" · ")}
                    </span>
                  </div>
                ) : null}
                {editingContact.emails && editingContact.emails.length > 1 ? (
                  <div className="contact-details-field">
                    <span>Extra emails</span>
                    <span>{editingContact.emails.slice(1).map((e) => e.value).join(", ")}</span>
                  </div>
                ) : null}
                {editingContact.phones && editingContact.phones.length > 1 ? (
                  <div className="contact-details-field">
                    <span>Extra phones</span>
                    <span>{editingContact.phones.slice(1).map((p) => p.value).join(", ")}</span>
                  </div>
                ) : null}
                {editingContact.addresses?.length ? (
                  <div className="contact-details-field">
                    <span>Address{editingContact.addresses.length > 1 ? "es" : ""}</span>
                    <span>{editingContact.addresses.length} on file</span>
                  </div>
                ) : null}
                {editingContact.birthday ? (
                  <div className="contact-details-field">
                    <span>Birthday</span>
                    <span>{editingContact.birthday}</span>
                  </div>
                ) : null}
              </div>
            ) : null}

            <div className="contacts-form-actions">
              <button type="submit" className="contacts-create-submit" disabled={saving}>
                {saving ? "Saving..." : editingUid ? "Save Changes" : "Add Contact"}
              </button>
              <button type="button" className="contacts-action" onClick={closeForm} disabled={saving}>
                Cancel
              </button>
            </div>
          </form>
        </div>
      </dialog>

      <dialog
        ref={contactDialogRef}
        className="contact-details-backdrop"
        onCancel={(event) => {
          event.preventDefault();
          setSelectedContact(null);
        }}
        onClick={(event) => {
          if (event.target === contactDialogRef.current) {
            setSelectedContact(null);
          }
        }}
      >
        {selectedContact ? (
          <div className="contact-details-window" onClick={(e) => e.stopPropagation()}>
            <div className="contact-details-head">
              <div className="contact-details-heading">
                <span className="contacts-avatar contact-details-avatar-lg" aria-hidden="true">
                  {selectedContact.fn.slice(0, 1).toUpperCase() || "?"}
                </span>
                <div>
                  <h3 style={{ margin: 0 }}>{selectedContact.fn}</h3>
                  {selectedContact.org || selectedContact.title ? (
                    <p className="contacts-sub" style={{ margin: "2px 0 0" }}>
                      {[selectedContact.title, selectedContact.org].filter(Boolean).join(" · ")}
                    </p>
                  ) : null}
                </div>
              </div>
              <div className="contact-details-actions">
                <button
                  type="button"
                  onClick={() => {
                    setSelectedContact(null);
                    openEditForm(selectedContact);
                  }}
                >
                  Edit
                </button>
                <button
                  type="button"
                  className="contacts-action-danger"
                  onClick={() => void removeContact(selectedContact)}
                >
                  Delete
                </button>
                <button type="button" onClick={() => setSelectedContact(null)}>
                  Close
                </button>
              </div>
            </div>

            <div className="contact-details-content">
              {[
                selectedContact.prefix,
                selectedContact.givenName,
                selectedContact.middleName,
                selectedContact.familyName,
                selectedContact.suffix,
                selectedContact.nickname
              ].some(Boolean) ? (
                <div className="contact-details-section">
                  <h4 className="contact-details-section-title">Name</h4>
                  {selectedContact.prefix ? (
                    <div className="contact-details-field">
                      <span>Prefix</span>
                      <span>{selectedContact.prefix}</span>
                    </div>
                  ) : null}
                  {selectedContact.givenName ? (
                    <div className="contact-details-field">
                      <span>Given Name</span>
                      <span>{selectedContact.givenName}</span>
                    </div>
                  ) : null}
                  {selectedContact.middleName ? (
                    <div className="contact-details-field">
                      <span>Middle Name</span>
                      <span>{selectedContact.middleName}</span>
                    </div>
                  ) : null}
                  {selectedContact.familyName ? (
                    <div className="contact-details-field">
                      <span>Family Name</span>
                      <span>{selectedContact.familyName}</span>
                    </div>
                  ) : null}
                  {selectedContact.suffix ? (
                    <div className="contact-details-field">
                      <span>Suffix</span>
                      <span>{selectedContact.suffix}</span>
                    </div>
                  ) : null}
                  {selectedContact.nickname ? (
                    <div className="contact-details-field">
                      <span>Nickname</span>
                      <span>{selectedContact.nickname}</span>
                    </div>
                  ) : null}
                </div>
              ) : null}

              {selectedContact.emails?.length ? (
                <div className="contact-details-section">
                  <h4 className="contact-details-section-title">{selectedContact.emails.length > 1 ? "Emails" : "Email"}</h4>
                  {selectedContact.emails.map((e, i) => (
                    <div className="contact-details-field" key={i}>
                      <span>{e.label || "Email"}</span>
                      <a href={`mailto:${e.value}`}>{e.value}</a>
                    </div>
                  ))}
                </div>
              ) : null}

              {selectedContact.phones?.length ? (
                <div className="contact-details-section">
                  <h4 className="contact-details-section-title">{selectedContact.phones.length > 1 ? "Phones" : "Phone"}</h4>
                  {selectedContact.phones.map((p, i) => (
                    <div className="contact-details-field" key={i}>
                      <span>{p.label || "Phone"}</span>
                      <a href={`tel:${p.value}`}>{p.value}</a>
                    </div>
                  ))}
                </div>
              ) : null}

              {selectedContact.addresses?.length ? (
                <div className="contact-details-section">
                  <h4 className="contact-details-section-title">
                    {selectedContact.addresses.length > 1 ? "Addresses" : "Address"}
                  </h4>
                  {selectedContact.addresses.map((a, i) => (
                    <div className="contact-details-address" key={i}>
                      {a.label ? <span className="contact-details-address-label">{a.label}</span> : null}
                      {a.street ? <span>{a.street}</span> : null}
                      {a.city || a.region || a.postalCode ? (
                        <span>{[a.city, a.region, a.postalCode].filter(Boolean).join(", ")}</span>
                      ) : null}
                      {a.country ? <span>{a.country}</span> : null}
                    </div>
                  ))}
                </div>
              ) : null}

              {selectedContact.birthday ? (
                <div className="contact-details-field">
                  <span>Birthday</span>
                  <span>{selectedContact.birthday}</span>
                </div>
              ) : null}

              {selectedContact.notes ? (
                <div className="contact-details-section">
                  <h4 className="contact-details-section-title">Notes</h4>
                  <p className="contact-details-notes">{selectedContact.notes}</p>
                </div>
              ) : null}

              {!selectedContact.org &&
              !selectedContact.title &&
              !selectedContact.emails?.length &&
              !selectedContact.phones?.length &&
              !selectedContact.addresses?.length &&
              !selectedContact.birthday &&
              !selectedContact.notes &&
              ![
                selectedContact.givenName,
                selectedContact.familyName,
                selectedContact.middleName,
                selectedContact.prefix,
                selectedContact.suffix,
                selectedContact.nickname
              ].some(Boolean) ? (
                <p className="contact-details-empty">No additional details on file.</p>
              ) : null}
            </div>
          </div>
        ) : null}
      </dialog>
    </section>
  );
}
