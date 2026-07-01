import { FormEvent, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { getJSON, postJSON } from "../api/client";

type AuthState = {
  authenticated: boolean;
  username?: string;
  mustChangePassword?: boolean;
};

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

  async function submitLogin(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setStatus("");
    try {
      const res = await postJSON<{ ok: boolean; mustChangePassword?: boolean }>("/api/auth/login", {
        username,
        password
      });
      await onAuthChanged();
      if (res.mustChangePassword) {
        setNeedsPasswordChange(true);
        setOldPassword(password);
        setStatus("Password change required before using the app.");
      } else {
        navigate("/read", { replace: true });
      }
    } catch {
      setStatus("Login failed. Check username and password.");
    } finally {
      setBusy(false);
    }
  }

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
      <p>{passwordMode ? "Update your current password to keep using the app." : "Use your local admin credentials to access configuration and daemon controls."}</p>

      {!needsPasswordChange ? (
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
          <p>Your account requires a password update before you can continue.</p>
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
