import { FormEvent, useEffect, useState } from "react";
import { toErrorMessage } from "../api/client";
import {
  createContact,
  deleteContact,
  listContacts,
  updateContact,
  type Contact,
  type ContactInput
} from "../api/contacts";

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

function formStateToInput(form: FormState): ContactInput {
  const input: ContactInput = {
    fn: form.fn.trim(),
    givenName: form.givenName.trim() || undefined,
    familyName: form.familyName.trim() || undefined,
    org: form.org.trim() || undefined,
    notes: form.notes.trim() || undefined
  };
  if (form.email.trim()) {
    input.emails = [{ value: form.email.trim() }];
  }
  if (form.phone.trim()) {
    input.phones = [{ value: form.phone.trim() }];
  }
  return input;
}

function contactDisplayLine(contact: Contact): string {
  return contact.emails?.[0]?.value ?? contact.phones?.[0]?.value ?? "";
}

export function ContactsPage() {
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("");
  const [busyId, setBusyId] = useState("");

  const [form, setForm] = useState<FormState>(emptyFormState);
  const [editingUid, setEditingUid] = useState("");
  const [saving, setSaving] = useState(false);

  const statusTone = status.toLowerCase().includes("failed") ? "notice notice-error" : "notice notice-success";

  async function refresh() {
    try {
      const next = await listContacts();
      next.sort((a, b) => a.fn.localeCompare(b.fn));
      setContacts(next);
    } catch (error: unknown) {
      setStatus(`Failed to load contacts: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void refresh();
  }, []);

  function startCreate() {
    setEditingUid("");
    setForm(emptyFormState);
  }

  function startEdit(contact: Contact) {
    setEditingUid(contact.uid);
    setForm(contactToFormState(contact));
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
      const input = formStateToInput(form);
      if (editingUid) {
        await updateContact(editingUid, input);
        setStatus(`${input.fn} updated.`);
      } else {
        await createContact(input);
        setStatus(`${input.fn} added.`);
      }
      startCreate();
      await refresh();
    } catch (error: unknown) {
      setStatus(`Failed to save contact: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setSaving(false);
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
        startCreate();
      }
      await refresh();
    } catch (error: unknown) {
      setStatus(`Failed to delete contact: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusyId("");
    }
  }

  return (
    <section className="panel contacts-page">
      <header className="contacts-header">
        <div>
          <h2>Contacts</h2>
          <p>
            Your local address book. It reaches the Llama Labels mobile app automatically once paired, and can also
            pull contacts in from an external CardDAV server or expose itself to other CardDAV apps — configure both
            under Configuration &rarr; CardDAV.
          </p>
        </div>
        {!loading && contacts.length > 0 ? (
          <div className="contacts-stats">
            <span className="contacts-stat">
              <strong>{contacts.length}</strong> contact{contacts.length === 1 ? "" : "s"}
            </span>
          </div>
        ) : null}
      </header>

      <div className="contacts-layout">
        <form onSubmit={submitForm} className="contacts-card contacts-form-card">
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
            <input value={form.phone} onChange={(e) => setForm({ ...form, phone: e.target.value })} autoComplete="off" />
          </label>
          <label>
            <div>Notes</div>
            <textarea value={form.notes} onChange={(e) => setForm({ ...form, notes: e.target.value })} rows={3} />
          </label>
          <div className="contacts-form-actions">
            <button type="submit" className="contacts-create-submit" disabled={saving}>
              {saving ? "Saving..." : editingUid ? "Save Changes" : "Add Contact"}
            </button>
            {editingUid ? (
              <button type="button" className="contacts-action" onClick={startCreate} disabled={saving}>
                Cancel
              </button>
            ) : null}
          </div>
        </form>

        <div className="contacts-card contacts-list-card">
          <div className="contacts-list-head">
            <h3>Address Book</h3>
            {!loading && contacts.length > 0 ? <span className="contacts-count">{contacts.length}</span> : null}
          </div>

          {loading ? <p className="contacts-muted">Loading contacts...</p> : null}
          {!loading && contacts.length === 0 ? <div className="contacts-empty">No contacts yet.</div> : null}

          {!loading && contacts.length > 0 ? (
            <div className="contacts-table-wrap">
              <table className="contacts-table">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Contact Info</th>
                    <th className="contacts-col-actions">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {contacts.map((contact) => {
                    const busy = busyId === contact.uid;
                    return (
                      <tr key={contact.uid} className={busy ? "contacts-row contacts-row-busy" : "contacts-row"}>
                        <td>
                          <div className="contacts-identity">
                            <span className="contacts-avatar" aria-hidden="true">
                              {contact.fn.slice(0, 1).toUpperCase() || "?"}
                            </span>
                            <div className="contacts-identity-text">
                              <span className="contacts-name">{contact.fn}</span>
                              {contact.org ? <span className="contacts-sub">{contact.org}</span> : null}
                            </div>
                          </div>
                        </td>
                        <td>{contactDisplayLine(contact) || <span className="contacts-muted">—</span>}</td>
                        <td className="contacts-col-actions">
                          <div className="contacts-actions">
                            <button
                              type="button"
                              className="contacts-action"
                              onClick={() => startEdit(contact)}
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
          ) : null}
        </div>
      </div>

      {status ? <p className={statusTone}>{status}</p> : null}
    </section>
  );
}
