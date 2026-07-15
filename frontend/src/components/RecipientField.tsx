import { useEffect, useRef, useState } from "react";
import type { ChangeEvent, KeyboardEvent } from "react";
import type { Contact } from "../api/contacts";
import { contactToToken, isPlausibleEmail } from "../lib/recipients";
import type { RecipientFieldState, RecipientToken } from "../lib/recipients";
import { useContactAutocomplete } from "../hooks/useContactAutocomplete";
import { RecipientAutocomplete } from "./RecipientAutocomplete";

type RecipientFieldProps = {
  label: string; // "To", "Cc", "Bcc" — for placeholder/aria-label
  state: RecipientFieldState;
  onDraftChange: (draft: string) => void;
  onAddToken: (token: RecipientToken) => void;
  onRemoveToken: (index: number) => void;
};

export function RecipientField({ label, state, onDraftChange, onAddToken, onRemoveToken }: RecipientFieldProps) {
  const [activeIndex, setActiveIndex] = useState(0);
  const [dismissed, setDismissed] = useState(false);
  const [fieldError, setFieldError] = useState("");
  const justHandledRef = useRef(false);

  const trimmedDraft = state.draft.trim();
  const { results, loading, searched } = useContactAutocomplete(state.draft, trimmedDraft.length > 0);
  const isOpen = !dismissed && (loading || searched);

  useEffect(() => {
    setActiveIndex(0);
  }, [results]);

  function selectContact(contact: Contact) {
    justHandledRef.current = true;
    const token = contactToToken(contact);
    if (token) {
      onAddToken(token);
    }
    onDraftChange("");
    setFieldError("");
    setDismissed(false);
    setActiveIndex(0);
  }

  function commitDraft() {
    const trimmed = state.draft.trim();
    if (!trimmed) return;
    if (isPlausibleEmail(trimmed)) {
      onAddToken({ email: trimmed, isCustom: true });
      onDraftChange("");
      setFieldError("");
      setDismissed(false);
    } else {
      setFieldError(`"${trimmed}" doesn't look like a valid email address.`);
    }
  }

  function handleInputChange(event: ChangeEvent<HTMLInputElement>) {
    justHandledRef.current = false;
    setFieldError("");
    setDismissed(false);
    onDraftChange(event.target.value);
  }

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setActiveIndex((i) => Math.min(i + 1, Math.max(results.length - 1, 0)));
      return;
    }
    if (event.key === "ArrowUp") {
      event.preventDefault();
      setActiveIndex((i) => Math.max(i - 1, 0));
      return;
    }
    if (event.key === "Enter" || event.key === "Tab") {
      if (isOpen && results[activeIndex]) {
        if (event.key === "Enter") {
          event.preventDefault();
        }
        selectContact(results[activeIndex]);
      }
      return;
    }
    if (event.key === "Escape") {
      if (isOpen) {
        // stopPropagation() alone does not stop the parent <dialog>'s native
        // Escape-to-cancel behavior (that's tied to preventDefault(), not to
        // event bubbling) — without this, Escape would close the whole
        // compose dialog instead of just the dropdown.
        event.preventDefault();
        event.stopPropagation();
        setDismissed(true);
      }
      return;
    }
    if (event.key === "," || event.key === ";") {
      event.preventDefault();
      justHandledRef.current = true;
      commitDraft();
      return;
    }
  }

  function handleBlur() {
    if (justHandledRef.current) {
      justHandledRef.current = false;
      return;
    }
    commitDraft();
  }

  return (
    <div className="compose-token-field-wrap">
      <div className="compose-token-field">
        {state.tokens.map((token, index) => (
          <span key={`${token.email}-${index}`} className="compose-token-pill">
            <span className="compose-token-pill-label">{token.name ? `${token.name} <${token.email}>` : token.email}</span>
            <button
              type="button"
              className="compose-token-pill-remove"
              aria-label={`Remove ${token.email}`}
              onClick={() => onRemoveToken(index)}
            >
              &times;
            </button>
          </span>
        ))}
        <input
          type="text"
          className="compose-token-input"
          value={state.draft}
          placeholder={state.tokens.length === 0 ? `${label} recipients` : ""}
          aria-label={`${label} recipients`}
          onChange={handleInputChange}
          onKeyDown={handleKeyDown}
          onBlur={handleBlur}
        />
        {isOpen ? (
          <RecipientAutocomplete
            query={state.draft}
            results={results}
            loading={loading}
            searched={searched}
            activeIndex={activeIndex}
            onHoverIndex={setActiveIndex}
            onSelect={selectContact}
          />
        ) : null}
      </div>
      {fieldError ? <p className="compose-field-error">{fieldError}</p> : null}
    </div>
  );
}
