import { useEffect, useState } from "react";
import { getJSON, putJSON } from "../api/client";
import { usePagination } from "../hooks/usePagination";
import { PageTabs } from "../components/PageTabs";

type TuningResponse = {
  content: string;
  path?: string;
};

type TuningSaveResponse = {
  ok: boolean;
  path?: string;
  restartOk?: boolean;
  restartError?: string;
};

type Decision = {
  messageId: string;
  sender: string;
  sentTo?: string;
  subject: string;
  label: string;
  status: string;
  detail: string;
  atUtc: string;
};

export function TuningPage() {
  const [tuningText, setTuningText] = useState("");
  const [tuningStatus, setTuningStatus] = useState("");
  const [activeTab, setActiveTab] = useState<"prompt" | "decisions">("prompt");
  const [decisions, setDecisions] = useState<Decision[]>([]);
  const [decisionsLoaded, setDecisionsLoaded] = useState(false);
  const [decisionsError, setDecisionsError] = useState("");

  async function saveTuning() {
    try {
      const result = await putJSON<TuningSaveResponse>("/api/tuning", { content: tuningText });
      setTuningStatus(
        result.restartOk === false
          ? `Tuning saved, but Llama restart needs attention: ${result.restartError ?? "unknown restart failure"}`
          : "TUNING.md saved and Llama restarted."
      );
    } catch {
      setTuningStatus("Failed to save tuning file.");
    }
  }

  useEffect(() => {
    getJSON<TuningResponse>("/api/tuning")
      .then((tuningData) => {
        setTuningText(tuningData.content ?? "");
      })
      .catch(() => setTuningStatus("Failed to load tuning settings."));
  }, []);

  useEffect(() => {
    if (activeTab === "decisions" && !decisionsLoaded) {
      getJSON<Decision[]>("/api/decisions?limit=500")
        .then((data) => {
          setDecisions(data ?? []);
          setDecisionsLoaded(true);
          setDecisionsError("");
        })
        .catch(() => {
          setDecisionsError("Failed to load decisions.");
          setDecisionsLoaded(true);
        });
    }
  }, [activeTab, decisionsLoaded]);

  const { currentPage, setCurrentPage, totalPages, pageItems: pageDecisions } = usePagination(decisions, 20);

  return (
    <section className="panel">
      <div className="config-tabs" role="tablist" aria-label="Tuning sections">
        <button type="button" role="tab" aria-selected={activeTab === "prompt"} className={`config-tab${activeTab === "prompt" ? " active" : ""}`} onClick={() => setActiveTab("prompt")}>TUNING.md</button>
        <button type="button" role="tab" aria-selected={activeTab === "decisions"} className={`config-tab${activeTab === "decisions" ? " active" : ""}`} onClick={() => setActiveTab("decisions")}>Decisions</button>
      </div>

      {activeTab === "prompt" ? (
        <div>
          <h2>TUNING.md</h2>
          <p>Edit and save the markdown instructions used for message labeling.</p>

          <label>
            <div>TUNING.md</div>
            <textarea rows={18} value={tuningText} onChange={(e) => setTuningText(e.target.value)} style={{ width: "100%" }} />
          </label>

          <button type="button" onClick={saveTuning}>Save TUNING.md</button>
          {tuningStatus ? <p>{tuningStatus}</p> : null}
        </div>
      ) : null}

      {activeTab === "decisions" ? (
        <div>
          <h2>Classification Decisions</h2>
          <p>Audit log of AI classification decisions for message labeling.</p>

          {decisionsError ? (
            <p className="notice notice-error">{decisionsError}</p>
          ) : decisionsLoaded ? (
            <>
              {decisions.length === 0 ? (
                <p className="config-muted">No classification decisions recorded yet.</p>
              ) : (
                <>
                  <div style={{ overflowX: "auto" }}>
                    <table style={{ width: "100%", borderCollapse: "collapse" }}>
                      <thead>
                        <tr style={{ borderBottom: "1px solid var(--border-color)" }}>
                          <th style={{ textAlign: "left", padding: "8px" }}>Time</th>
                          <th style={{ textAlign: "left", padding: "8px" }}>Sender</th>
                          <th style={{ textAlign: "left", padding: "8px" }}>Subject</th>
                          <th style={{ textAlign: "left", padding: "8px" }}>Label</th>
                          <th style={{ textAlign: "left", padding: "8px" }}>Status</th>
                          <th style={{ textAlign: "left", padding: "8px" }}>Detail</th>
                        </tr>
                      </thead>
                      <tbody>
                        {pageDecisions.map((decision) => (
                          <tr key={`${decision.messageId}-${decision.atUtc}`} style={{ borderBottom: "1px solid var(--border-color)" }}>
                            <td style={{ padding: "8px", fontSize: "0.9em" }}>{new Date(decision.atUtc).toLocaleString()}</td>
                            <td style={{ padding: "8px", fontSize: "0.9em" }}>{decision.sender}</td>
                            <td style={{ padding: "8px", fontSize: "0.9em" }}>{decision.subject}</td>
                            <td style={{ padding: "8px", fontSize: "0.9em" }}>{decision.label}</td>
                            <td style={{ padding: "8px", fontSize: "0.9em" }}>{decision.status}</td>
                            <td style={{ padding: "8px", fontSize: "0.85em" }}>{decision.detail}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                  <PageTabs currentPage={currentPage} totalPages={totalPages} onPageChange={setCurrentPage} />
                </>
              )}
            </>
          ) : (
            <p className="config-muted">Loading decisions...</p>
          )}
        </div>
      ) : null}
    </section>
  );
}
