"use client";
// Signed-out / onboarding shell for /[locale]/ and (signed-out only) /[locale]/signup, /[locale]/reset.
// BFF routes called: POST /api/auth/login (OIDC) → /v1/identity/login/start, POST /api/auth/logout →
// /v1/identity/logout, POST /api/onboarding/initial-store → /v1/identity/initial-store
// (internal/identityhttp). With passwordMode set, the signed-out panel is PasswordAuth.tsx, whose
// /api/auth/password/* routes are documented there.

import { useEffect, useRef, useState, type FormEvent } from "react";
import { localeNames, locales, type Locale } from "@live-commerce/i18n";
import { entryCopy } from "@/lib/entry-copy";
import {
  ENTRY_TTL_MS,
  emptyEntryDraft,
  encodeEntryJournal,
  parseEntryJournal,
  type EntryDraft,
  type EntryPending,
} from "@/lib/entry-state";
import { Icon } from "./Icon";
import { signalLogout } from "@/lib/session-events";
import { PasswordAuth, type PasswordMode } from "./PasswordAuth";

type EntryStatus = "disabled" | "signed-out" | "onboarding" | "unavailable";

function csrfToken() {
  const values = document.cookie
    .split(";")
    .map((part) => part.trim())
    .filter((part) => part.startsWith("__Host-commerce_csrf="))
    .map((part) => part.slice("__Host-commerce_csrf=".length));
  return values.length === 1 && /^[A-Za-z0-9_-]{43}$/.test(values[0])
    ? values[0]
    : "";
}

async function sessionStorageKey(csrf: string) {
  if (!csrf) throw new Error("missing CSRF session boundary");
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(csrf),
  );
  const namespace = [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
  return `commerce-onboarding:${namespace}`;
}

export function Entry({
  locale,
  status,
  authResult,
  onboardingEnabled,
  currencies,
  passwordMode,
  oidc = true,
  path = "",
}: {
  locale: Locale;
  status: EntryStatus;
  authResult: string;
  onboardingEnabled: boolean;
  currencies: string[];
  // U6: set only when COMMERCE_PASSWORD_LOGIN_ENABLED=1; replaces the OIDC signed-out panel.
  passwordMode?: PasswordMode;
  // False when no COMMERCE_OIDC_ISSUER is configured: the OIDC button is not rendered (U4).
  oidc?: boolean;
  // Page path below /<locale>/ kept when switching language ("" | "signup" | "reset").
  path?: string;
}) {
  const c = entryCopy[locale];
  // Native locale data labels currencies; the selected ISO value never changes.
  const currencyNames = new Intl.DisplayNames([locale], { type: "currency" });
  const [step, setStep] = useState<1 | 2 | 3>(1);
  const [draft, setDraft] = useState<EntryDraft>(() =>
    emptyEntryDraft(currencies[0] ?? ""),
  );
  const [pending, setPending] = useState<EntryPending | null>(null);
  const [ready, setReady] = useState(status !== "onboarding");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);
  const storageKey = useRef("");
  const busyRef = useRef(false);

  function persist(nextDraft = draft, nextStep = step, nextPending = pending) {
    if (!storageKey.current) throw new Error("entry journal unavailable");
    const encoded = encodeEntryJournal({
      version: 1,
      expiresAt: Date.now() + ENTRY_TTL_MS,
      step: nextStep,
      draft: nextDraft,
      pending: nextPending,
    });
    sessionStorage.setItem(storageKey.current, encoded);
    if (sessionStorage.getItem(storageKey.current) !== encoded)
      throw new Error("entry journal not durable");
  }

  function clearJournal() {
    if (storageKey.current) sessionStorage.removeItem(storageKey.current);
  }

  useEffect(() => {
    if (status !== "onboarding" || !onboardingEnabled) return;
    let active = true;
    const csrf = csrfToken();
    void sessionStorageKey(csrf)
      .then((key) => {
        if (!active) return;
        storageKey.current = key;
        const raw = sessionStorage.getItem(key);
        const saved = parseEntryJournal(raw, currencies);
        if (raw && !saved) sessionStorage.removeItem(key);
        if (saved) {
          setDraft(saved.draft);
          setStep(saved.step);
          setPending(saved.pending);
          if (saved.pending) setNotice(c.unknown);
        }
        setReady(true);
      })
      .catch(() => {
        if (!active) return;
        setError(c.storageUnavailable);
        setReady(false);
      });
    return () => {
      active = false;
    };
  }, [c.storageUnavailable, c.unknown, currencies, onboardingEnabled, status]);

  useEffect(() => {
    if (!ready || status !== "onboarding" || !onboardingEnabled || success)
      return;
    try {
      persist();
    } catch {
      setError(c.storageUnavailable);
      setReady(false);
    }
  }, [draft, step, pending, ready, status, onboardingEnabled, success]);

  const locked = busy || pending !== null || !ready || success;

  function update(name: keyof EntryDraft, value: string) {
    if (locked) return;
    setDraft((current) => ({ ...current, [name]: value }));
    setError("");
  }

  function required(value: string) {
    return value.trim().length > 0 && [...value.trim()].length <= 120;
  }

  function advance(event: FormEvent) {
    event.preventDefault();
    const valid =
      step === 1
        ? required(draft.tenant_name)
        : required(draft.store_name) && currencies.includes(draft.currency);
    if (!valid) {
      setError(c.invalid);
      return;
    }
    setError("");
    setStep(step === 1 ? 2 : 3);
  }

  async function expireSession(csrf: string) {
    clearJournal();
    signalLogout();
    try {
      await fetch("/api/auth/logout", {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": csrf,
        },
        body: "{}",
      });
    } finally {
      signalLogout();
      window.location.replace(`/${locale}/?auth=expired`);
    }
  }

  async function send(command: EntryPending) {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const csrf = csrfToken();
      const currentKey = await sessionStorageKey(csrf);
      if (currentKey !== storageKey.current) {
        clearJournal();
        window.location.reload();
        return;
      }
      persist(draft, 3, command);
      setPending(command);
      const response = await fetch("/api/onboarding/initial-store", {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": command.key,
          "X-CSRF-Token": csrf,
        },
        body: command.body,
        signal: AbortSignal.timeout(8000),
      });
      if (response.status === 401) {
        await expireSession(csrf);
        return;
      }
      let body: unknown;
      try {
        body = await response.json();
      } catch {
        setNotice(c.unknown);
        return;
      }
      if (response.status >= 500) {
        setNotice(c.unknown);
        return;
      }
      if (response.status === 409) {
        setNotice(c.conflict);
        return;
      }
      if (!response.ok) {
        clearJournal();
        setPending(null);
        setError(c.failed);
        return;
      }
      if (
        !body ||
        typeof body !== "object" ||
        !["tenant_id", "store_id", "warehouse_id"].every((key) =>
          /^[0-9a-f-]{36}$/.test(
            String((body as Record<string, unknown>)[key] ?? ""),
          ),
        )
      ) {
        setNotice(c.unknown);
        return;
      }
      clearJournal();
      setPending(null);
      setSuccess(true);
      setNotice(c.success);
    } catch {
      setNotice(c.unknown);
    } finally {
      setBusy(false);
      busyRef.current = false;
    }
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (busyRef.current || pending || !required(draft.warehouse_name)) {
      if (!pending) setError(c.invalid);
      return;
    }
    const body = JSON.stringify({
      tenant_name: draft.tenant_name.trim(),
      store_name: draft.store_name.trim(),
      warehouse_name: draft.warehouse_name.trim(),
      currency: draft.currency,
    });
    void send({ key: crypto.randomUUID(), body });
  }

  function switchLocale(next: string) {
    if (!locales.includes(next as Locale)) return;
    if (status === "onboarding" && ready && !success) {
      try {
        persist();
      } catch {
        setError(c.storageUnavailable);
        return;
      }
    }
    window.location.assign(`/${next}/${path}`);
  }

  async function signOut() {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    signalLogout();
    try {
      const csrf = csrfToken();
      if (
        storageKey.current &&
        (await sessionStorageKey(csrf)) !== storageKey.current
      ) {
        clearJournal();
        window.location.reload();
        return;
      }
      const response = await fetch("/api/auth/logout", {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": csrf,
        },
        body: "{}",
      });
      if (response.status === 204 || response.status === 401) {
        signalLogout();
        clearJournal();
        window.location.replace(`/${locale}/`);
        return;
      }
      setError(c.failed);
    } catch {
      setError(c.failed);
    } finally {
      setBusy(false);
      busyRef.current = false;
    }
  }

  const message =
    authResult === "failed"
      ? c.authFailed
      : authResult === "expired"
        ? c.authExpired
        : "";

  return (
    <div className="entry-root">
      <header className="entry-topbar">
        <div className="entry-brand">{c.product}</div>
        <div className="entry-manage">
          <Icon name="inventory" size={20} />
          {c.manage}
        </div>
        <div className="entry-account-tools">
          <label className="entry-language">
            <span className="sr-only">{c.language}</span>
            <select
              aria-label={c.language}
              value={locale}
              onChange={(event) => switchLocale(event.target.value)}
              disabled={busy}
            >
              {locales.map((item) => (
                <option key={item} value={item}>
                  {localeNames[item]}
                </option>
              ))}
            </select>
          </label>
          <span className="entry-avatar" aria-hidden="true">
            M
          </span>
          <span className="entry-account-label">{c.account}</span>
          {status === "onboarding" && (
            <button
              className="entry-signout"
              type="button"
              onClick={() => void signOut()}
              disabled={busy || !!pending}
            >
              {c.signOut}
            </button>
          )}
        </div>
      </header>

      <main className="entry-main">
        {status === "signed-out" && passwordMode ? (
          <PasswordAuth locale={locale} mode={passwordMode} oidc={oidc} notice={message} />
        ) : status === "signed-out" ||
        status === "disabled" ||
        status === "unavailable" ? (
          <section
            className="entry-auth-panel"
            aria-labelledby="entry-auth-title"
          >
            <h1 id="entry-auth-title">
              {status === "disabled"
                ? c.providerDisabled
                : status === "unavailable"
                  ? c.serviceUnavailable
                  : c.signInTitle}
            </h1>
            <p>
              {status === "disabled"
                ? c.providerDisabledBody
                : status === "unavailable"
                  ? c.serviceUnavailableBody
                  : c.signInBody}
            </p>
            {message && (
              <p className="entry-message error" role="alert">
                {message}
              </p>
            )}
            {status === "signed-out" && (
              <form method="post" action="/api/auth/login">
                <input type="hidden" name="locale" value={locale} />
                <button className="entry-primary" type="submit">
                  {c.signIn}
                </button>
              </form>
            )}
          </section>
        ) : !onboardingEnabled ? (
          <section
            className="entry-auth-panel"
            aria-labelledby="entry-off-title"
          >
            <h1 id="entry-off-title">{c.onboardingDisabled}</h1>
            <p>{c.onboardingDisabledBody}</p>
            <button
              type="button"
              onClick={() => void signOut()}
              disabled={busy}
            >
              {c.signOut}
            </button>
          </section>
        ) : (
          <>
            <h1 className="entry-title">{c.title}</h1>
            <ol className="entry-steps" aria-label={c.title}>
              {([1, 2, 3] as const).map((item) => {
                const label = [c.stepMerchant, c.stepStore, c.stepWarehouse][
                  item - 1
                ];
                return (
                  <li
                    key={item}
                    className={`${item < step ? "complete" : ""} ${item === step ? "current" : ""}`}
                    aria-current={item === step ? "step" : undefined}
                  >
                    <span className="entry-step-number">
                      {item < step ? (
                        <svg
                          aria-hidden="true"
                          width="17"
                          height="17"
                          viewBox="0 0 20 20"
                          fill="none"
                        >
                          <path
                            d="m4 10 4 4 8-9"
                            stroke="currentColor"
                            strokeWidth="2"
                            strokeLinecap="round"
                            strokeLinejoin="round"
                          />
                        </svg>
                      ) : (
                        item
                      )}
                    </span>
                    <span>{label}</span>
                  </li>
                );
              })}
            </ol>

            <section className="entry-wizard" aria-live="polite">
              {step > 1 && (
                <div className="entry-completed-row">
                  <span>
                    {c.merchantName}: <strong>{draft.tenant_name}</strong>
                  </span>
                  <span>{c.completed}</span>
                  <button
                    type="button"
                    onClick={() => setStep(1)}
                    disabled={locked}
                  >
                    {c.edit}
                  </button>
                </div>
              )}

              {step === 1 && (
                <form onSubmit={advance} className="entry-form">
                  <div className="entry-heading-group">
                    <h2>{c.merchantHeading}</h2>
                    <p>{c.merchantBody}</p>
                  </div>
                  <label>
                    <span>{c.merchantName}</span>
                    <input
                      name="tenant_name"
                      value={draft.tenant_name}
                      onChange={(event) =>
                        update("tenant_name", event.target.value)
                      }
                      placeholder={c.merchantPlaceholder}
                      maxLength={120}
                      autoComplete="organization"
                      disabled={locked}
                      required
                    />
                  </label>
                  <div className="entry-actions end">
                    <button
                      className="entry-primary"
                      type="submit"
                      disabled={locked}
                    >
                      {c.nextStore}
                    </button>
                  </div>
                </form>
              )}

              {step === 2 && (
                <form onSubmit={advance} className="entry-form">
                  <h2>{c.storeHeading}</h2>
                  <label>
                    <span>{c.storeName}</span>
                    <input
                      name="store_name"
                      value={draft.store_name}
                      onChange={(event) =>
                        update("store_name", event.target.value)
                      }
                      placeholder={c.storePlaceholder}
                      maxLength={120}
                      disabled={locked}
                      required
                    />
                  </label>
                  <label>
                    <span>{c.currency}</span>
                    <select
                      name="currency"
                      value={draft.currency}
                      onChange={(event) =>
                        update("currency", event.target.value)
                      }
                      disabled={locked || currencies.length === 0}
                      required
                    >
                      {currencies.length === 0 && <option value="">—</option>}
                      {currencies.map((currency) => (
                        <option key={currency} value={currency}>
                          {currency} · {currencyNames.of(currency) ?? currency}
                        </option>
                      ))}
                    </select>
                    <small>
                      {currencies.length
                        ? c.currencyHint
                        : c.currencyUnavailable}
                    </small>
                  </label>
                  <div className="entry-actions">
                    <button
                      type="button"
                      onClick={() => setStep(1)}
                      disabled={locked}
                    >
                      {c.previous}
                    </button>
                    <button
                      className="entry-primary"
                      type="submit"
                      disabled={locked || currencies.length === 0}
                    >
                      {c.nextWarehouse}
                    </button>
                  </div>
                </form>
              )}

              {step === 3 && (
                <form onSubmit={submit} className="entry-form">
                  <h2>{c.warehouseHeading}</h2>
                  <label>
                    <span>{c.warehouseName}</span>
                    <input
                      name="warehouse_name"
                      value={draft.warehouse_name}
                      onChange={(event) =>
                        update("warehouse_name", event.target.value)
                      }
                      placeholder={c.warehousePlaceholder}
                      maxLength={120}
                      disabled={locked}
                      required
                    />
                  </label>
                  <div className="entry-review" aria-label={c.review}>
                    <h3>{c.review}</h3>
                    <dl>
                      <div>
                        <dt>{c.merchantName}</dt>
                        <dd>{draft.tenant_name}</dd>
                      </div>
                      <div>
                        <dt>{c.storeName}</dt>
                        <dd>{draft.store_name}</dd>
                      </div>
                      <div>
                        <dt>{c.currency}</dt>
                        <dd>{draft.currency}</dd>
                      </div>
                      <div>
                        <dt>{c.warehouseName}</dt>
                        <dd>{draft.warehouse_name || "—"}</dd>
                      </div>
                    </dl>
                  </div>
                  <p className="entry-final-hint">{c.finalHint}</p>
                  <div className="entry-actions">
                    <button
                      type="button"
                      onClick={() => setStep(2)}
                      disabled={locked}
                    >
                      {c.previous}
                    </button>
                    {success ? (
                      <button
                        className="entry-primary"
                        type="button"
                        onClick={() => window.location.assign(`/${locale}/`)}
                      >
                        {c.openWorkspace}
                      </button>
                    ) : pending ? (
                      <button
                        className="entry-primary"
                        type="button"
                        onClick={() => void send(pending)}
                        disabled={busy}
                      >
                        {busy ? c.creating : c.retryExact}
                      </button>
                    ) : (
                      <button
                        className="entry-primary"
                        type="submit"
                        disabled={locked}
                      >
                        {busy ? c.creating : c.create}
                      </button>
                    )}
                  </div>
                </form>
              )}

              {error && (
                <p className="entry-message error" role="alert">
                  {error}
                </p>
              )}
              {notice && (
                <p
                  className={`entry-message ${success ? "success" : "pending"}`}
                  role={success ? "status" : "alert"}
                >
                  {notice}
                </p>
              )}
              {pending && !success && notice === c.conflict && (
                <button
                  className="entry-check"
                  type="button"
                  onClick={() => window.location.reload()}
                >
                  {c.checkWorkspace}
                </button>
              )}
            </section>
            <p className="entry-scope">{c.scope}</p>
          </>
        )}
      </main>
    </div>
  );
}
