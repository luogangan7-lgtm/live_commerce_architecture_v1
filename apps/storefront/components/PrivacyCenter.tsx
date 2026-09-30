"use client";

// Buyer privacy page UI (/{locale}/privacy, U8): current consents with toggles (context "settings"), "Download my data",
// "Erase my data" (native <dialog>, typed ERASE, one Idempotency-Key per dialog open reused on retry), and the "your data
// was erased" state on 410. BFF GET /api/buyer/privacy, PUT /api/buyer/consents, POST /api/buyer/privacy/export,
// POST /api/buyer/privacy/erasure -> Go /v1/buyer/{privacy,consents,privacy/export,privacy/erasure}
// (internal/buyerhttp/privacy.go; internal/customers.*). Session/context handling reuses buyer-client.ts exactly like
// ClaimLink. No consent or erasure authority here: every toggle re-reads GET privacy before it shows a state, and an
// unknown result is retried with the same key or re-read, never assumed.
// Non-goals: no local/session storage of privacy data, no third-party script, no merchant data.
import { useEffect, useRef, useState } from "react";
import type { Locale } from "@live-commerce/i18n";
import {
  BuyerClientError,
  buyerRequest,
  initializeBuyerSession,
  readBuyerSession,
  resetBuyerSession,
} from "../lib/buyer-client";
import { orderRecoveryRequired } from "../lib/purchase";
import { privacyCopy } from "../lib/privacy-copy";
import {
  consentBody,
  consentPairs,
  erasureBody,
  LC_PRIVACY_POLICY_VERSION,
  privacyFailure,
  validBuyerExport,
  validBuyerPrivacy,
  validConsentResult,
  validErasureSummary,
  type BuyerPrivacy,
  type ConsentChannel,
  type ConsentPurpose,
  type PrivacyFailure,
} from "../lib/privacy-contract";

type View = "loading" | "ready" | "erased" | "failed" | "session";

async function errorCode(response: Response): Promise<string> {
  const body: unknown = await response.json().catch(() => null);
  const code = body && typeof body === "object" ? (body as { code?: unknown }).code : null;
  return typeof code === "string" && /^[a-z_]{1,64}$/.test(code) ? code : "";
}

export default function PrivacyCenter({
  locale: initialLocale,
  notice = false,
}: {
  locale: Locale;
  notice?: boolean;
}) {
  const [locale, setLocale] = useState(initialLocale);
  const copy = privacyCopy[locale];
  const [view, setView] = useState<View>("loading");
  const [context, setContext] = useState("");
  const [privacy, setPrivacy] = useState<BuyerPrivacy | null>(null);
  const [busy, setBusy] = useState("");
  const [message, setMessage] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);
  const epoch = useRef(0);
  const dialog = useRef<HTMLDialogElement>(null);
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const [problem, setProblem] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const eraseKey = useRef("");

  function fail(reason: unknown) {
    if (reason instanceof BuyerClientError && (["context_changed", "requires_reset"].includes(reason.code) || reason.status === 401))
      setView("session");
    else setView("failed");
  }
  async function readPrivacy(ctx: string) {
    const response = await buyerRequest("GET", "privacy", ctx);
    if (response.status === 410) return null;
    if (!response.ok) throw new BuyerClientError("request_failed", response.status);
    const value: unknown = await response.json().catch(() => null);
    if (!validBuyerPrivacy(value)) throw new BuyerClientError("invalid_response");
    return value;
  }
  async function load() {
    const version = ++epoch.current;
    setView("loading");
    try {
      const session = await initializeBuyerSession();
      if (!session.context) throw new BuyerClientError("requires_reset");
      const current = await readPrivacy(session.context);
      if (version !== epoch.current) return;
      setContext(session.context);
      setPrivacy(current);
      setView(current === null || current.erased ? "erased" : "ready");
    } catch (reason) {
      if (version === epoch.current) fail(reason);
    }
  }
  useEffect(() => {
    void load();
    // Personal data leaves memory with the page (bfcache must not keep it).
    const forget = () => {
      setPrivacy(null);
    };
    window.addEventListener("pagehide", forget);
    return () => {
      epoch.current++;
      window.removeEventListener("pagehide", forget);
    };
  }, []);
  useEffect(() => {
    document.documentElement.lang = locale;
  }, [locale]);
  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    if (open && !element.open) element.showModal();
    if (!open && element.open) element.close();
  }, [open]);

  async function renew() {
    setBusy("renew");
    try {
      const current = await readBuyerSession();
      if (current.context && current.state !== "active") {
        if (orderRecoveryRequired(current.context)) throw new BuyerClientError("requires_reset");
        await resetBuyerSession(current.context);
      }
      await load();
    } catch (reason) {
      fail(reason);
    } finally {
      setBusy("");
    }
  }

  async function toggle(purpose: ConsentPurpose, channel: ConsentChannel, granted: boolean) {
    if (busy) return;
    setBusy(purpose);
    setMessage(null);
    try {
      // One fresh key per click. An unknown result is settled by re-reading the state, never by guessing.
      const response = await buyerRequest("PUT", "consents", context, consentBody(purpose, channel, granted, "settings"), `consent-${crypto.randomUUID()}`);
      if (response.status === 410) return setView("erased");
      const value: unknown = response.ok ? await response.json().catch(() => null) : null;
      const saved = response.ok && validConsentResult(value);
      const current = await readPrivacy(context).catch(() => undefined);
      if (current === null || current?.erased) return setView("erased");
      if (current) setPrivacy(current);
      setMessage(saved ? { tone: "ok", text: copy.saved } : { tone: "bad", text: copy.saveFailed });
    } catch (reason) {
      if (reason instanceof BuyerClientError && (reason.code === "context_changed" || reason.status === 401)) fail(reason);
      else setMessage({ tone: "bad", text: copy.saveFailed });
    } finally {
      setBusy("");
    }
  }

  async function download() {
    if (busy) return;
    setBusy("export");
    setMessage(null);
    try {
      const response = await buyerRequest("POST", "privacy/export", context, {}, `export-${crypto.randomUUID()}`);
      if (response.status === 410) return setView("erased");
      if (!response.ok) {
        const kind = privacyFailure(response.status, await errorCode(response));
        return setMessage({ tone: "bad", text: kind === "export_too_large" ? copy.exportTooLarge : copy.genericFailure });
      }
      const text = await response.text();
      let parsed: unknown = null;
      try {
        parsed = JSON.parse(text);
      } catch {
        /* falls through to the failure message */
      }
      if (!validBuyerExport(parsed)) return setMessage({ tone: "bad", text: copy.genericFailure });
      // The file is the buyer's own data: a Blob download, object URL revoked right after the click, nothing stored.
      const href = URL.createObjectURL(new Blob([text], { type: "application/json" }));
      const link = document.createElement("a");
      link.href = href;
      link.download = "my-data.json";
      link.rel = "noopener";
      document.body.append(link);
      link.click();
      link.remove();
      setTimeout(() => URL.revokeObjectURL(href), 1000);
      setMessage({ tone: "ok", text: copy.downloaded });
    } catch (reason) {
      if (reason instanceof BuyerClientError && (reason.code === "context_changed" || reason.status === 401)) fail(reason);
      else setMessage({ tone: "bad", text: copy.genericFailure });
    } finally {
      setBusy("");
    }
  }

  function beginErase() {
    eraseKey.current = `erase-${crypto.randomUUID()}`;
    setTyped("");
    setProblem("");
    setUncertain(false);
    setOpen(true);
  }
  async function erase() {
    if (typed !== "ERASE" || busy) return;
    setBusy("erase");
    setProblem("");
    try {
      const response = await buyerRequest("POST", "privacy/erasure", context, erasureBody, eraseKey.current);
      if (response.status === 410) {
        setOpen(false);
        return setView("erased");
      }
      if (response.ok) {
        const value: unknown = await response.json().catch(() => null);
        if (!validErasureSummary(value)) throw new BuyerClientError("invalid_response");
        setOpen(false);
        setPrivacy(null);
        return setView("erased");
      }
      const kind: PrivacyFailure = privacyFailure(response.status, await errorCode(response));
      // A definite refusal ends this key; the next attempt is a new request.
      eraseKey.current = `erase-${crypto.randomUUID()}`;
      setUncertain(false);
      setProblem(kind === "erasure_blocked" ? copy.eraseBlocked : kind === "idempotency_conflict" ? copy.conflict : copy.genericFailure);
    } catch (reason) {
      if (reason instanceof BuyerClientError && reason.code === "context_changed") {
        setOpen(false);
        return fail(reason);
      }
      // Lost answer or unreadable body: the same key replays the stored result, so retrying is safe.
      setUncertain(true);
      setProblem(copy.uncertain);
    } finally {
      setBusy("");
    }
  }

  const label = (purpose: ConsentPurpose) => (purpose === "marketing_messages" ? copy.marketing : copy.ads);
  return (
    <>
      <header className="shop-header">
        <span>{copy.title}</span>
        <label className="locale">
          <span className="sr-only">{copy.language}</span>
          <select
            value={locale}
            disabled={busy !== ""}
            onChange={(event) => {
              const next = event.target.value as Locale;
              window.history.replaceState(window.history.state, "", `/${next}/privacy`);
              setLocale(next);
            }}
          >
            <option value="zh-CN">简体中文</option>
            <option value="zh-TW">繁體中文</option>
            <option value="en">English</option>
          </select>
        </label>
      </header>
      <main className="purchase-main claim-main" aria-busy={view === "loading" || busy !== ""}>
        <h1>{copy.title}</h1>
        <p className="claim-intro">{copy.intro}</p>
        {view === "loading" && <p role="status">{copy.loading}</p>}
        {view === "failed" && (
          <div role="alert" className="purchase-error">
            <p>{copy.failed}</p>
            <button disabled={busy !== ""} onClick={() => void load()}>{copy.reload}</button>
          </div>
        )}
        {view === "session" && (
          <div role="alert" className="purchase-error">
            <p>{copy.session}</p>
            <button disabled={busy !== ""} onClick={() => void renew()}>{copy.renew}</button>
          </div>
        )}
        {view === "erased" && (
          <section role="status" className="claim-preview" data-testid="privacy-erased">
            <h2>{copy.erasedTitle}</h2>
            <p>{copy.erasedText}</p>
          </section>
        )}
        {view === "ready" && privacy && (
          <>
            <section className="claim-preview" aria-labelledby="privacy-choices-title" data-testid="privacy-choices">
              <h2 id="privacy-choices-title">{copy.choicesTitle}</h2>
              <ul className="claim-lines">
                {consentPairs.map(({ purpose, channel }) => {
                  const on = privacy.consents[purpose];
                  return (
                    <li key={purpose} data-testid={`privacy-${purpose}`}>
                      <div>
                        <strong>{label(purpose)}</strong>
                        <span>{on ? copy.granted : copy.notGranted}</span>
                      </div>
                      <button
                        data-testid={`privacy-toggle-${purpose}`}
                        disabled={busy !== ""}
                        aria-label={`${on ? copy.toggleOff : copy.toggleOn}: ${label(purpose)}`}
                        onClick={() => void toggle(purpose, channel, !on)}
                      >
                        {on ? copy.toggleOff : copy.toggleOn}
                      </button>
                    </li>
                  );
                })}
              </ul>
              <p className="claim-note">{copy.historyNote}</p>
              {message && (
                <p role={message.tone === "ok" ? "status" : "alert"} className={message.tone === "ok" ? "claim-success" : "claim-note"} data-testid="privacy-message">
                  {message.text}
                </p>
              )}
            </section>
            <section className="claim-cart" aria-labelledby="privacy-download-title">
              <h2 id="privacy-download-title">{copy.downloadTitle}</h2>
              <p>{copy.downloadText}</p>
              <button data-testid="privacy-download" disabled={busy !== ""} onClick={() => void download()}>
                {busy === "export" ? copy.downloading : copy.download}
              </button>
            </section>
            <section className="claim-cart" aria-labelledby="privacy-erase-title">
              <h2 id="privacy-erase-title">{copy.eraseTitle}</h2>
              <p>{copy.eraseText}</p>
              <p className="claim-note">{copy.eraseKept}</p>
              <button data-testid="privacy-erase" disabled={busy !== ""} onClick={beginErase}>{copy.erase}</button>
            </section>
          </>
        )}
        <section
          className="claim-cart"
          aria-labelledby="privacy-notice-title"
          id="notice"
          data-testid="privacy-notice"
          data-highlight={notice ? "true" : undefined}
        >
          <h2 id="privacy-notice-title">
            {copy.noticeTitle} ({copy.noticeVersion(LC_PRIVACY_POLICY_VERSION)})
          </h2>
          <ul>
            {copy.noticeItems.map((item) => (
              <li key={item}>{item}</li>
            ))}
          </ul>
        </section>
        <dialog ref={dialog} aria-labelledby="privacy-erase-dialog-title" data-testid="privacy-erase-dialog" onClose={() => setOpen(false)}
          style={{ width: "min(440px, calc(100vw - 32px))", padding: 20, border: "1px solid #becddd", borderRadius: 8 }}>
          {open && (
            <form
              method="dialog"
              onSubmit={(event) => {
                event.preventDefault();
                void erase();
              }}
            >
              <h2 id="privacy-erase-dialog-title">{copy.eraseDialogTitle}</h2>
              <p>{copy.eraseText}</p>
              <p>{copy.eraseKept}</p>
              <div className="address-fields">
                <label className="wide">
                  {copy.eraseType}
                  <input data-testid="privacy-erase-typed" autoComplete="off" autoCapitalize="off" spellCheck={false}
                    value={typed} onChange={(event) => setTyped(event.target.value)} />
                </label>
              </div>
              {problem && <p role="alert" className="purchase-error" data-testid="privacy-erase-problem">{problem}</p>}
              <div className="claim-actions">
                <button type="button" onClick={() => setOpen(false)}>{copy.cancel}</button>
                <button type="submit" className="primary" data-testid="privacy-erase-submit" disabled={typed !== "ERASE" || busy !== ""}>
                  {busy === "erase" ? copy.erasing : uncertain ? copy.retrySame : copy.eraseConfirm}
                </button>
              </div>
            </form>
          )}
        </dialog>
      </main>
    </>
  );
}
