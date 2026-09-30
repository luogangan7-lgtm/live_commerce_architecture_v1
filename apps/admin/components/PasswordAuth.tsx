"use client";
// Email + password + emailed-code forms for the admin host (merchant-password-auth-v1 §7.3).
// Mounted by Entry.tsx at /[locale]/ (signin), /[locale]/signup and /[locale]/reset.
// BFF routes called (apps/admin/app/api/auth/password/step1.ts) and the Go endpoints behind them:
//   POST /api/auth/password/{signup,login,reset} → POST /v1/identity/password/{signup,login,reset}
//   POST /api/auth/password/verify              → POST /v1/identity/password/complete
//   (internal/identityhttp/password.go). The OIDC button posts /api/auth/login → /v1/identity/login/start.
// The password and code live only in component state: never storage, never a URL, never a log line.
// Resend re-posts step 1 with the values held here; the 60 s cooldown is UX only, the server throttles.
// Client checks are convenience: Go owns the password policy, code validity and every throttle.
import { useEffect, useRef, useState, type FormEvent } from "react";
import type { Locale } from "@live-commerce/i18n";
import { entryCopy, passwordCopy } from "@/lib/entry-copy";
import {
  RESEND_COOLDOWN_SECONDS,
  fillCopy,
  maskEmail,
  throttleMinutes,
} from "@/lib/entry-state";

export type PasswordMode = "signin" | "signup" | "reset";
const purposeOf = {
  signin: "login",
  signup: "signup",
  reset: "reset",
} as const;
const redirectPattern = /^\/(?:zh-CN|zh-TW|en)\/$/;

type Reply = {
  status: number;
  json: {
    code?: unknown;
    redirect?: unknown;
    expires_at?: unknown;
    details?: { reason?: unknown };
  } | null;
  retryAfter: number;
};

async function post(path: string, body: unknown): Promise<Reply> {
  try {
    const response = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      credentials: "same-origin",
      cache: "no-store",
    });
    let json: Reply["json"] = null;
    try {
      json = await response.json();
    } catch {
      json = null;
    }
    return {
      status: response.status,
      json,
      retryAfter: Number(response.headers.get("retry-after") ?? "0"),
    };
  } catch {
    return { status: 0, json: null, retryAfter: 0 };
  }
}

export function PasswordAuth({
  locale,
  mode,
  oidc,
  notice,
}: {
  locale: Locale;
  mode: PasswordMode;
  oidc: boolean;
  notice: string;
}) {
  const c = passwordCopy[locale];
  const entry = entryCopy[locale];
  const [step, setStep] = useState<"form" | "code">("form");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const [resendAt, setResendAt] = useState(0);
  const busyRef = useRef(false);
  const codeInput = useRef<HTMLInputElement>(null);
  const purpose = purposeOf[mode];
  const secondsLeft = Math.max(0, Math.ceil((resendAt - now) / 1000));

  useEffect(() => {
    if (step !== "code") return;
    codeInput.current?.focus();
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [step]);

  // Maps a BFF answer to localized text. invalid_request on the code step means the challenge
  // cookie is gone or malformed; on the form step it is a plain field error.
  function errorText(reply: Reply, where: "form" | "code") {
    const code = typeof reply.json?.code === "string" ? reply.json.code : "";
    const e = c.errors;
    if (reply.status === 0) return e.retry_later;
    switch (code) {
      case "throttled":
        return fillCopy(e.throttled, {
          minutes: throttleMinutes(reply.retryAfter),
        });
      case "mail_unavailable":
        // §2 round-3 P2: a login mail-budget 503 consumed an attempt; tell the user to wait.
        return mode === "signin" && where === "form"
          ? e.mail_unavailable_login
          : e.mail_unavailable;
      case "password_policy": {
        const reason = reply.json?.details?.reason;
        return typeof reason === "string" && reason in c.policy
          ? c.policy[reason as keyof typeof c.policy]
          : e.password_policy;
      }
      case "invalid_request":
        return where === "code" ? e.challenge_lost : e.invalid_request;
      case "invalid_credentials":
      case "invalid_code":
      case "invalid_email":
      case "account_exists":
      case "busy":
      case "retry_later":
        return e[code];
      default:
        return e.unknown;
    }
  }

  async function sendStep1(resend: boolean) {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError("");
    const body =
      mode === "reset"
        ? { email: email.trim(), locale }
        : { email: email.trim(), password, locale };
    const reply = await post(`/api/auth/password/${purpose}`, body);
    busyRef.current = false;
    setBusy(false);
    if (reply.status !== 202) {
      setError(errorText(reply, "form"));
      return;
    }
    setCode("");
    setNewPassword("");
    setNow(Date.now());
    setResendAt(Date.now() + RESEND_COOLDOWN_SECONDS * 1000);
    setStep("code");
    if (resend) codeInput.current?.focus();
  }

  async function sendVerify() {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError("");
    const body =
      mode === "reset" ? { code, new_password: newPassword } : { code };
    const reply = await post("/api/auth/password/verify", body);
    if (reply.status === 200) {
      const target = reply.json?.redirect;
      if (typeof target === "string" && redirectPattern.test(target)) {
        // busy stays true: the page is navigating away.
        window.location.assign(target);
        return;
      }
    }
    busyRef.current = false;
    setBusy(false);
    setError(errorText(reply, "code"));
    // 409 (account_exists) drops the challenge server-side, and a lost challenge cannot be retried.
    if (
      reply.status === 409 ||
      (reply.status === 422 && reply.json?.code === "invalid_request")
    )
      setStep("form");
  }

  function onForm(event: FormEvent) {
    event.preventDefault();
    void sendStep1(false);
  }

  function onCode(event: FormEvent) {
    event.preventDefault();
    void sendVerify();
  }

  const title = {
    signin: c.signinTitle,
    signup: c.signupTitle,
    reset: c.resetTitle,
  }[mode];
  const body = {
    signin: c.signinBody,
    signup: c.signupBody,
    reset: c.resetBody,
  }[mode];
  const submit = {
    signin: c.submitSignin,
    signup: c.submitSignup,
    reset: c.submitReset,
  }[mode];
  const verify = {
    signin: c.verifySignin,
    signup: c.verifySignup,
    reset: c.verifyReset,
  }[mode];
  const left = { textAlign: "left" } as const;
  const link = { color: "var(--focus)" } as const;

  return (
    <section className="entry-auth-panel" aria-labelledby="password-auth-title">
      <h1 id="password-auth-title">{step === "code" ? c.codeTitle : title}</h1>
      {step === "form" ? (
        <p>{body}</p>
      ) : (
        <p>{fillCopy(c.codeSent, { email: maskEmail(email.trim()) })}</p>
      )}
      {notice && step === "form" && (
        <p className="entry-message error" role="alert">
          {notice}
        </p>
      )}
      {error && (
        <p className="entry-message error" role="alert">
          {error}
        </p>
      )}

      {step === "form" ? (
        <form className="entry-form" method="post" style={left} onSubmit={onForm}>
          <label>
            <span>{c.email}</span>
            <input
              name="email"
              type="email"
              autoComplete="email"
              maxLength={254}
              value={email}
              onChange={(event) => setEmail(event.target.value)}
              disabled={busy}
              required
            />
          </label>
          {mode !== "reset" && (
            <label>
              <span>{c.password}</span>
              <input
                name="password"
                type="password"
                autoComplete={
                  mode === "signin" ? "current-password" : "new-password"
                }
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                disabled={busy}
                required
              />
              {mode === "signup" && <small>{c.policyHint}</small>}
            </label>
          )}
          <div className="entry-actions end">
            <button className="entry-primary" type="submit" disabled={busy}>
              {busy ? c.busy : submit}
            </button>
          </div>
        </form>
      ) : (
        <form className="entry-form" method="post" style={left} onSubmit={onCode}>
          <label>
            <span>{c.code}</span>
            <input
              ref={codeInput}
              name="code"
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              maxLength={6}
              pattern="[0-9]{6}"
              value={code}
              onChange={(event) =>
                setCode(event.target.value.replace(/\D/g, "").slice(0, 6))
              }
              disabled={busy}
              required
            />
            <small>
              {c.codeExpiry} {c.spamHint}
            </small>
          </label>
          {mode === "reset" && (
            <label>
              <span>{c.newPassword}</span>
              <input
                name="new_password"
                type="password"
                autoComplete="new-password"
                value={newPassword}
                onChange={(event) => setNewPassword(event.target.value)}
                disabled={busy}
                required
              />
              <small>{c.policyHint}</small>
            </label>
          )}
          <div className="entry-actions">
            <button
              type="button"
              onClick={() => void sendStep1(true)}
              disabled={busy || secondsLeft > 0}
            >
              {secondsLeft > 0
                ? fillCopy(c.resendIn, { seconds: secondsLeft })
                : c.resend}
            </button>
            <button
              className="entry-primary"
              type="submit"
              disabled={busy || code.length !== 6}
            >
              {busy ? c.busy : verify}
            </button>
          </div>
          <button
            type="button"
            onClick={() => {
              setStep("form");
              setError("");
            }}
            disabled={busy}
          >
            {c.changeEmail}
          </button>
        </form>
      )}

      {step === "form" && (
        <p className="entry-scope">
          {mode === "signin" && (
            <>
              <a href={`/${locale}/reset`} style={link}>
                {c.toReset}
              </a>{" "}
              ·{" "}
              <a href={`/${locale}/signup`} style={link}>
                {c.toSignup}
              </a>
            </>
          )}
          {mode === "signup" && (
            <a href={`/${locale}/`} style={link}>
              {c.toSignin}
            </a>
          )}
          {mode === "reset" && (
            <a href={`/${locale}/`} style={link}>
              {c.toSignin}
            </a>
          )}
        </p>
      )}
      {oidc && mode === "signin" && step === "form" && (
        <form method="post" action="/api/auth/login">
          <input type="hidden" name="locale" value={locale} />
          <p className="entry-scope">{c.or}</p>
          <button className="entry-check" type="submit">
            {entry.signIn}
          </button>
        </form>
      )}
    </section>
  );
}
