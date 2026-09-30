"use client";

// Owns the buyer claim page UI (/{locale}/claim, live-keyword-claims-v1 §11.1): read the
// one-time link token from `#t=`, remove it from the address bar and history at once, keep
// it only in this component's memory, preview the claimed lines (B1), and on an explicit
// click add them to the buyer's own cart (B2, one Idempotency-Key per click, no automatic
// retry). Shows the current cart so a 409 can be resolved by removing an item.
// Non-goals: no Quote, checkout or stock hold (claims never reserve stock), no storage of
// the token or preview (no local/session storage, no cache), no third-party script, and
// no merchant data (the preview carries no label, actor or session title).
// Depends on: buyer-client.ts (session coordinator and the only BFF request helper; the
// token is sent only as X-Commerce-Claim-Token on the two claim routes), purchase.ts
// (orderRecoveryRequired, so a session reset never strands an in-flight order),
// claim-contract.ts (fragment grammar and closed response validators), claim-copy.ts.

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
import {
  claimFragment,
  validClaimCart,
  validClaimPreview,
  validClaimRedeemed,
  type ClaimCart,
  type ClaimPreview,
} from "../lib/claim-contract";
import { claimCopy } from "../lib/claim-copy";

type View = "loading" | "ready" | "not-found" | "conflict" | "failed" | "session";

async function json<T>(response: Response, valid: (value: unknown) => value is T): Promise<T> {
  if (!response.ok) throw new BuyerClientError("request_failed", response.status);
  let value: unknown;
  try {
    value = await response.json();
  } catch {
    throw new BuyerClientError("invalid_response");
  }
  if (!valid(value)) throw new BuyerClientError("invalid_response");
  return value;
}

export default function ClaimLink({
  locale: initialLocale,
  demonstration = false,
}: {
  locale: Locale;
  demonstration?: boolean;
}) {
  const [locale, setLocale] = useState(initialLocale);
  const copy = claimCopy[locale];
  const [view, setView] = useState<View>("loading");
  const [context, setContext] = useState("");
  const [preview, setPreview] = useState<ClaimPreview | null>(null);
  const [cart, setCart] = useState<ClaimCart | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<"added" | "nothing" | "cart-failed" | null>(null);
  const [skipped, setSkipped] = useState(false);
  const token = useRef<string | null>(null);
  const fragmentRead = useRef(false);
  const epoch = useRef(0);

  const money = (amount: number, currency: string) => {
    const formatter = new Intl.NumberFormat(locale, {
      style: "currency",
      currency,
      currencyDisplay: locale === "zh-TW" && currency === "TWD" ? "code" : "symbol",
    });
    return formatter.format(amount / 10 ** (formatter.resolvedOptions().maximumFractionDigits ?? 2));
  };

  function fail(reason: unknown) {
    if (
      reason instanceof BuyerClientError &&
      (["context_changed", "requires_reset"].includes(reason.code) || reason.status === 401)
    )
      setView("session");
    else if (reason instanceof BuyerClientError && reason.status === 404) {
      // A dead link has nothing left to show; drop the preview with the token's hope.
      setPreview(null);
      setView("not-found");
    } else if (reason instanceof BuyerClientError && reason.status === 409) setView("conflict");
    else setView("failed");
  }

  const readClaim = async (ctx: string, link: string) =>
    json(await buyerRequest("GET", "claim-link", ctx, undefined, undefined, link), validClaimPreview);
  const readCart = async (ctx: string) => json(await buyerRequest("GET", "cart", ctx), validClaimCart);

  async function load() {
    const version = ++epoch.current;
    setView("loading");
    setNotice(null);
    setSkipped(false);
    const link = token.current;
    if (!link) {
      setView("not-found");
      return;
    }
    try {
      const session = await initializeBuyerSession();
      if (!session.context) throw new BuyerClientError("requires_reset");
      const [claimed, current] = await Promise.all([readClaim(session.context, link), readCart(session.context)]);
      if (version !== epoch.current) return;
      setContext(session.context);
      setPreview(claimed);
      setCart(current);
      setView("ready");
    } catch (reason) {
      if (version === epoch.current) fail(reason);
    }
  }

  useEffect(() => {
    if (!fragmentRead.current) {
      // The fragment never reaches a server; drop it from the address bar and history
      // before any request so it cannot be copied, bookmarked or sent as a Referer.
      fragmentRead.current = true;
      token.current = claimFragment(window.location.hash);
      if (window.location.hash)
        window.history.replaceState(window.history.state, "", window.location.pathname);
    }
    void load();
    const forget = () => {
      token.current = null;
    };
    // Opening another claim link in this tab changes only the fragment (no document load):
    // take the new token the same way. Other fragments (#claim-cart) are page anchors.
    const onHash = () => {
      if (!window.location.hash.startsWith("#t=")) return;
      token.current = claimFragment(window.location.hash);
      window.history.replaceState(window.history.state, "", window.location.pathname);
      setPreview(null);
      setCart(null);
      void load();
    };
    window.addEventListener("pagehide", forget);
    window.addEventListener("hashchange", onHash);
    return () => {
      epoch.current++;
      window.removeEventListener("pagehide", forget);
      window.removeEventListener("hashchange", onHash);
    };
  }, []);

  useEffect(() => {
    document.documentElement.lang = locale;
  }, [locale]);

  async function redeem() {
    const link = token.current;
    if (!preview || !link || busy) return;
    const version = epoch.current;
    setBusy(true);
    setNotice(null);
    try {
      // One fresh key per explicit click; a lost answer is resolved by reloading the claim.
      const result = await json(
        await buyerRequest(
          "POST",
          "claim-link/redeem",
          context,
          { expected_bundle_version: preview.bundle_version },
          crypto.randomUUID(),
          link,
        ),
        validClaimRedeemed,
      );
      if (version !== epoch.current) return;
      setCart(result.cart);
      setSkipped(result.skipped.length > 0);
      setNotice(result.applied.length ? "added" : "nothing");
      const claimed = await readClaim(context, link);
      if (version === epoch.current) setPreview(claimed);
    } catch (reason) {
      if (version === epoch.current) fail(reason);
    } finally {
      setBusy(false);
    }
  }

  async function remove(sku: string) {
    if (!cart || busy) return;
    setBusy(true);
    setNotice(null);
    try {
      const next = await json(
        await buyerRequest(
          "PUT",
          "cart",
          context,
          { expected_version: cart.version, items: cart.items.filter((item) => item.sku_id !== sku) },
          crypto.randomUUID(),
        ),
        validClaimCart,
      );
      setCart(next);
    } catch (reason) {
      if (reason instanceof BuyerClientError && (reason.code !== "request_failed" || reason.status === 401)) fail(reason);
      else setNotice("cart-failed");
    } finally {
      setBusy(false);
    }
  }

  async function renew() {
    setBusy(true);
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
      setBusy(false);
    }
  }

  const names = new Map(preview?.lines.map((line) => [line.sku_id, `${line.product_name} · ${line.sku_code}`]) ?? []);
  const applicable = !!preview?.lines.some((line) => line.pending && line.available);
  const cartVisible = (view === "ready" || view === "conflict") && cart;

  return (
    <>
      <header className="shop-header">
        <span>{copy.store}</span>
        <label className="locale">
          <span className="sr-only">{copy.language}</span>
          <select
            value={locale}
            disabled={busy}
            onChange={(event) => {
              const next = event.target.value as Locale;
              // Path only: the token stays in memory and never returns to the URL.
              window.history.replaceState(window.history.state, "", `/${next}/claim`);
              setLocale(next);
            }}
          >
            <option value="zh-CN">简体中文</option>
            <option value="zh-TW">繁體中文</option>
            <option value="en">English</option>
          </select>
        </label>
      </header>
      {demonstration && <p className="demonstration">{copy.demonstration}</p>}
      <main className="purchase-main claim-main" aria-busy={view === "loading" || busy}>
        <h1>{copy.title}</h1>
        {view === "loading" && <p role="status">{copy.loading}</p>}
        {view === "not-found" && (
          <p role="alert" className="purchase-error claim-alert" data-testid="claim-not-found">
            {copy.notFound}
          </p>
        )}
        {view === "failed" && (
          <div role="alert" className="purchase-error">
            <p>{copy.failed}</p>
            <button disabled={busy} onClick={() => void load()}>{copy.reload}</button>
          </div>
        )}
        {view === "session" && (
          <div role="alert" className="purchase-error">
            <p>{copy.session}</p>
            <button disabled={busy} onClick={() => void renew()}>{copy.renew}</button>
          </div>
        )}
        {view === "conflict" && (
          <div role="alert" className="purchase-error" data-testid="claim-conflict">
            <p>{copy.conflict}</p>
            <div className="claim-actions">
              <a href="#claim-cart">{copy.reviewCart}</a>
              <button disabled={busy} onClick={() => void load()}>{copy.reload}</button>
            </div>
          </div>
        )}
        {view === "ready" && preview && (
          <section className="claim-preview" aria-labelledby="claim-lines-title">
            <p className="claim-intro">{copy.intro}</p>
            <h2 id="claim-lines-title" className="sr-only">{copy.title}</h2>
            <ul className="claim-lines">
              {preview.lines.map((line) => (
                <li key={line.sku_id} data-testid={`claim-line-${line.keyword}`}>
                  <div>
                    <strong>{line.product_name}</strong>
                    <span>{line.sku_code} · {copy.keyword} {line.keyword}</span>
                    {!line.available ? (
                      <small className="claim-flag">{copy.unavailable}</small>
                    ) : !line.pending ? (
                      <small className="claim-flag done">{copy.inCart}</small>
                    ) : null}
                  </div>
                  <div className="claim-amounts">
                    <span>{copy.quantity} {line.quantity}</span>
                    <strong>{money(line.unit_price_minor * line.quantity, line.currency)}</strong>
                  </div>
                </li>
              ))}
            </ul>
            <p className="claim-note">{copy.price}</p>
            <p className="claim-note">{copy.stock}</p>
            <p className="claim-note">
              {copy.expires(new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short" }).format(new Date(preview.expires_at)))}
            </p>
            {notice === "added" && <p role="status" className="claim-success" data-testid="claim-added">{copy.added}</p>}
            {notice === "nothing" && <p role="status" className="claim-success">{copy.nothing}</p>}
            {skipped && <p role="status" className="claim-note">{copy.skipped}</p>}
            <button
              className="primary claim-add"
              data-testid="claim-add"
              disabled={busy || !applicable}
              onClick={() => void redeem()}
            >
              {busy ? copy.adding : preview.bound ? copy.addAgain : copy.add}
            </button>
          </section>
        )}
        {cartVisible && (
          <section id="claim-cart" className="claim-cart" aria-labelledby="claim-cart-title" data-testid="claim-cart">
            <h2 id="claim-cart-title">{copy.cart}</h2>
            {notice === "cart-failed" && <p role="alert" className="claim-note">{copy.cartFailed}</p>}
            {cart.items.length ? (
              <ul>
                {cart.items.map((item) => (
                  <li key={item.sku_id} data-testid={`claim-cart-${item.sku_id}`}>
                    <span>{names.get(item.sku_id) ?? copy.otherItem}</span>
                    <strong>× {item.quantity}</strong>
                    <button disabled={busy} onClick={() => void remove(item.sku_id)}>{copy.remove}</button>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="claim-note">{copy.emptyCart}</p>
            )}
          </section>
        )}
      </main>
    </>
  );
}
