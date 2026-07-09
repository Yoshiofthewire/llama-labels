import { FormEvent, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { getJSON, postJSON } from "../api/client";
import type { AuthState } from "../auth";

type SetupState = {
  configured: boolean;
  setup?: {
    admin_user?: string;
    must_change_password?: boolean;
  };
};

type LoginPageProps = {
  auth: AuthState;
  onAuthChanged: () => Promise<void> | void;
  mode?: "login" | "password";
};

export function LoginPage({ auth, onAuthChanged, mode = "login" }: LoginPageProps) {
  const navigate = useNavigate();
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [needsPasswordChange, setNeedsPasswordChange] = useState(false);
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  const [mfaChallengeId, setMfaChallengeId] = useState("");
  const [mfaCode, setMfaCode] = useState("");
  const [useRecoveryCode, setUseRecoveryCode] = useState(false);
  const [mfaMethods, setMfaMethods] = useState<string[]>([]);
  const [mfaMode, setMfaMode] = useState<"totp" | "push">("totp");
  const passwordMode = mode === "password";

  useEffect(() => {
    if (passwordMode) {
      setNeedsPasswordChange(true);
      setUsername(auth.username ?? username);
      return;
    }
    if (auth.authenticated && !auth.mustChangePassword) {
      navigate("/read", { replace: true });
    }
    if (auth.mustChangePassword) {
      setNeedsPasswordChange(true);
      setUsername(auth.username ?? username);
    }
  }, [auth.authenticated, auth.mustChangePassword, auth.username, navigate, passwordMode, username]);

  useEffect(() => {
    getJSON<SetupState>("/api/setup")
      .then((res) => {
        if (res.setup?.admin_user) {
          setUsername(res.setup.admin_user);
        }
      })
      .catch(() => {
        // Keep defaults if setup endpoint is unavailable.
      });
  }, []);

  function finishSignIn(mustChangePassword: boolean) {
    if (mustChangePassword) {
      setNeedsPasswordChange(true);
      setOldPassword(password);
      setStatus("Password change required before using the app.");
    } else {
      navigate("/read", { replace: true });
    }
  }

  async function submitLogin(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setStatus("");
    try {
      const res = await postJSON<{
        ok?: boolean;
        mustChangePassword?: boolean;
        mfaRequired?: boolean;
        challengeId?: string;
        methods?: string[];
      }>("/api/auth/login", { username, password });
      if (res.mfaRequired && res.challengeId) {
        const methods = res.methods ?? [];
        setMfaChallengeId(res.challengeId);
        setMfaMethods(methods);
        setMfaMode(methods.includes("push") ? "push" : "totp");
        setMfaCode("");
        setUseRecoveryCode(false);
        setStatus("");
        return;
      }
      await onAuthChanged();
      finishSignIn(Boolean(res.mustChangePassword));
    } catch {
      setStatus("Login failed. Check username and password.");
    } finally {
      setBusy(false);
    }
  }

  async function submitMfa(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setStatus("");
    const endpoint = useRecoveryCode ? "/api/auth/mfa/recovery-code" : "/api/auth/mfa/totp";
    try {
      const res = await postJSON<{ ok: boolean; mustChangePassword?: boolean }>(endpoint, {
        challengeId: mfaChallengeId,
        code: mfaCode.trim()
      });
      await onAuthChanged();
      setMfaChallengeId("");
      setMfaCode("");
      finishSignIn(Boolean(res.mustChangePassword));
    } catch {
      setStatus(useRecoveryCode ? "Invalid recovery code." : "Invalid authentication code.");
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    if (!mfaChallengeId || mfaMode !== "push") {
      return;
    }
    let cancelled = false;
    const interval = setInterval(async () => {
      try {
        const res = await postJSON<{ status: string }>("/api/auth/mfa/push/poll", {
          challengeId: mfaChallengeId
        });
        if (cancelled) {
          return;
        }
        if (res.status === "approved") {
          clearInterval(interval);
          const fin = await postJSON<{ ok: boolean; mustChangePassword?: boolean }>(
            "/api/auth/mfa/push/finish",
            { challengeId: mfaChallengeId }
          );
          if (cancelled) {
            return;
          }
          await onAuthChanged();
          setMfaChallengeId("");
          finishSignIn(Boolean(fin.mustChangePassword));
        } else if (res.status === "denied" || res.status === "expired") {
          clearInterval(interval);
          if (cancelled) {
            return;
          }
          setStatus(
            res.status === "denied"
              ? "Sign-in was denied on your device."
              : "The approval request expired."
          );
          if (mfaMethods.includes("totp")) {
            setMfaMode("totp");
          }
        }
      } catch {
        // Transient error; keep polling until the challenge resolves or expires.
      }
    }, 1500);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
    // finishSignIn/onAuthChanged are stable enough for this flow; re-running on
    // challenge/mode/method changes is what matters.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mfaChallengeId, mfaMode, mfaMethods]);

  async function submitPasswordChange(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setStatus("");
    const currentPassword = oldPassword || password;
    if (!currentPassword) {
      setStatus("Enter your current password from initial sign-in.");
      setBusy(false);
      return;
    }
    try {
      await postJSON<{ ok: boolean }>("/api/auth/password", {
        username,
        oldPassword: currentPassword,
        newPassword
      });
      await onAuthChanged();
      setNeedsPasswordChange(false);
      setPassword("");
      setOldPassword("");
      setNewPassword("");
      setStatus("Password updated. You can now continue.");
      navigate("/read", { replace: true });
    } catch (err) {
      const message = err instanceof Error ? err.message : "";
      if (message.includes("401")) {
        setStatus("Password change failed. Sign in first, then try again.");
      } else {
        setStatus("Password change failed. Verify current password.");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel">
      <h2>{passwordMode ? "Change Password" : "Login and Setup"}</h2>
      <p>{passwordMode ? "Update your current password." : "Use your local admin credentials to access configuration and daemon controls."}</p>

      {mfaChallengeId ? (
        mfaMode === "push" ? (
          <div className="auth-form">
            <h3>Two-Factor Authentication</h3>
            <p>Approve this sign-in from a paired device. Waiting for approval…</p>
            {mfaMethods.includes("totp") ? (
              <button
                type="button"
                className="nav-link-button"
                onClick={() => {
                  setMfaMode("totp");
                  setStatus("");
                  setMfaCode("");
                }}
              >
                Use authenticator code instead
              </button>
            ) : null}
          </div>
        ) : (
          <form onSubmit={submitMfa} className="auth-form">
            <h3>Two-Factor Authentication</h3>
            {useRecoveryCode ? (
              <>
                <p>Enter one of your saved recovery codes.</p>
                <label>
                  <div>Recovery Code</div>
                  <input
                    value={mfaCode}
                    onChange={(e) => setMfaCode(e.target.value)}
                    autoComplete="one-time-code"
                    placeholder="xxxx-xxxx-xxxx"
                    autoFocus
                  />
                </label>
              </>
            ) : (
              <>
                <p>Enter the 6-digit code from your authenticator app.</p>
                <label>
                  <div>Authentication Code</div>
                  <input
                    value={mfaCode}
                    onChange={(e) => setMfaCode(e.target.value.replace(/\D/g, "").slice(0, 6))}
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    placeholder="123456"
                    autoFocus
                  />
                </label>
              </>
            )}
            <button type="submit" disabled={busy || mfaCode.trim() === ""}>
              {busy ? "Verifying..." : "Verify"}
            </button>
            <button
              type="button"
              className="nav-link-button"
              onClick={() => {
                setUseRecoveryCode((v) => !v);
                setMfaCode("");
                setStatus("");
              }}
            >
              {useRecoveryCode ? "Use authenticator code instead" : "Use a recovery code instead"}
            </button>
            {mfaMethods.includes("push") ? (
              <button
                type="button"
                className="nav-link-button"
                onClick={() => {
                  setMfaMode("push");
                  setStatus("");
                  setMfaCode("");
                }}
              >
                Approve on a device instead
              </button>
            ) : null}
          </form>
        )
      ) : !needsPasswordChange ? (
        <form onSubmit={submitLogin} className="auth-form">
          <label>
            <div>Username</div>
            <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" />
          </label>
          <label>
            <div>Password</div>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </label>
          <button type="submit" disabled={busy}>
            {busy ? "Signing in..." : "Sign In"}
          </button>
        </form>
      ) : (
        <form onSubmit={submitPasswordChange} className="auth-form">
          <h3>Change Password</h3>
          <p>Enter your current password and choose a new one.</p>
          <label>
            <div>Username</div>
            <input value={username} autoComplete="username" readOnly />
          </label>
          <label>
            <div>Current Password</div>
            <input
              type="password"
              value={oldPassword}
              onChange={(e) => setOldPassword(e.target.value)}
              autoComplete="current-password"
            />
          </label>
          <label>
            <div>New Password</div>
            <input
              type="password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              autoComplete="new-password"
            />
          </label>
          <button type="submit" disabled={busy}>
            {busy ? "Updating..." : "Update Password"}
          </button>
        </form>
      )}

      {status ? <p>{status}</p> : null}
    </section>
  );
}
