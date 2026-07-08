type PageTabsProps = {
  totalPages: number;
  currentPage: number;
  onSelect: (page: number) => void;
  classPrefix: string;
  ariaLabel: string;
};

export function PageTabs({ totalPages, currentPage, onSelect, classPrefix, ariaLabel }: PageTabsProps) {
  if (totalPages <= 1) {
    return null;
  }

  return (
    <div className={`${classPrefix}-page-tabs`} role="tablist" aria-label={ariaLabel}>
      {Array.from({ length: totalPages }, (_, idx) => idx + 1).map((page) => (
        <button
          key={page}
          type="button"
          role="tab"
          aria-selected={currentPage === page}
          onClick={() => onSelect(page)}
          className={`${classPrefix}-page-tab ${currentPage === page ? "active" : ""}`}
        >
          {page}
        </button>
      ))}
    </div>
  );
}
