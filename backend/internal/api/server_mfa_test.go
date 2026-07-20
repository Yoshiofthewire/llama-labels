package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kypost-server/backend/internal/users"
)

// totpCodeForTest independently computes a 6-digit TOTP for a base32 secret at
// time t, mirroring RFC 6238. Used to drive the real handlers and to
// cross-check the production totp package.
func totpCodeForTest(t *testing.T, base32Secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(base32Secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	counter := uint64(at.Unix() / 30)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset])&0x7f)<<24 |
		(uint32(sum[offset+1])&0xff)<<16 |
		(uint32(sum[offset+2])&0xff)<<8 |
		(uint32(sum[offset+3]) & 0xff)
	return fmt.Sprintf("%06d", bin%1_000_000)
}

// enrollTOTP runs setup + confirm for userID and returns the base32 secret and
// the recovery codes.
func enrollTOTP(t *testing.T, srv *Server, userID string) (secret string, recoveryCodes []string) {
	t.Helper()

	setupReq := httptest.NewRequest(http.MethodPost, "/api/mfa/totp/setup", nil)
	authRequestAs(srv, setupReq, userID)
	setupRec := httptest.NewRecorder()
	srv.withAuth(srv.handleMFASetup)(setupRec, setupReq)
	if setupRec.Code != http.StatusOK {
		t.Fatalf("setup: status=%d body=%s", setupRec.Code, setupRec.Body.String())
	}
	var setupResp struct {
		Secret     string `json:"secret"`
		OtpauthURI string `json:"otpauthUri"`
	}
	if err := json.Unmarshal(setupRec.Body.Bytes(), &setupResp); err != nil {
		t.Fatalf("unmarshal setup: %v", err)
	}
	if setupResp.Secret == "" || setupResp.OtpauthURI == "" {
		t.Fatalf("setup response missing fields: %s", setupRec.Body.String())
	}

	code := totpCodeForTest(t, setupResp.Secret, time.Now())
	confirmRec := doJSONAuth(srv, srv.withAuth(srv.handleMFAConfirm), http.MethodPost,
		"/api/mfa/totp/confirm", map[string]string{"code": code}, userID)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm: status=%d body=%s", confirmRec.Code, confirmRec.Body.String())
	}
	var confirmResp struct {
		Ok            bool     `json:"ok"`
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	if err := json.Unmarshal(confirmRec.Body.Bytes(), &confirmResp); err != nil {
		t.Fatalf("unmarshal confirm: %v", err)
	}
	if !confirmResp.Ok || len(confirmResp.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("confirm response unexpected: %s", confirmRec.Body.String())
	}
	return setupResp.Secret, confirmResp.RecoveryCodes
}

// doJSONAuth is doJSON plus an injected session for userID, including the
// matching X-CSRF-Token header a real browser client would send (see
// authRequestAs/csrfCheckOK) — without it every mutating request would be
// rejected with 403 regardless of the test's own assertions.
func doJSONAuth(srv *Server, handler http.HandlerFunc, method, path string, payload any, userID string) *httptest.ResponseRecorder {
	token := "session-token-" + userID
	csrfToken := "csrf-token-" + userID
	srv.mu.Lock()
	srv.sessions[token] = Session{UserID: userID, ExpiresAt: time.Now().Add(24 * time.Hour), CSRFToken: csrfToken}
	srv.mu.Unlock()
	// Model an onboarded session; the must-change gate (withAuth) is exercised
	// by its own dedicated test.
	_, _ = srv.users.ClearMustChangePassword(userID)

	var body *bytes.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
	req.Header.Set("X-CSRF-Token", csrfToken)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestTOTPEnrollmentAndLoginFlow(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("erin", "pw-erin", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	secret, _ := enrollTOTP(t, srv, u.ID)

	// Password login now returns an MFA challenge, NOT a session cookie.
	loginRec := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "erin", "password": "pw-erin"})
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login: status=%d body=%s", loginRec.Code, loginRec.Body.String())
	}
	if cookies := loginRec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("expected no session cookie on MFA-required login, got %+v", cookies)
	}
	var login struct {
		MFARequired bool     `json:"mfaRequired"`
		ChallengeID string   `json:"challengeId"`
		Methods     []string `json:"methods"`
	}
	if err := json.Unmarshal(loginRec.Body.Bytes(), &login); err != nil {
		t.Fatalf("unmarshal login: %v", err)
	}
	if !login.MFARequired || login.ChallengeID == "" {
		t.Fatalf("expected mfaRequired challenge, got %s", loginRec.Body.String())
	}

	// Second factor mints the real session.
	code := totpCodeForTest(t, secret, time.Now())
	totpRec := doJSON(srv, srv.handleMFATOTP, http.MethodPost, "/api/auth/mfa/totp",
		map[string]string{"challengeId": login.ChallengeID, "code": code})
	if totpRec.Code != http.StatusOK {
		t.Fatalf("mfa/totp: status=%d body=%s", totpRec.Code, totpRec.Body.String())
	}
	cookies := totpRec.Result().Cookies()
	if findCookie(cookies, "kypost_session") == nil {
		t.Fatalf("expected a kypost_session cookie after second factor, got %+v", cookies)
	}

	// Replay: reusing the same challenge is rejected.
	replayRec := doJSON(srv, srv.handleMFATOTP, http.MethodPost, "/api/auth/mfa/totp",
		map[string]string{"challengeId": login.ChallengeID, "code": totpCodeForTest(t, secret, time.Now())})
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("replay: status=%d, want 401", replayRec.Code)
	}
}

func TestTOTPAttemptLockout(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("frank", "pw-frank", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	secret, _ := enrollTOTP(t, srv, u.ID)

	loginRec := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "frank", "password": "pw-frank"})
	var login struct {
		ChallengeID string `json:"challengeId"`
	}
	_ = json.Unmarshal(loginRec.Body.Bytes(), &login)

	// 5 wrong codes are tolerated (401 invalid), the 6th locks out.
	for i := 0; i < 5; i++ {
		rec := doJSON(srv, srv.handleMFATOTP, http.MethodPost, "/api/auth/mfa/totp",
			map[string]string{"challengeId": login.ChallengeID, "code": "000000"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d: status=%d, want 401", i+1, rec.Code)
		}
	}
	rec := doJSON(srv, srv.handleMFATOTP, http.MethodPost, "/api/auth/mfa/totp",
		map[string]string{"challengeId": login.ChallengeID, "code": "000000"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("lockout attempt: status=%d, want 401", rec.Code)
	}
	// Challenge is now gone: even the genuinely correct current code cannot
	// revive it. Using a real valid code here (rather than another wrong one)
	// is what actually proves the lockout is enforced by challenge deletion,
	// not merely that a wrong code keeps failing.
	correctCode := totpCodeForTest(t, secret, time.Now())
	after := doJSON(srv, srv.handleMFATOTP, http.MethodPost, "/api/auth/mfa/totp",
		map[string]string{"challengeId": login.ChallengeID, "code": correctCode})
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("post-lockout with correct code: status=%d, want 401", after.Code)
	}
	// And a second correct-code attempt to be doubly sure it isn't a one-shot fluke.
	after2 := doJSON(srv, srv.handleMFATOTP, http.MethodPost, "/api/auth/mfa/totp",
		map[string]string{"challengeId": login.ChallengeID, "code": correctCode})
	if after2.Code != http.StatusUnauthorized {
		t.Fatalf("post-lockout retry with correct code: status=%d, want 401", after2.Code)
	}
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("grace", "pw-grace", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, recoveryCodes := enrollTOTP(t, srv, u.ID)
	code := recoveryCodes[0]

	// First challenge: recovery code works and mints a session.
	login1 := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "grace", "password": "pw-grace"})
	var l1 struct {
		ChallengeID string `json:"challengeId"`
	}
	_ = json.Unmarshal(login1.Body.Bytes(), &l1)
	rec1 := doJSON(srv, srv.handleMFARecoveryCode, http.MethodPost, "/api/auth/mfa/recovery-code",
		map[string]string{"challengeId": l1.ChallengeID, "code": code})
	if rec1.Code != http.StatusOK {
		t.Fatalf("recovery use 1: status=%d body=%s", rec1.Code, rec1.Body.String())
	}

	// Second challenge: the same code is now consumed and rejected.
	login2 := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "grace", "password": "pw-grace"})
	var l2 struct {
		ChallengeID string `json:"challengeId"`
	}
	_ = json.Unmarshal(login2.Body.Bytes(), &l2)
	rec2 := doJSON(srv, srv.handleMFARecoveryCode, http.MethodPost, "/api/auth/mfa/recovery-code",
		map[string]string{"challengeId": l2.ChallengeID, "code": code})
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("recovery reuse: status=%d, want 401", rec2.Code)
	}
}

func TestMFAStatusAndDisable(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create("heidi", "pw-heidi", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	enrollTOTP(t, srv, u.ID)

	statusRec := doJSONAuth(srv, srv.withAuth(srv.handleMFAStatus), http.MethodGet, "/api/mfa/status", nil, u.ID)
	var status struct {
		TOTPEnabled            bool `json:"totpEnabled"`
		RecoveryCodesRemaining int  `json:"recoveryCodesRemaining"`
	}
	_ = json.Unmarshal(statusRec.Body.Bytes(), &status)
	if !status.TOTPEnabled || status.RecoveryCodesRemaining != recoveryCodeCount {
		t.Fatalf("status = %+v", status)
	}

	// Disable requires the correct password.
	bad := doJSONAuth(srv, srv.withAuth(srv.handleMFADisable), http.MethodPost, "/api/mfa/totp/disable",
		map[string]string{"password": "wrong"}, u.ID)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("disable wrong pw: status=%d, want 401", bad.Code)
	}
	good := doJSONAuth(srv, srv.withAuth(srv.handleMFADisable), http.MethodPost, "/api/mfa/totp/disable",
		map[string]string{"password": "pw-heidi"}, u.ID)
	if good.Code != http.StatusOK {
		t.Fatalf("disable: status=%d body=%s", good.Code, good.Body.String())
	}
	got, _ := srv.users.Get(u.ID)
	if got.TOTPEnabled {
		t.Fatalf("expected TOTP disabled")
	}
}
