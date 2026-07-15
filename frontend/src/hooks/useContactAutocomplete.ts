import { useEffect, useState } from "react";
import { searchContacts } from "../api/contacts";
import type { Contact } from "../api/contacts";

export function useContactAutocomplete(query: string, enabled: boolean) {
  const [results, setResults] = useState<Contact[]>([]);
  const [loading, setLoading] = useState(false);
  const [searched, setSearched] = useState(false);

  useEffect(() => {
    const trimmed = query.trim();
    if (!enabled || !trimmed) {
      setResults([]);
      setLoading(false);
      setSearched(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    const timeoutId = setTimeout(() => {
      searchContacts(trimmed, 5)
        .then(({ contacts }) => {
          if (cancelled) return;
          setResults(contacts);
          setSearched(true);
        })
        .catch(() => {
          if (cancelled) return;
          setResults([]);
          setSearched(true);
        })
        .finally(() => {
          if (!cancelled) setLoading(false);
        });
    }, 150);
    return () => {
      cancelled = true;
      clearTimeout(timeoutId);
    };
  }, [query, enabled]);

  return { results, loading, searched };
}
