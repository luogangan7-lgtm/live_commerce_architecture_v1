"use client";

// Product page purchase flow (BFF /api/buyer/{catalog,cart,checkout-options,quotes,destination,checkout,orders}
// -> Go /v1/buyer/*). Delivery list (checkout-options): home and the four CVS chains; a chain the store cannot
// sell yet arrives as {available:false, reason} and is shown disabled "Coming soon"
// (contracts/taiwan-cvs-logistics-v1.md §5.1). The CVS store picker itself lives in OrderFlow/CvsPickup and
// returns to this route with ?cvs_selection= (§5.2), which is why the route keeps checkout state across a reload.
import { useEffect, useRef, useState } from "react";
import type { Locale } from "@live-commerce/i18n";
import {
  BuyerClientError,
  initializeBuyerSession,
  readBuyerSession,
  resetBuyerSession,
} from "../lib/buyer-client";
import { purchaseCopy } from "../lib/purchase-copy";
import { orderCopy } from "../lib/order-copy";
import { cvsCopy } from "../lib/cvs-copy";
import OrderFlow, { OrderDetails } from "./OrderFlow";
import OrderHistory from "./OrderHistory";
import { historyCopy } from "../lib/history-copy";
import {
  cartSelection,
  assertPurchaseContext,
  optionKey,
  lineSubtotal,
  pendingPurchase,
  purchasePage,
  readPurchase,
  validCart,
  validOptionRow,
  isUnavailable,
  validProduct,
  validQuote,
  writePurchase,
  knownOrderID,
  readOrder,
  writeCheckout,
  orderRecoveryRequired,
  forgetAddressAttempt,
  continueShopping,
} from "../lib/purchase";
import type { Cart, Product, OptionRow, Quote, Order } from "../lib/purchase";

export default function ProductPurchase({
  locale: initialLocale,
  productID,
  demonstration = false,
}: {
  locale: Locale;
  productID: string;
  demonstration?: boolean;
}) {
  const [locale, setLocale] = useState(initialLocale);
  const copy = purchaseCopy[locale];
  const [products, setProducts] = useState<Product[]>([]);
  const [cursor, setCursor] = useState("");
  const [context, setContext] = useState("");
  const [cart, setCart] = useState<Cart | null>(null);
  const [sku, setSKU] = useState("");
  const [quantity, setQuantity] = useState("1");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [paymentBusy, setPaymentBusy] = useState(false);
  const paymentWorking = useRef(false);
  const onPaymentBusy = (value: boolean) => {
    paymentWorking.current = value;
    setPaymentBusy(value);
  };
  const [error, setError] = useState<
    "failed" | "session" | "conflict" | "pending" | null
  >(null);
  const [deliveryOpen, setDeliveryOpen] = useState(false);
  const [options, setOptions] = useState<OptionRow[]>([]);
  const [optionCursor, setOptionCursor] = useState("");
  const [method, setMethod] = useState("");
  const [quote, setQuote] = useState<Quote | null>(null);
  const [pending, setPending] = useState(false);
  const [pendingKind, setPendingKind] = useState<string | null>(null);
  const [recoveryCountry, setRecoveryCountry] = useState<string | null>(null);
  const [orderLocked, setOrderLocked] = useState(false);
  const [order, setOrder] = useState<Order | null>(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  const historyVisible = useRef(false);
  historyVisible.current = historyOpen;
  const epoch = useRef(0);
  const currentContext = useRef("");
  const currentOrder = useRef<Order | null>(null);
  const working = useRef(false);
  const deliveryRef = useRef<HTMLElement>(null);
  const quoteRef = useRef<HTMLElement>(null);
  const selected = products.find((p) => p.sku_id === sku);
  const subtotal = selected
    ? lineSubtotal(selected.price_minor, Number(quantity))
    : null;
  const found = options.find((o) => optionKey(o) === method);
  // Unavailable rows are listed disabled, so they can never be the chosen delivery.
  const chosen = found && !isUnavailable(found) ? found : undefined;
  const draftKey = (ctx: string) =>
    `commerce-product-selection-v1:${ctx}:${productID}`;
  const money = (amount: number, currency: string) => {
    const formatter = new Intl.NumberFormat(locale, {
      style: "currency",
      currency,
      // zh-TW's local "$" is ambiguous when an order is shared across markets.
      currencyDisplay: locale === "zh-TW" && currency === "TWD" ? "code" : "symbol",
    });
    return formatter.format(
      amount / 10 ** (formatter.resolvedOptions().maximumFractionDigits ?? 2),
    );
  };

  function showError(reason: unknown) {
    if (
      reason instanceof BuyerClientError &&
      (["context_changed", "requires_reset"].includes(reason.code) ||
        reason.status === 401)
    ) {
      setError("session");
      setCart(null);
      setQuote(null);
      setOrder(null);
      currentOrder.current = null;
      setHistoryOpen(false);
    } else if (reason instanceof BuyerClientError && reason.status === 409)
      setError("conflict");
    else setError("failed");
  }

  function syncRecovery(ctx: string) {
    const waiting = pendingPurchase(ctx);
    setPending(waiting !== null);
    setPendingKind(waiting?.kind ?? null);
    setRecoveryCountry(
      waiting?.kind === "destination" ? waiting.body.country : null,
    );
    const id = knownOrderID(ctx);
    setOrderLocked(
      waiting?.kind === "checkout" ||
        waiting?.kind === "next-cart" ||
        id !== null,
    );
    return { waiting, id };
  }

  function acceptOrder(value: Order) {
    currentOrder.current = value;
    setOrder(value);
    setOrderLocked(true);
    setQuote(null);
    setDeliveryOpen(false);
    forgetAddressAttempt();
  }

  async function load() {
    const version = ++epoch.current;
    setLoading(true);
    setPending(false);
    setError(null);
    setQuote(null);
    setOrder(null);
    currentOrder.current = null;
    setDeliveryOpen(false);
    try {
      // Capture the expired context BEFORE initialization rejects it. This
      // preserves the reset guard for an unknown or already-known old order.
      const before = await readBuyerSession();
      if (before.context) {
        if (version !== epoch.current) return;
        currentContext.current = before.context;
        setContext(before.context);
        syncRecovery(before.context);
      }
      const session = await initializeBuyerSession();
      if (!session.context) throw new BuyerClientError("requires_reset");
      const ctx = session.context;
      if (version !== epoch.current) return;
      currentContext.current = ctx;
      setContext(ctx);
      const recovery = syncRecovery(ctx);
      if (["checkout", "next-cart"].includes(recovery.waiting?.kind ?? ""))
        setError("pending");
      if (recovery.id) {
        const existing = await readOrder(ctx, recovery.id);
        if (version !== epoch.current) return;
        // The locator can change while this initial GET is in flight. A late
        // old-order response must not undo another tab's explicit continuation.
        if (knownOrderID(ctx) === recovery.id) acceptOrder(existing);
        else await load();
        return;
      }
      let [page, current] = await Promise.all([
        purchasePage(
          `catalog?product_id=${productID}&limit=100`,
          ctx,
          validProduct,
        ),
        readPurchase("cart", ctx, validCart),
      ]);
      if (version !== epoch.current) return;
      if (
        page.items.some(
          (p) => p.product_id !== productID || p.currency !== current.currency,
        )
      )
        throw new BuyerClientError("invalid_response");
      const raw = sessionStorage.getItem(draftKey(ctx));
      let saved: { sku: string; quantity: string } | null = null;
      if (raw) {
        try {
          const d = JSON.parse(raw);
          if (
            typeof d.sku === "string" &&
            /^[0-9a-f-]{36}$/.test(d.sku) &&
            typeof d.quantity === "string" &&
            /^\d{1,10}$/.test(d.quantity)
          )
            saved = d;
        } catch {
          /* Optional selection only, not a command journal. */
        }
      }
      const visited = new Set<string>();
      while (
        saved &&
        !page.items.some((p) => p.sku_id === saved.sku) &&
        page.next_cursor
      ) {
        if (visited.has(page.next_cursor) || visited.size >= 100)
          throw new BuyerClientError("invalid_response");
        visited.add(page.next_cursor);
        const more = await purchasePage(
          `catalog?product_id=${productID}&limit=100&cursor=${encodeURIComponent(page.next_cursor)}`,
          ctx,
          validProduct,
        );
        if (
          more.items.some(
            (p) =>
              p.product_id !== productID || p.currency !== current.currency,
          )
        )
          throw new BuyerClientError("invalid_response");
        page = {
          items: [...page.items, ...more.items],
          next_cursor: more.next_cursor,
        };
      }
      await assertPurchaseContext(ctx);
      if (version !== epoch.current) return;
      currentContext.current = ctx;
      setContext(ctx);
      setCart(current);
      setProducts(page.items);
      setCursor(page.next_cursor);
      let selectedID =
        page.items.find((p) => current.items.some((i) => i.sku_id === p.sku_id))
          ?.sku_id ??
        page.items[0]?.sku_id ??
        "";
      let qty = String(
        current.items.find((i) => i.sku_id === selectedID)?.quantity ?? 1,
      );
      if (saved) {
        selectedID = page.items.some((p) => p.sku_id === saved.sku)
          ? saved.sku
          : "";
        qty = saved.quantity;
        if (!selectedID) setError("conflict");
      }
      setSKU(selectedID);
      setQuantity(qty);
      const unresolved = syncRecovery(ctx).waiting !== null;
      setPending(unresolved);
      if (unresolved) setError("pending");
      const quoteID = sessionStorage.getItem(
        `commerce-purchase-quote-v1:${ctx}`,
      );
      if (quoteID && /^[0-9a-f-]{36}$/.test(quoteID)) {
        const previous = await readPurchase(
          `quotes/${quoteID}`,
          ctx,
          validQuote,
        );
        if (
          version === epoch.current &&
          previous.cart_version === current.version &&
          previous.cart_id === current.id
        )
          setQuote(previous);
      }
    } catch (reason) {
      if (version === epoch.current) showError(reason);
    } finally {
      if (version === epoch.current) setLoading(false);
    }
  }

  useEffect(() => {
    void load();
    return () => {
      epoch.current++;
      forgetAddressAttempt();
    };
  }, [productID]); // Route locale never changes account/currency.

  useEffect(() => {
    setLocale(initialLocale);
    document.documentElement.lang = initialLocale;
  }, [initialLocale]);

  useEffect(() => {
    if (order || orderLocked)
      requestAnimationFrame(() =>
        document
          .querySelector(
            order ? '[data-testid="order-section"]' : ".purchase-error",
          )
          ?.scrollIntoView({ behavior: "auto", block: "start" }),
      );
  }, [order?.order_id, orderLocked]);

  useEffect(() => {
    if (!context) return;
    let active = true;
    const check = async () => {
      // Opening/returning to the child can focus this tab mid-handoff. Do not
      // replace the selected order object underneath its one-shot transaction.
      if (paymentWorking.current) return;
      const version = epoch.current;
      try {
        await assertPurchaseContext(context);
        if (active && version === epoch.current) {
          const recovery = syncRecovery(context);
          if (["checkout", "next-cart"].includes(recovery.waiting?.kind ?? ""))
            setError("pending");
          if (recovery.id) {
            const current = await readOrder(context, recovery.id);
            if (
              active &&
              version === epoch.current &&
              knownOrderID(context) === recovery.id
            )
              acceptOrder(current);
          } else if (!recovery.waiting && currentOrder.current) {
            await load();
          }
        }
      } catch (reason) {
        if (active && version === epoch.current) {
          epoch.current++;
          showError(reason);
        }
      }
    };
    const changed = (event: StorageEvent) => {
      if (
        event.key === "commerce-buyer-pending-v1" ||
        event.key === `commerce-purchase-pending-v1:${context}` ||
        event.key === `commerce-purchase-order-v1:${context}`
      ) {
        // Another tab finished continuation. Reload current cart/quote rather
        // than leaving this tab's old order/quotation actionable.
        void check();
      }
    };
    window.addEventListener("storage", changed);
    window.addEventListener("focus", check);
    return () => {
      active = false;
      window.removeEventListener("storage", changed);
      window.removeEventListener("focus", check);
    };
  }, [context]);

  function select(nextSKU: string, nextQuantity: string) {
    if (busy || pending || orderLocked || error === "session") return;
    setSKU(nextSKU);
    setQuantity(nextQuantity);
    setQuote(null);
    setDeliveryOpen(false);
    try {
      sessionStorage.setItem(
        draftKey(context),
        JSON.stringify({ sku: nextSKU, quantity: nextQuantity }),
      );
    } catch {
      setError("failed");
    }
  }

  async function act(fn: (isCurrent: () => boolean) => Promise<void>) {
    if (working.current || paymentWorking.current) return;
    const version = epoch.current;
    const startedContext = currentContext.current;
    const isCurrent = () =>
      version === epoch.current && startedContext === currentContext.current;
    working.current = true;
    setBusy(true);
    setError(null);
    try {
      await fn(isCurrent);
    } catch (reason) {
      if (isCurrent()) showError(reason);
    } finally {
      try {
        if (currentContext.current) syncRecovery(currentContext.current);
      } catch {
        setPending(true);
        setOrderLocked(true);
      }
      working.current = false;
      setBusy(false);
    }
  }

  async function deliveryOptions(nextCursor = "") {
    const version = epoch.current;
    const page = await purchasePage(
      `checkout-options?limit=100${nextCursor ? `&cursor=${encodeURIComponent(nextCursor)}` : ""}`,
      context,
      validOptionRow,
    );
    if (version !== epoch.current || context !== currentContext.current) return;
    const rows = page.items.filter((o) => o.currency === cart?.currency);
    setOptions((old) => (nextCursor ? [...old, ...rows] : rows));
    setOptionCursor(page.next_cursor);
    if (!nextCursor) {
      const first = rows.find((o) => !isUnavailable(o));
      setMethod(first ? optionKey(first) : "");
    }
    setDeliveryOpen(true);
    requestAnimationFrame(() =>
      deliveryRef.current?.scrollIntoView({ behavior: "auto", block: "start" }),
    );
  }

  return (
    <>
      <header className="shop-header">
        <span>{copy.store}</span>
        <label className="locale">
          <span className="sr-only">{copy.language}</span>
          <select
            value={locale}
            disabled={busy || paymentBusy}
            onChange={(event) => {
              try {
                sessionStorage.setItem(
                  draftKey(context),
                  JSON.stringify({ sku, quantity }),
                );
                const next = event.target.value as Locale;
                window.history.replaceState(
                  window.history.state,
                  "",
                  `/${next}/products/${productID}`,
                );
                document.documentElement.lang = next;
                setLocale(next);
              } catch {
                setError("failed");
              }
            }}
          >
            <option value="zh-CN">简体中文</option>
            <option value="zh-TW">繁體中文</option>
            <option value="en">English</option>
          </select>
        </label>
      </header>
      {demonstration && <p className="demonstration">{copy.demonstration}</p>}
      <main
        className="purchase-main"
        aria-busy={loading || busy || paymentBusy}
      >
        <nav
          className="purchase-navigation"
          aria-label={historyCopy[locale].title}
        >
          <button
            data-testid="toggle-order-history"
            disabled={
              busy || paymentBusy || loading || pending || error === "session"
            }
            onClick={() => setHistoryOpen((value) => !value)}
          >
            {historyOpen ? historyCopy[locale].back : historyCopy[locale].title}
          </button>
        </nav>
        {error && (
          <div role="alert" className="purchase-error">
            <p>
              {error === "session" && orderLocked
                ? orderCopy[locale].recovery
                : copy[error]}
            </p>
            {pending && error !== "session" ? (
              <button
                data-testid="recover-purchase"
                disabled={busy}
                onClick={() =>
                  void act(async (isCurrent) => {
                    if (pendingKind === "next-cart") {
                      await continueShopping(context);
                      if (isCurrent()) await load();
                      return;
                    }
                    if (pendingKind === "checkout") {
                      const current = await writeCheckout(context);
                      if (isCurrent()) acceptOrder(current);
                      return;
                    }
                    if (pendingKind === "destination") {
                      document
                        .querySelector('[data-testid="address-section"]')
                        ?.scrollIntoView({ behavior: "auto", block: "start" });
                      return;
                    }
                    const result = await writePurchase(context);
                    if (!isCurrent()) return;
                    if (result.kind === "cart") {
                      setCart(result.value);
                      await deliveryOptions();
                    } else setQuote(result.value);
                    setPending(false);
                  })
                }
              >
                {pendingKind === "destination"
                  ? orderCopy[locale].review
                  : copy.recover}
              </button>
            ) : (
              <button
                disabled={busy}
                onClick={() =>
                  void act(async () => {
                    if (error === "session") {
                      const current = await readBuyerSession();
                      if (
                        current.context &&
                        current.state !== "active" &&
                        orderRecoveryRequired(current.context)
                      )
                        throw new BuyerClientError("requires_reset");
                      if (current.context && current.state !== "active")
                        await resetBuyerSession(current.context);
                    }
                    await load();
                  })
                }
              >
                {error === "session" && !orderLocked ? copy.renew : copy.retry}
              </button>
            )}
          </div>
        )}
        {historyOpen && context && (
          <OrderHistory
            key={context}
            context={context}
            locale={locale}
            money={money}
            onError={showError}
            onPaymentBusy={onPaymentBusy}
          />
        )}
        {order && !historyOpen && (
          <>
            <OrderDetails
              context={context}
              order={order}
              locale={locale}
              money={money}
              busy={busy || paymentBusy}
              onPaymentBusy={onPaymentBusy}
              isSelected={(() => {
                const generation = epoch.current;
                return () =>
                  generation === epoch.current &&
                  !historyVisible.current &&
                  currentOrder.current?.order_id === order.order_id;
              })()}
              refresh={() =>
                void act(async (isCurrent) => {
                  const current = await readOrder(context, order.order_id);
                  if (isCurrent()) acceptOrder(current);
                })
              }
            />
            <div className="next-purchase">
              <p className="order-note">{historyCopy[locale].note}</p>
              <button
                data-testid="continue-shopping"
                disabled={busy || paymentBusy || pending || error === "session"}
                onClick={() =>
                  void act(async () => {
                    await continueShopping(context, order.order_id);
                    await load();
                  })
                }
              >
                {historyCopy[locale].next}
              </button>
            </div>
          </>
        )}
        {historyOpen ? null : loading ? (
          <p role="status">{copy.loading}</p>
        ) : order ? null : !products.length ? (
          <p>{copy.empty}</p>
        ) : (
          <>
            <section className="product-detail" aria-labelledby="product-title">
              <h1 id="product-title">{selected?.name ?? products[0].name}</h1>
              {selected && (
                <p className="unit-price">
                  {money(selected.price_minor, selected.currency)}
                </p>
              )}
              <p className="description">
                {selected?.description ?? products[0].description}
              </p>
            </section>
            <fieldset
              className="sku-options"
              disabled={busy || pending || orderLocked || error === "session"}
            >
              <legend>{copy.choose}</legend>
              {products.map((p) => (
                <label
                  key={p.sku_id}
                  className={p.sku_id === sku ? "sku-row selected" : "sku-row"}
                >
                  <input
                    type="radio"
                    name="sku"
                    value={p.sku_id}
                    checked={p.sku_id === sku}
                    onChange={() => select(p.sku_id, quantity)}
                  />
                  <span>{p.sku_code}</span>
                </label>
              ))}
              {cursor && (
                <button
                  className="text-button"
                  onClick={() =>
                    void act(async (isCurrent) => {
                      const page = await purchasePage(
                        `catalog?product_id=${productID}&limit=100&cursor=${encodeURIComponent(cursor)}`,
                        context,
                        validProduct,
                      );
                      if (!isCurrent()) return;
                      if (
                        page.items.some(
                          (p) =>
                            p.product_id !== productID ||
                            p.currency !== cart?.currency,
                        )
                      )
                        throw new BuyerClientError("invalid_response");
                      setProducts((old) =>
                        Array.from(
                          new Map(
                            [...old, ...page.items].map((p) => [p.sku_id, p]),
                          ).values(),
                        ),
                      );
                      setCursor(page.next_cursor);
                    })
                  }
                >
                  {copy.more}
                </button>
              )}
            </fieldset>
            <div className="quantity-row">
              <label htmlFor="quantity">{copy.quantity}</label>
              <div className="stepper">
                <button
                  aria-label={copy.decrease}
                  disabled={
                    busy ||
                    pending ||
                    orderLocked ||
                    error === "session" ||
                    Number(quantity) <= 1
                  }
                  onClick={() => select(sku, String(Number(quantity) - 1))}
                >
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M5 12h14" />
                  </svg>
                </button>
                <input
                  id="quantity"
                  type="number"
                  inputMode="numeric"
                  min="1"
                  max="1000000000"
                  step="1"
                  value={quantity}
                  disabled={
                    busy || pending || orderLocked || error === "session"
                  }
                  onChange={(e) => select(sku, e.target.value)}
                  aria-invalid={subtotal === null}
                />
                <button
                  aria-label={copy.increase}
                  disabled={
                    busy ||
                    pending ||
                    orderLocked ||
                    error === "session" ||
                    Number(quantity) >= 1_000_000_000
                  }
                  onClick={() => select(sku, String(Number(quantity) + 1))}
                >
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M5 12h14M12 5v14" />
                  </svg>
                </button>
              </div>
            </div>
            {subtotal === null && <p role="status">{copy.amountInvalid}</p>}
            {!!cart?.items.some((i) => i.sku_id !== sku) && (
              <p className="cart-note">{copy.savedCart}</p>
            )}
            {deliveryOpen && (
              <section
                className="delivery-section"
                ref={deliveryRef}
                aria-labelledby="delivery-title"
              >
                <h2 id="delivery-title">{copy.deliveryTitle}</h2>
                {!options.length ? (
                  <p>{copy.noDelivery}</p>
                ) : (
                  <>
                    <label className="delivery-label" htmlFor="delivery">
                      {copy.selectDelivery}
                    </label>
                    <select
                      id="delivery"
                      value={method}
                      disabled={
                        busy || pending || orderLocked || error === "session"
                      }
                      onChange={(e) => {
                        setMethod(e.target.value);
                        setQuote(null);
                      }}
                    >
                      {options.map((o) => (
                        <option
                          key={optionKey(o)}
                          value={optionKey(o)}
                          disabled={isUnavailable(o)}
                        >
                          {locale === "zh-CN"
                            ? o.name_hans
                            : locale === "zh-TW"
                              ? o.name_hant
                              : o.name_en}{" "}
                          · {o.country}
                          {isUnavailable(o) ? ` · ${cvsCopy[locale].comingSoon}` : ""}
                        </option>
                      ))}
                    </select>
                    {chosen && chosen.delivery_kind !== "home" && (
                      <p data-testid="cvs-next-step">{copy.cvs}</p>
                    )}
                  </>
                )}
                {optionCursor && (
                  <button
                    disabled={busy}
                    onClick={() =>
                      void act(() => deliveryOptions(optionCursor))
                    }
                  >
                    {copy.more}
                  </button>
                )}
              </section>
            )}
            {quote && (
              <section
                className="quotation"
                ref={quoteRef}
                aria-labelledby="quote-title"
              >
                <h2 id="quote-title">{copy.lines}</h2>
                <ul>
                  {quote.lines.map((line) => (
                    <li key={line.sku_id}>
                      <span>
                        {line.name} · {line.code} × {line.quantity}
                      </span>
                      <span>
                        {money(
                          line.unit_price_minor * line.quantity,
                          quote.currency,
                        )}
                      </span>
                    </li>
                  ))}
                </ul>
                <dl>
                  <div>
                    <dt>{copy.shipping}</dt>
                    <dd>
                      {money(quote.amount.shipping_minor, quote.currency)}
                    </dd>
                  </div>
                  <div>
                    <dt>{copy.taxes}</dt>
                    <dd>{money(quote.amount.tax_minor, quote.currency)}</dd>
                  </div>
                  <div>
                    <dt>{copy.discount}</dt>
                    <dd>
                      {money(quote.amount.discount_minor, quote.currency)}
                    </dd>
                  </div>
                  <div className="total">
                    <dt>{copy.total}</dt>
                    <dd>{money(quote.amount.total_minor, quote.currency)}</dd>
                  </div>
                </dl>
                <p>
                  {copy.expires}{" "}
                  {new Intl.DateTimeFormat(locale, {
                    dateStyle: "short",
                    timeStyle: "short",
                  }).format(new Date(quote.expires_at))}
                </p>
                <p>{copy.noPayment}</p>
                <button
                  className="text-button"
                  disabled={
                    busy || pending || orderLocked || error === "session"
                  }
                  onClick={() =>
                    void act(async (isCurrent) => {
                      await deliveryOptions();
                      if (isCurrent()) setQuote(null);
                    })
                  }
                >
                  {copy.changeDelivery}
                </button>
              </section>
            )}
            {(quote || pendingKind === "destination") &&
              cart &&
              !orderLocked &&
              error !== "session" && (
                <OrderFlow
                  key={`${context}:${quote?.id ?? "address-recovery"}`}
                  context={context}
                  cart={cart}
                  quote={quote}
                  recoveryCountry={recoveryCountry}
                  locale={locale}
                  busy={busy}
                  blocked={pending && pendingKind !== "destination"}
                  recoveringDestination={pendingKind === "destination"}
                  run={act}
                  money={money}
                  onOrder={acceptOrder}
                  reload={async () => {
                    // Requotation may unmount the form, so first resolve its
                    // PII-free destination journal through explicit confirmation.
                    if (pendingPurchase(context)?.kind === "destination")
                      return;
                    sessionStorage.removeItem(
                      `commerce-purchase-quote-v1:${context}`,
                    );
                    await load();
                  }}
                />
              )}
          </>
        )}
      </main>
      {selected && !historyOpen && !order && !orderLocked && (
        <footer className="purchase-footer">
          <div className="footer-inner">
            <div>
              <span>{quote ? copy.total : copy.subtotal}</span>
              <strong>
                {quote
                  ? money(quote.amount.total_minor, quote.currency)
                  : subtotal === null
                    ? "—"
                    : money(subtotal, selected.currency)}
              </strong>
              <small>{quote ? copy.unpaid : copy.shippingLater}</small>
            </div>
            <button
              className={quote ? "quote-navigation" : "primary"}
              disabled={
                busy ||
                loading ||
                error === "session" ||
                pending ||
                subtotal === null ||
                (!quote && deliveryOpen && !chosen)
              }
              onClick={() =>
                void act(async (isCurrent) => {
                  if (quote) {
                    quoteRef.current?.scrollIntoView({
                      behavior: "auto",
                      block: "start",
                    });
                    return;
                  }
                  if (!cart || subtotal === null) return;
                  if (!deliveryOpen) {
                    const result = await writePurchase(context, {
                      kind: "cart",
                      body: cartSelection(cart, sku, Number(quantity)),
                    });
                    if (!isCurrent()) return;
                    if (result.kind !== "cart")
                      throw new BuyerClientError("invalid_response");
                    setCart(result.value);
                    setQuote(null);
                    await deliveryOptions();
                  } else if (chosen) {
                    const result = await writePurchase(context, {
                      kind: "quote",
                      body: {
                        cart_version: cart.version,
                        market_id: chosen.market_id,
                        country: chosen.country,
                        method: chosen.method,
                      },
                    });
                    if (!isCurrent()) return;
                    if (result.kind !== "quote")
                      throw new BuyerClientError("invalid_response");
                    setQuote(result.value);
                  }
                })
              }
            >
              {busy
                ? copy.loading
                : quote
                  ? copy.viewQuote
                  : deliveryOpen
                    ? copy.quote
                    : copy.delivery}
            </button>
          </div>
        </footer>
      )}
    </>
  );
}
