// Package ollamaupdate checks whether the Ollama release bundled in this
// container is behind the latest one published upstream, so operators can be
// told when a rebuild/redeploy would pick up a newer, better-patched Ollama.
package ollamaupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// releasesURLOverrideForTest lets tests point LatestVersion at a local
// httptest server instead of the real GitHub API; production code never
// changes it.
var releasesURLOverrideForTest = ""

// SetReleasesURLForTest overrides the GitHub releases URL LatestVersion
// queries, for use by other packages' tests (e.g. api's monitor test). Pass
// "" to restore the real GitHub API URL.
func SetReleasesURLForTest(url string) {
	releasesURLOverrideForTest = url
}

const releasesURL = "https://api.github.com/repos/ollama/ollama/releases/latest"

// LatestVersion queries GitHub for the most recently published Ollama
// release and returns its version with any leading "v" stripped — GitHub
// tags this repo as e.g. "v0.32.1", while Ollama's own /api/version reports
// "0.32.1" with no prefix, so both sides compare on the same form.
func LatestVersion(ctx context.Context) (string, error) {
	url := releasesURL
	if releasesURLOverrideForTest != "" {
		url = releasesURLOverrideForTest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// GitHub's REST API rejects unauthenticated requests with no User-Agent.
	req.Header.Set("User-Agent", "kypost-server-ollama-update-check")
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("github releases lookup failed: status %d", resp.StatusCode)
	}

	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	version := strings.TrimPrefix(strings.TrimSpace(out.TagName), "v")
	if version == "" {
		return "", fmt.Errorf("github release response missing tag_name")
	}
	return version, nil
}

// IsNewer reports whether latest is a strictly newer dotted-numeric version
// than installed. Any component that doesn't parse as a number makes the
// comparison fail safe (false) rather than risk a false "update available"
// from an unexpected version format on either side.
func IsNewer(latest, installed string) bool {
	l, lok := parseVersion(latest)
	i, iok := parseVersion(installed)
	if !lok || !iok {
		return false
	}
	for idx := 0; idx < len(l) || idx < len(i); idx++ {
		var lv, iv int
		if idx < len(l) {
			lv = l[idx]
		}
		if idx < len(i) {
			iv = i[idx]
		}
		if lv != iv {
			return lv > iv
		}
	}
	return false
}

func parseVersion(v string) ([]int, bool) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return nil, false
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
