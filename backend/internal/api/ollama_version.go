package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"kypost-server/backend/internal/adapters/classifier"
	"kypost-server/backend/internal/mailmsg"
	"kypost-server/backend/internal/ollamaupdate"
	"kypost-server/backend/internal/users"
)

// ollamaVersionPollInterval controls how often the installed Ollama version
// is compared against the latest upstream GitHub release. The installed
// version only ever changes when the container image itself is rebuilt, so
// there is no benefit to polling more often than this — it mainly exists to
// pick up a freshly-published upstream release in a timely way.
const ollamaVersionPollInterval = 1 * time.Hour

// ollamaVersionStatus is the last-known result of comparing the installed
// Ollama version against the latest upstream release, cached in memory so
// handleOllamaVersion never has to make a live network call on every page
// load (both to Ollama itself and to the GitHub API, which unauthenticated
// callers should be conservative with).
type ollamaVersionStatus struct {
	installedVersion string
	latestVersion    string
	upgradeAvailable bool
	checkedAt        time.Time
	checkErr         string
}

// SetClassifier attaches the shared classifier HTTP client so the Ollama
// version monitor can query the running instance's own /api/version. Must be
// called before StartOllamaVersionMonitor for the monitor to do anything.
func (s *Server) SetClassifier(c *classifier.HTTPClient) {
	s.classifier = c
}

func (s *Server) getOllamaStatus() ollamaVersionStatus {
	s.ollamaMu.Lock()
	defer s.ollamaMu.Unlock()
	return s.ollamaStatus
}

func (s *Server) setOllamaStatus(status ollamaVersionStatus) {
	s.ollamaMu.Lock()
	defer s.ollamaMu.Unlock()
	s.ollamaStatus = status
}

// StartOllamaVersionMonitor periodically checks the installed Ollama version
// against the latest upstream release and emails the admin the first time an
// update becomes available, so a self-hosted operator knows to rebuild and
// redeploy the container. Safe to call even when SetClassifier was never
// called — each check is then a no-op. Intended to be run in its own
// goroutine (mirrors StartPickupSweeper) and returns when ctx is canceled.
func (s *Server) StartOllamaVersionMonitor(ctx context.Context) {
	s.refreshOllamaVersionStatus(ctx)

	ticker := time.NewTicker(ollamaVersionPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshOllamaVersionStatus(ctx)
		}
	}
}

func (s *Server) refreshOllamaVersionStatus(ctx context.Context) {
	if s.classifier == nil {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	installed, err := s.classifier.Version(checkCtx)
	if err != nil {
		s.logger.Error("ollama installed-version check failed", "error", err.Error())
		s.setOllamaStatus(ollamaVersionStatus{checkErr: "failed to reach ollama"})
		return
	}

	latest, err := ollamaupdate.LatestVersion(checkCtx)
	if err != nil {
		s.logger.Error("ollama upstream-release check failed", "error", err.Error())
		s.setOllamaStatus(ollamaVersionStatus{installedVersion: installed, checkErr: "failed to check for updates"})
		return
	}

	upgradeAvailable := ollamaupdate.IsNewer(latest, installed)
	s.setOllamaStatus(ollamaVersionStatus{
		installedVersion: installed,
		latestVersion:    latest,
		upgradeAvailable: upgradeAvailable,
		checkedAt:        time.Now().UTC(),
	})

	if !upgradeAvailable || s.globalStore == nil {
		return
	}
	notify, err := s.globalStore.SetOllamaUpdateNotified(latest)
	if err != nil {
		s.logger.Error("failed to persist ollama update notification state", "error", err.Error())
		return
	}
	if !notify {
		return
	}

	s.logger.Info("ollama update available", "installed", installed, "latest", latest)
	if err := s.notifyAdminOllamaUpdateAvailable(installed, latest); err != nil {
		s.logger.Error("failed to email admin about ollama update", "error", err.Error())
	}
}

// handleOllamaVersion reports the cached installed/latest Ollama version
// comparison for the Prompt Tuning page's version block. It deliberately
// reads the in-memory cache rather than checking live on every request.
func (s *Server) handleOllamaVersion(w http.ResponseWriter, r *http.Request) {
	status := s.getOllamaStatus()
	if status.installedVersion == "" && status.checkErr == "" {
		http.Error(w, "ollama version check has not completed yet", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installedVersion": status.installedVersion,
		"latestVersion":    status.latestVersion,
		"upgradeAvailable": status.upgradeAvailable,
		"checkedAt":        status.checkedAt.Format(time.RFC3339),
		"error":            status.checkErr,
	})
}

// notifyAdminOllamaUpdateAvailable emails the install's primary admin
// (FirstAdminFrom) that a newer Ollama release is available upstream than
// the one bundled in this container, sent through that admin's own
// configured IMAP/SMTP credentials and addressed to themselves — the same
// self-notification pattern sendPickupNotification and handleMailSend use.
func (s *Server) notifyAdminOllamaUpdateAvailable(installed, latest string) error {
	all, err := s.users.List()
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	admin := users.FirstAdminFrom(all)
	if admin.ID == "" {
		return fmt.Errorf("no active admin to notify")
	}

	payload, exists, err := readIMAPConfigPayload(s.userIMAPConfigPath(admin.ID), s.imapConfigKeyPath)
	if err != nil {
		return fmt.Errorf("read admin imap config: %w", err)
	}
	if !exists {
		return fmt.Errorf("admin has no imap configuration to send through")
	}

	smtpHost, smtpPort, addr, err := resolveSMTPTarget(payload)
	if err != nil {
		return fmt.Errorf("resolve smtp target: %w", err)
	}

	from := sanitizeHeaderValue(payload.Username)
	if from == "" {
		return fmt.Errorf("admin imap username is empty")
	}

	msg := mailmsg.Message{
		From:    from,
		To:      []string{from},
		Subject: "A newer Ollama version is available for your kypost container",
		Body: fmt.Sprintf(
			"Your kypost-server container is currently running Ollama %s. Version %s is now available upstream.\n\n"+
				"This container doesn't update itself — pull and rebuild/redeploy the latest kypost-server image "+
				"to pick up the newer Ollama (and any other patched dependencies) baked into a fresh build.",
			installed, latest,
		),
		Mode: "plain",
	}.Build()

	return smtpDeliver(smtpHost, smtpPort, addr, payload.Username, payload.Password, from, []string{from}, msg)
}
