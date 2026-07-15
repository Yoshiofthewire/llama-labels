import type { ReactNode } from "react";
import type { Contact } from "../api/contacts";

type RecipientAutocompleteProps = {
  query: string;
  results: Contact[];
  loading: boolean;
  searched: boolean;
  activeIndex: number;
  onHoverIndex: (i: number) => void;
  onSelect: (contact: Contact) => void;
};

function highlightMatch(text: string, query: string): ReactNode {
  if (!query) return text;
  const idx = text.toLowerCase().indexOf(query.toLowerCase());
  if (idx === -1) return text;
  const before = text.slice(0, idx);
  const match = text.slice(idx, idx + query.length);
  const after = text.slice(idx + query.length);
  return (
    <>
      {before}
      <strong className="recipient-autocomplete-match">{match}</strong>
      {after}
    </>
  );
}

export function RecipientAutocomplete({
  query,
  results,
  loading,
  searched,
  activeIndex,
  onHoverIndex,
  onSelect
}: RecipientAutocompleteProps) {
  if (!searched && !loading) {
    return null;
  }

  const showEmpty = searched && !loading && results.length === 0;

  return (
    <div className="recipient-autocomplete">
      {showEmpty ? (
        <div className="recipient-autocomplete-empty">No contacts found</div>
      ) : (
        results.map((contact, index) => {
          const email = contact.emails?.[0]?.value ?? "";
          return (
            <div
              key={contact.uid}
              className={`recipient-autocomplete-row${index === activeIndex ? " active" : ""}`}
              onMouseEnter={() => onHoverIndex(index)}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => onSelect(contact)}
            >
              <span className="recipient-autocomplete-name">{highlightMatch(contact.fn, query)}</span>
              {email ? (
                <span className="recipient-autocomplete-email">{highlightMatch(email, query)}</span>
              ) : null}
            </div>
          );
        })
      )}
    </div>
  );
}
