"use client";

// Storefront CVS pickup section, rendered by OrderFlow.tsx when the quoted delivery option is a CVS kind.
// BFF routes (lib/buyer-client.ts buyerRequest -> lib/buyer-server.ts) and the Go endpoints behind them
// (contracts/taiwan-cvs-logistics-v1.md §5.2, §16.1):
//   POST /api/buyer/cvs-selections            -> Go POST /v1/buyer/cvs-selections (ECPay e-map form, keyed)
//   POST /api/buyer/cvs-selections/{id}/verify -> Go POST /v1/buyer/cvs-selections/{id}/verify (keyless)
//   POST /api/buyer/cvs-stores                -> Go POST /v1/buyer/cvs-stores (buyer-entered store, keyed)
// Two store sources, decided by the server per store (option.pickup_selection): "ecpay_map" (same-tab POST to the
// ECPay map, return with ?cvs_selection=, verify) and "buyer_entered" (typed code/name/address, official search
// link, never labelled verified). Nothing here writes the destination or the order: OrderFlow calls
// handle.ensure() to get a pickup_id, then writes the destination and Begin (checkout) itself.
// Also exports CvsOrderStatus, the CVS/pay-at-pickup block of the buyer order page (§5.3, §16.4, §16.8).
import { useEffect, useRef, useState, type MutableRefObject } from "react";
import type { Locale } from "@live-commerce/i18n";
import { BuyerClientError, buyerRequest } from "../lib/buyer-client";
import {
  CVS_SEARCH_LINKS,
  errorField,
  isCvsErrorCode,
  isCvsKind,
  normalizeSelection,
  normalizeTwMobile,
  recipientNameOK,
  saveCvsDraft,
  shipmentCopyKey,
  takeCvsDraft,
  cvsReturnID,
  validBuyerStore,
  validBuyerStoreBody,
  validCvsSelection,
  validCvsSelectionOpen,
  validReturnPath,
  validStoreAddress,
  validStoreCode,
  validStoreName,
  type CvsDraft,
  type CvsErrorCode,
  type CvsKind,
  type CvsSelectionOpen,
  type PaymentMode,
} from "../lib/cvs-contract";
import { cvsCopy } from "../lib/cvs-copy";
import { assertPurchaseContext } from "../lib/purchase";
import type { Option, Order } from "../lib/purchase";

export type PickupHandle = {
  // Resolves the chosen store's pickup_id (creating the buyer-entered record when needed), or null after
  // showing why the store or recipient inputs are not ready. Throws only for an uncertain/transport failure.
  ensure: () => Promise<string | null>;
};
type Chosen = {
  pickup_id: string;
  kind: CvsKind;
  code: string;
  name: string;
  address: string;
  outside: boolean;
  source: "map" | "entered";
};
type Notice =
  | { key: "verifying" | "rejected" | "expired" | "mismatch" | "noSession" | "openFailed" | "unavailable" }
  | { key: "retry"; seconds: number }
  | null;
type Run = (work: (isCurrent: () => boolean) => Promise<void>) => Promise<void>;
type FieldErrors = Partial<
  Record<"store_code" | "store_name" | "store_address" | "recipient" | "store", string>
>;

// A refusal body {code} from the BFF; only codes the contract lists are shown, everything else is generic.
async function refusalCode(response: Response): Promise<CvsErrorCode | null> {
  try {
    const body: unknown = await response.clone().json();
    const code = (body as { code?: unknown })?.code;
    return isCvsErrorCode(code) ? code : null;
  } catch {
    return null;
  }
}
// The service code is the part of `delivery:<code>` (begin_hold resolves it the same way).
const serviceCode = (option: Option) => option.method.slice("delivery:".length);

export default function CvsPickup({
  context,
  locale,
  option,
  cartVersion,
  recipient,
  onRecipient,
  paymentMode,
  onPaymentMode,
  onRestore,
  onStoreChange,
  run,
  handle,
  total,
}: {
  context: string;
  locale: Locale;
  option: Option & { delivery_kind: CvsKind };
  cartVersion: number;
  recipient: { recipient_name: string; phone: string };
  onRecipient: (next: { recipient_name: string; phone: string }) => void;
  paymentMode: PaymentMode;
  onPaymentMode: (mode: PaymentMode) => void;
  onRestore: (draft: CvsDraft) => void;
  onStoreChange: () => void;
  run: Run;
  handle: MutableRefObject<PickupHandle | null>;
  total: string;
}) {
  const copy = cvsCopy[locale];
  const kind = option.delivery_kind;
  const mapMode = option.pickup_selection === "ecpay_map";
  const payAtPickup = paymentMode === "pay_at_pickup";
  const [store, setStore] = useState<Chosen | null>(null);
  const [notice, setNotice] = useState<Notice>(null);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [opening, setOpening] = useState(false);
  const [retryReady, setRetryReady] = useState(true);
  const [copied, setCopied] = useState<"" | "ok" | "failed">("");
  const [entered, setEntered] = useState({ code: "", name: "", address: "" });
  // One key per distinct typed store; a retry of the same body replays the same server record.
  const attempt = useRef<{ key: string; body: string } | null>(null);
  const live = useRef(0);
  const restore = useRef(onRestore);
  restore.current = onRestore;
  const changed = useRef(onStoreChange);
  changed.current = onStoreChange;

  // Return from the ECPay map: take the saved draft once, then verify the selection named in the URL.
  useEffect(() => {
    if (!mapMode) return;
    const version = ++live.current;
    try {
      const draft = takeCvsDraft(window.sessionStorage, context);
      if (draft) restore.current(draft);
    } catch {
      /* sessionStorage blocked: the buyer retypes name and mobile */
    }
    const selection = cvsReturnID(window.location.search);
    if (selection) void verify(selection, version);
    const back = (event: PageTransitionEvent) => {
      if (event.persisted) setOpening(false); // browser Back from the map restores this page from bfcache
    };
    window.addEventListener("pageshow", back);
    return () => {
      live.current++;
      window.removeEventListener("pageshow", back);
    };
  }, [context, option.market_id, option.method]);

  async function verify(selection: string, version: number) {
    setNotice({ key: "verifying" });
    try {
      const response = await buyerRequest(
        "POST",
        `cvs-selections/${selection}/verify`,
        context,
      );
      if (version !== live.current) return;
      if (!response.ok) {
        // Other owners are indistinguishable from not-found (§4.3 read_cvs_selection): in an in-app browser that
        // lost the cookie this is exactly what the buyer sees.
        setNotice({
          key: response.status === 404 || response.status === 401 ? "noSession" : "unavailable",
        });
        return;
      }
      const data: unknown = await response.json();
      if (!validCvsSelection(data) || data.selection_id !== selection)
        throw new BuyerClientError("invalid_response");
      await assertPurchaseContext(context);
      if (version !== live.current) return;
      const result = normalizeSelection(data);
      if (result.state === "VERIFIED" && result.pickup) {
        if (result.pickup.kind !== kind) {
          setNotice({ key: "mismatch" });
          return;
        }
        setStore({
          pickup_id: result.pickup.pickup_id,
          kind,
          code: result.pickup.code,
          name: result.pickup.name,
          address: result.pickup.address,
          outside: result.pickup.outside,
          source: "map",
        });
        setNotice(null);
        changed.current();
      } else if (result.state === "RETURNED") {
        const seconds = result.retry_after_s ?? 5;
        setNotice({ key: "retry", seconds });
        setRetryReady(false);
        window.setTimeout(() => {
          if (version === live.current) setRetryReady(true);
        }, Math.min(seconds, 3600) * 1000);
      } else if (result.state === "REJECTED") setNotice({ key: "rejected" });
      else if (result.state === "EXPIRED") setNotice({ key: "expired" });
      else setNotice(null); // OPEN: the buyer came back without choosing a store
    } catch {
      if (version === live.current) setNotice({ key: "unavailable" });
    }
  }

  function clearReturn() {
    try {
      window.history.replaceState(window.history.state, "", window.location.pathname);
    } catch {
      /* the param is ignored without a draft anyway */
    }
  }
  function changeStore() {
    setStore(null);
    setNotice(null);
    clearReturn();
    changed.current();
  }

  // Same-tab hand-off to the ECPay map (§5.2, U5): the recipient inputs are saved first because the whole page
  // is replaced. No iframe, no target, no window.open: a hidden form is posted top-level.
  async function openMap(isCurrent: () => boolean) {
    setErrors({});
    setNotice(null);
    const returnPath = window.location.pathname;
    if (!validReturnPath(returnPath)) {
      setNotice({ key: "openFailed" });
      return;
    }
    const response = await buyerRequest(
      "POST",
      "cvs-selections",
      context,
      {
        cart_version: cartVersion,
        market_id: option.market_id,
        service_code: serviceCode(option),
        return_path: returnPath,
      },
      crypto.randomUUID(),
    );
    if (!isCurrent()) return;
    if (!response.ok) {
      const code = await refusalCode(response);
      if (code) {
        setErrors({ store: copy.errors[code] });
        return;
      }
      throw new BuyerClientError("request_failed", response.status);
    }
    const data: unknown = await response.json();
    if (!validCvsSelectionOpen(data)) throw new BuyerClientError("invalid_response");
    await assertPurchaseContext(context);
    if (!isCurrent()) return;
    try {
      saveCvsDraft(window.sessionStorage, context, {
        recipient_name: recipient.recipient_name,
        phone: recipient.phone,
        payment_mode: paymentMode,
      });
    } catch {
      /* the buyer retypes name and mobile on return */
    }
    setOpening(true);
    submitForm(data);
  }
  function submitForm(open: CvsSelectionOpen) {
    const form = document.createElement("form");
    form.method = "post";
    form.action = open.form.action; // allowlisted by validMapForm; never derived from input
    form.hidden = true;
    for (const [name, value] of Object.entries(open.form.fields)) {
      const input = document.createElement("input");
      input.type = "hidden";
      input.name = name;
      input.value = value;
      form.appendChild(input);
    }
    document.body.appendChild(form);
    form.submit();
  }

  async function ensure(): Promise<string | null> {
    setErrors({});
    // C3/F5 mirror: pay-at-pickup and ECPay-created shipments need the real name and a 09 mobile.
    const strict = payAtPickup || mapMode;
    const name = recipient.recipient_name.trim();
    const phone = recipient.phone.trim();
    if (strict ? !recipientNameOK(name) || normalizeTwMobile(phone) === null : name === "" || phone === "") {
      setErrors({ recipient: copy.invalidRecipient });
      return null;
    }
    if (mapMode) {
      if (!store) {
        setErrors({ store: copy.noStoreYet });
        return null;
      }
      return store.pickup_id;
    }
    const code = entered.code.trim();
    const storeName = entered.name.trim();
    const address = entered.address.trim();
    if (store?.source === "entered" && store.code === code && store.name === storeName && store.address === address)
      return store.pickup_id;
    const next: FieldErrors = {};
    if (!validStoreCode(kind, code)) next.store_code = copy.errors.bad_store_code;
    if (!validStoreName(storeName)) next.store_name = copy.errors.bad_store_name;
    if (!validStoreAddress(address)) next.store_address = copy.errors.bad_store_address;
    if (Object.keys(next).length) {
      setErrors(next);
      return null;
    }
    const body = {
      cart_version: cartVersion,
      market_id: option.market_id,
      service_code: serviceCode(option),
      store_code: code,
      store_name: storeName,
      store_address: address,
    };
    if (!validBuyerStoreBody(body, kind)) {
      setErrors({ store: copy.invalidStore });
      return null;
    }
    const text = JSON.stringify(body);
    if (attempt.current?.body !== text) attempt.current = { key: crypto.randomUUID(), body: text };
    const response = await buyerRequest("POST", "cvs-stores", context, body, attempt.current.key);
    if (!response.ok) {
      const refused = await refusalCode(response);
      if (refused) {
        attempt.current = null;
        const field = errorField(refused);
        const slot =
          field === "store_code" || field === "store_name" || field === "store_address"
            ? field
            : "store";
        const shown: FieldErrors = {};
        shown[slot] = copy.errors[refused];
        setErrors(shown);
        return null;
      }
      throw new BuyerClientError("request_failed", response.status);
    }
    const data: unknown = await response.json();
    if (!validBuyerStore(data) || data.kind !== kind) throw new BuyerClientError("invalid_response");
    await assertPurchaseContext(context);
    attempt.current = null;
    setStore({
      pickup_id: data.pickup_id,
      kind,
      code: data.code,
      name: data.name,
      address: data.address,
      outside: false,
      source: "entered",
    });
    return data.pickup_id;
  }
  handle.current = { ensure };

  function edit(field: "code" | "name" | "address", value: string) {
    setEntered({ ...entered, [field]: value });
    setStore(null);
    setErrors({});
    changed.current();
  }
  function setRecipient(field: "recipient_name" | "phone", value: string) {
    onRecipient({ ...recipient, [field]: value });
    setErrors({});
  }
  async function copyLink() {
    try {
      await navigator.clipboard.writeText(window.location.href);
      setCopied("ok");
    } catch {
      setCopied("failed");
    }
  }
  const cardLabel = store?.source === "entered" ? copy.enteredLabel : copy.storeChosen;

  return (
    <>
      {(option.payment_modes ?? []).length > 1 && (
        <fieldset className="sku-options wide" data-testid="cvs-payment-mode">
          <legend>{copy.paymentLegend}</legend>
          {(option.payment_modes ?? []).map((mode) => (
            // .address-fields label (grid) would win over .sku-row, so the row layout is stated inline.
            <label
              key={mode}
              className={mode === paymentMode ? "sku-row selected" : "sku-row"}
              style={{ display: "flex", gap: 16 }}
            >
              <input
                type="radio"
                name="payment_mode"
                style={{ width: 20, height: 20, padding: 0, flex: "none" }} // .address-fields input is width:100%
                value={mode}
                checked={mode === paymentMode}
                onChange={() => {
                  onPaymentMode(mode);
                  setErrors({});
                }}
              />
              <span>{mode === "card" ? copy.payCard : copy.payAtPickup}</span>
            </label>
          ))}
          {payAtPickup && (
            <p className="order-note" data-testid="cvs-pay-note">
              {copy.payAtPickupTotal(total)} · {copy.payAtPickupNote}
            </p>
          )}
        </fieldset>
      )}
      <label>
        <span>{payAtPickup ? copy.recipientNameReal : copy.recipientName}</span>
        <input
          name="recipient_name"
          data-testid="cvs-recipient-name"
          type="text"
          autoComplete="name"
          maxLength={120}
          required
          value={recipient.recipient_name}
          onChange={(event) => setRecipient("recipient_name", event.target.value)}
        />
      </label>
      <label>
        <span>{payAtPickup ? copy.phoneReal : copy.phone}</span>
        <input
          name="phone"
          data-testid="cvs-recipient-phone"
          type="tel"
          inputMode="tel"
          autoComplete="tel"
          maxLength={32}
          required
          value={recipient.phone}
          onChange={(event) => setRecipient("phone", event.target.value)}
        />
      </label>
      {(payAtPickup || mapMode) && <p className="order-note wide">{copy.recipientRules}</p>}
      {errors.recipient && (
        <p role="alert" className="wide" data-testid="cvs-recipient-error">
          {errors.recipient}
        </p>
      )}

      {notice && (
        <div role={notice.key === "verifying" ? "status" : "alert"} className="wide" data-testid="cvs-notice">
          <p>
            {notice.key === "retry" ? copy.retryLater(notice.seconds) : copy[notice.key]}
          </p>
          {notice.key === "noSession" && (
            <>
              <button type="button" className="text-button" onClick={() => void copyLink()}>
                {copy.copyLink}
              </button>
              <span role="status">
                {copied === "ok" ? ` ${copy.copied}` : copied === "failed" ? ` ${copy.copyFailed}` : ""}
              </span>
            </>
          )}
          {(notice.key === "retry" || notice.key === "unavailable") && (
            <button
              type="button"
              className="text-button"
              disabled={notice.key === "retry" && !retryReady}
              onClick={() => {
                const id = cvsReturnID(window.location.search);
                if (id) void verify(id, live.current);
              }}
            >
              {copy.retry}
            </button>
          )}
        </div>
      )}
      {errors.store && (
        <p role="alert" className="wide" data-testid="cvs-store-error">
          {errors.store}
        </p>
      )}

      {store ? (
        <div className="wide" data-testid="cvs-store-card">
          <p>
            <strong>{cardLabel}</strong>
            {store.outside && (
              <>
                {" "}
                · <span data-testid="cvs-outside">{copy.outside}</span>
              </>
            )}
          </p>
          <p data-testid="cvs-store-name">
            {copy.chains[kind]} · {store.name}
          </p>
          <p>
            {copy.storeCode}: <span data-testid="cvs-store-code">{store.code}</span>
          </p>
          <p>
            {copy.storeAddress}: <span data-testid="cvs-store-address">{store.address}</span>
          </p>
          <p className="order-note" data-testid="cvs-consent">
            {copy.consent}
          </p>
          <button type="button" className="text-button" data-testid="cvs-change-store" onClick={changeStore}>
            {copy.changeStore}
          </button>
        </div>
      ) : mapMode ? (
        <div className="wide">
          <p className="order-note">{copy.mapNote}</p>
          <button
            type="button"
            className="primary"
            data-testid="cvs-pick-store"
            disabled={opening}
            onClick={() => void run(openMap)}
          >
            {opening ? copy.goingToMap : copy.pickStore}
          </button>
        </div>
      ) : (
        <>
          <p className="wide">{copy.enteredIntro}</p>
          <p className="wide">
            <a href={CVS_SEARCH_LINKS[kind]} target="_blank" rel="noopener noreferrer" data-testid="cvs-search-link">
              {copy.searchLink} ({copy.chains[kind]})
            </a>
          </p>
          <label>
            <span>
              {copy.enteredCode} · {copy.codeHint[kind]}
            </span>
            <input
              name="store_code"
              data-testid="cvs-entered-code"
              type="text"
              inputMode="numeric"
              autoComplete="off"
              maxLength={8}
              required
              aria-invalid={!!errors.store_code}
              value={entered.code}
              onChange={(event) => edit("code", event.target.value)}
            />
            {errors.store_code && <span role="alert">{errors.store_code}</span>}
          </label>
          <label>
            <span>{copy.enteredName}</span>
            <input
              name="store_name"
              data-testid="cvs-entered-name"
              type="text"
              autoComplete="off"
              maxLength={40}
              required
              aria-invalid={!!errors.store_name}
              value={entered.name}
              onChange={(event) => edit("name", event.target.value)}
            />
            {errors.store_name && <span role="alert">{errors.store_name}</span>}
          </label>
          <label className="wide">
            <span>{copy.enteredAddress}</span>
            <input
              name="store_address"
              data-testid="cvs-entered-address"
              type="text"
              autoComplete="off"
              maxLength={120}
              required
              aria-invalid={!!errors.store_address}
              value={entered.address}
              onChange={(event) => edit("address", event.target.value)}
            />
            {errors.store_address && <span role="alert">{errors.store_address}</span>}
          </label>
          <p className="order-note wide">{copy.enteredNote}</p>
        </>
      )}
    </>
  );
}

// Buyer order page block (§5.3, §16.4, §16.8): pay-at-pickup collection line and the CVS shipment progress.
// Reads only the order projection (BFF orders/{id} -> Go GET /v1/buyer/orders/{id}); never claims "delivered"
// before PICKED_UP, and REQUESTED/UNKNOWN/FAILED/ABANDONED all read "processing" (no error detail).
export function CvsOrderStatus({
  order,
  locale,
  money,
}: {
  order: Order;
  locale: Locale;
  money: (amount: number, currency: string) => string;
}) {
  const copy = cvsCopy[locale];
  const shipment = order.cvs_shipment ?? null;
  const collection = order.collection_state ?? null;
  if (!shipment && !collection) return null;
  const amount = money(order.snapshot.quote.amount.total_minor, order.snapshot.quote.currency);
  return (
    <section data-testid="order-cvs" aria-labelledby="cvs-order-title">
      <h2 id="cvs-order-title">{copy.pickupSection}</h2>
      {collection && (
        <p data-testid="order-collection" data-state={collection}>
          {collection === "PENDING" ? copy.collection.PENDING(amount) : copy.collection[collection]}
        </p>
      )}
      {collection === "PENDING" && <p className="order-note">{copy.payAtPickupOrderNote}</p>}
      {shipment && isCvsKind(shipment.chain) && (
        <>
          <p data-testid="order-cvs-state" data-state={shipment.state}>
            <strong>{copy.shipmentTitle}:</strong> {copy.shipmentStates[shipmentCopyKey(shipment.state)]}
          </p>
          <p>
            {copy.chains[shipment.chain]} · {shipment.store_name} · {shipment.store_code}
          </p>
          <p className="order-note">{copy.shipmentNote}</p>
        </>
      )}
    </section>
  );
}
