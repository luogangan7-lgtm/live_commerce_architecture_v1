"use client";

// Local extension of approved B: native address form after the real quotation;
// no new wizard, payment claim or persistent address cache. Transport/CAS and
// receipt recovery live in purchase.ts, not in this rendering component.
// OrderDetails hosts <OrderPayment> (BFF orders/{id}/payment[/prepare|handoff|refresh|cancel]);
// its Refresh order button also fires the Stripe payment/refresh signal via paymentSignalRef.
// The shipment block renders Go GET /v1/buyer/orders/{id} `shipment` (BFF orders/{id}); no route of its own.
// CVS options (taiwan-cvs-logistics-v1 §5, §16): the address form is replaced by <CvsPickup> (BFF cvs-selections,
// cvs-selections/{id}/verify, cvs-stores -> Go /v1/buyer/cvs-*), the destination is written with kind=cvs_* +
// pickup_id (BFF PUT destination -> Go SetDestination) and Begin carries payment_mode (BFF POST checkout ->
// Go checkout.Begin). A pay-at-pickup order takes no Stripe step: OrderPayment is not mounted for it.
import { useEffect, useRef, useState } from "react";
import OrderPayment from "./OrderPayment";
import { ConsentChoices, noConsentChoices, submitCheckoutConsents } from "./ConsentChoices";
import CvsPickup, { CvsOrderStatus, type PickupHandle } from "./CvsPickup";
import type { Locale } from "@live-commerce/i18n";
import { BuyerClientError } from "../lib/buyer-client";
import { carrierNames, orderCopy } from "../lib/order-copy";
import { purchaseCopy } from "../lib/purchase-copy";
import { cvsCopy } from "../lib/cvs-copy";
import {
  isCvsErrorCode,
  isCvsKind,
  type CvsDraft,
  type CvsErrorCode,
  type CvsKind,
  type PaymentMode,
} from "../lib/cvs-contract";
import {
  checkoutInput,
  currentDestination,
  pendingPurchase,
  purchasePage,
  readPurchase,
  validCart,
  validOptionRow,
  isUnavailable,
  validDestinationWrite,
  writeDestination,
  writeCheckout,
} from "../lib/purchase";
import type {
  Cart,
  Destination,
  DestinationWrite,
  HomeAddress,
  Option,
  Order,
  Quote,
  Shipment,
} from "../lib/purchase";

type Fields = HomeAddress & { recipient_name: string; phone: string };
const empty: Fields = {
  recipient_name: "",
  phone: "",
  region: "",
  city: "",
  postal_code: "",
  line1: "",
  line2: "",
};
const fieldsOf = (d: DestinationWrite | Destination): Fields => ({
  recipient_name: d.recipient_name,
  phone: d.phone,
  ...d.home_address,
});
type Run = (work: (isCurrent: () => boolean) => Promise<void>) => Promise<void>;
type Money = (amount: number, currency: string) => string;
const noHome: HomeAddress = { region: "", city: "", postal_code: "", line1: "", line2: "" };
const inputs = [
  ["recipient_name", "name", 120, true],
  ["phone", "tel", 32, true],
  ["region", "address-level1", 100, false],
  ["city", "address-level2", 100, true],
  ["postal_code", "postal-code", 20, false],
  ["line1", "address-line1", 200, true],
  ["line2", "address-line2", 200, false],
] as const;

export default function OrderFlow({
  context,
  cart,
  quote,
  recoveryCountry,
  locale,
  busy,
  blocked,
  recoveringDestination,
  run,
  reload,
  onOrder,
  money,
}: {
  context: string;
  cart: Cart;
  quote: Quote | null;
  recoveryCountry: string | null;
  locale: Locale;
  busy: boolean;
  blocked: boolean;
  recoveringDestination: boolean;
  run: Run;
  reload: () => Promise<void>;
  onOrder: (order: Order) => void;
  money: Money;
}) {
  const copy = orderCopy[locale];
  // A destination journal intentionally stores no PII or quotation. A fresh
  // tab must still offer explicit address recovery when it has no quote ID.
  const country = quote?.country ?? recoveryCountry ?? "";
  const [fields, setFields] = useState<Fields>(empty);
  const [head, setHead] = useState<Destination | null>(null);
  const [option, setOption] = useState<Option | null>(null);
  const [confirmed, setConfirmed] = useState<Destination | null>(null);
  const [consents, setConsents] = useState(noConsentChoices);
  const [notice, setNotice] = useState<
    "loading" | "recovered" | "failed" | "invalid" | "uncertain" | null
  >("loading");
  const [headLoaded, setHeadLoaded] = useState(false);
  const [expired, setExpired] = useState(
    !quote || Date.parse(quote.expires_at) <= Date.now(),
  );
  const live = useRef(0);
  const attempt = useRef<{ body: DestinationWrite; key: string } | null>(null);
  // CVS (§16): payment mode, the picker's ensure() handle and the one refusal shown next to the create button.
  const [paymentMode, setPaymentMode] = useState<PaymentMode>("card");
  const [createError, setCreateError] = useState<CvsErrorCode | null>(null);
  const pickup = useRef<PickupHandle | null>(null);
  const cvsOption = option && isCvsKind(option.delivery_kind) ? (option as Option & { delivery_kind: CvsKind }) : null;

  useEffect(() => {
    const version = ++live.current;
    void (async () => {
      try {
        const [current, currentCart] = await Promise.all([
          currentDestination(context),
          readPurchase("cart", context, validCart),
        ]);
        if (
          currentCart.id !== cart.id ||
          currentCart.version !== cart.version ||
          (quote &&
            (quote.cart_id !== cart.id || quote.cart_version !== cart.version))
        )
          throw new BuyerClientError("request_failed", 409);
        if (version !== live.current) return;
        setHead(current);
        setHeadLoaded(true);
        if (current?.kind === "home" && current.country === country) {
          setFields(fieldsOf(current));
          setNotice("recovered");
        } else setNotice(null);
        if (!quote) return;
        let found: Option | undefined,
          cursor = "";
        const seen = new Set<string>();
        do {
          const page = await purchasePage(
            `checkout-options?market_id=${quote.market_id}&country=${quote.country}&limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
            context,
            validOptionRow,
          );
          // A row the store lists but cannot sell yet ("coming soon") is never checkout-able here.
          found = page.items.find(
            (o): o is Option =>
              !isUnavailable(o) &&
              o.market_id === quote.market_id &&
              o.country === quote.country &&
              o.method === quote.method &&
              o.currency === quote.currency &&
              (o.delivery_kind !== "home" || o.mode === "MANUAL"),
          );
          cursor = page.next_cursor;
          if (cursor && (seen.has(cursor) || seen.size >= 100))
            throw new BuyerClientError("invalid_response");
          seen.add(cursor);
        } while (!found && cursor);
        if (!found) throw new BuyerClientError("request_failed");
        if (version !== live.current) return;
        setOption(found);
        // Options can arrive after the buyer edits or confirms a recovered
        // address. Never hydrate fields/head again from that older snapshot.
      } catch {
        if (version === live.current) setNotice("failed");
      }
    })();
    const timer = quote
      ? window.setTimeout(
          () => {
            setExpired(true);
            setConfirmed(null);
          },
          Math.max(
            0,
            Math.min(2_147_483_647, Date.parse(quote.expires_at) - Date.now()),
          ),
        )
      : undefined;
    return () => {
      live.current++;
      window.clearTimeout(timer);
      attempt.current = null;
    };
  }, [context, quote?.id, country, cart.id, cart.version]);

  useEffect(() => {
    let active = true;
    const recheck = async () => {
      setConfirmed(null);
      try {
        const latest = await currentDestination(context);
        if (active) setHead(latest);
      } catch {}
    };
    const changed = (event: StorageEvent) => {
      if (event.key === `commerce-purchase-pending-v1:${context}`)
        void recheck();
    };
    window.addEventListener("storage", changed);
    window.addEventListener("focus", recheck);
    return () => {
      active = false;
      window.removeEventListener("storage", changed);
      window.removeEventListener("focus", recheck);
    };
  }, [context]);

  async function confirm(isCurrent: () => boolean) {
    const version = live.current;
    const current = () => version === live.current && isCurrent();
    setConfirmed(null);
    const quoteExpired = !quote || Date.parse(quote.expires_at) <= Date.now();
    if (quoteExpired) {
      setExpired(true);
      if (!recoveringDestination) return;
    }
    const { recipient_name, phone, ...home_address } = fields;
    let body: DestinationWrite = {
      expected_version: head?.version ?? 0,
      cart_version: cart.version,
      kind: "home",
      country,
      recipient_name,
      phone,
      home_address,
    };
    if (cvsOption) {
      // §16.1: the store first (map selection verified / buyer-entered record), then the ordinary destination
      // write with kind=cvs_* + pickup_id. A null means the picker already showed what is missing.
      setCreateError(null);
      const pickupID = await pickup.current?.ensure();
      if (!pickupID || !current()) return;
      body = {
        expected_version: head?.version ?? 0,
        cart_version: cart.version,
        kind: cvsOption.delivery_kind,
        country,
        recipient_name: recipient_name.trim(),
        phone: phone.trim(),
        home_address: { ...noHome },
        pickup_id: pickupID,
      };
    }
    if (!validDestinationWrite(body)) {
      setNotice("invalid");
      return;
    }
    const pending = pendingPurchase(context);
    let replace: string | undefined;
    if (pending?.kind === "destination") {
      if (
        attempt.current?.key === pending.key &&
        JSON.stringify(fieldsOf(attempt.current.body)) ===
          JSON.stringify(fieldsOf(body)) &&
        attempt.current.body.pickup_id === body.pickup_id
      )
        body = attempt.current.body;
      else replace = pending.key; // Explicit reconfirmation of the displayed head, never silent replay of lost PII.
    }
    try {
      const saved = await writeDestination(context, body, replace);
      if (!current()) return;
      setHead(saved);
      setConfirmed(
        !quote || Date.parse(quote.expires_at) <= Date.now() ? null : saved,
      );
      setNotice(null);
      attempt.current = null;
    } catch (reason) {
      if (current()) {
        const waiting = pendingPurchase(context);
        if (waiting?.kind === "destination")
          attempt.current = { key: waiting.key, body };
        setNotice("uncertain");
        // Re-read the observed CAS head, but never replace typed fields or mark
        // it confirmed. A new intent requires the buyer's next explicit click.
        try {
          const latest = await currentDestination(context);
          if (current()) setHead(latest);
        } catch {}
      }
      throw reason;
    }
  }

  return (
    <section
      className="address-section"
      data-testid="address-section"
      aria-labelledby="address-title"
    >
      <h2 id="address-title">{cvsOption ? cvsCopy[locale].title : copy.address}</h2>
      <p>{quote ? copy.explain : copy.recoverWithoutQuote}</p>
      <p className="address-total">
        {quote && (
          <>
            {purchaseCopy[locale].total}:{" "}
            <strong>{money(quote.amount.total_minor, quote.currency)}</strong>{" "}
            ·{" "}
          </>
        )}
        {copy.country}: {country}
      </p>
      {notice && (
        <p
          role={
            notice === "failed" || notice === "invalid" ? "alert" : "status"
          }
        >
          {copy[notice]}
        </p>
      )}
      {quote && expired && <p role="alert">{copy.expired}</p>}
      {(notice === "failed" || expired) && (
        <button
          className="text-button"
          disabled={busy || blocked || recoveringDestination}
          onClick={() => void run(reload)}
        >
          {copy.reload}
        </button>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void run(confirm);
        }}
      >
        <fieldset
          disabled={
            busy ||
            blocked ||
            !headLoaded ||
            (!recoveringDestination && (!option || expired))
          }
          className="address-fields"
        >
          <legend className="sr-only">
            {cvsOption ? cvsCopy[locale].title : copy.address}
          </legend>
          {cvsOption && (
            <CvsPickup
              context={context}
              locale={locale}
              option={cvsOption}
              cartVersion={cart.version}
              recipient={{ recipient_name: fields.recipient_name, phone: fields.phone }}
              onRecipient={(next) => {
                setFields({ ...fields, ...next });
                setConfirmed(null);
                if (notice === "invalid") setNotice(null);
              }}
              paymentMode={paymentMode}
              onPaymentMode={(mode) => {
                setPaymentMode(mode);
                setConfirmed(null);
                setCreateError(null);
              }}
              onRestore={(draft: CvsDraft) => {
                setFields((old) => ({
                  ...old,
                  recipient_name: draft.recipient_name,
                  phone: draft.phone,
                }));
                if (cvsOption.payment_modes?.includes(draft.payment_mode))
                  setPaymentMode(draft.payment_mode);
              }}
              onStoreChange={() => setConfirmed(null)}
              run={run}
              handle={pickup}
              total={quote ? money(quote.amount.total_minor, quote.currency) : ""}
            />
          )}
          {!cvsOption && inputs.map(([name, autoComplete, maxLength, required]) => (
            <label
              key={name}
              className={
                name === "line1" || name === "line2" ? "wide" : undefined
              }
            >
              <span>{copy[name]}</span>
              <input
                name={name}
                type={name === "phone" ? "tel" : "text"}
                autoComplete={autoComplete}
                maxLength={maxLength}
                required={required}
                value={fields[name]}
                onChange={(event) => {
                  setFields({ ...fields, [name]: event.target.value });
                  setConfirmed(null);
                  if (notice === "invalid") setNotice(null);
                }}
              />
            </label>
          ))}
          <button
            type="submit"
            data-testid="confirm-address"
            className="address-confirm"
          >
            {expired && recoveringDestination
              ? copy.recoverAddress
              : cvsOption
                ? cvsCopy[locale].confirmPickup
                : copy.confirm}
          </button>
        </fieldset>
      </form>
      {confirmed && (
        <p role="status" className="address-confirmed">
          {cvsOption ? cvsCopy[locale].confirmedPickup : copy.confirmed}
        </p>
      )}
      {createError && (
        <p role="alert" data-testid="cvs-create-error">
          {cvsCopy[locale].errors[createError]}
        </p>
      )}
      <ConsentChoices locale={locale} value={consents} onChange={setConsents} disabled={busy || blocked} />
      <button
        data-testid="create-order"
        className="primary create-order"
        disabled={busy || blocked || expired || !confirmed || !option}
        onClick={() =>
          void run(async (isCurrent) => {
            if (!quote || !confirmed || !option) return;
            const version = live.current;
            try {
              const result = await writeCheckout(
                context,
                checkoutInput(
                  quote,
                  option,
                  cart,
                  confirmed,
                  Date.now(),
                  cvsOption ? paymentMode : undefined,
                ),
              );
              void submitCheckoutConsents(context, consents); // never blocks the order (customers-billing-v1 U7)
              if (isCurrent() && version === live.current) onOrder(result);
            } catch (reason) {
              if (version === live.current) setConfirmed(null);
              // A definite CVS/pay-at-pickup refusal (§16.2) is shown here, not as a generic failure.
              if (
                cvsOption &&
                reason instanceof BuyerClientError &&
                isCvsErrorCode(reason.detail)
              ) {
                if (version === live.current) setCreateError(reason.detail);
                return;
              }
              throw reason;
            }
          })
        }
      >
        {cvsOption && paymentMode === "pay_at_pickup"
          ? cvsCopy[locale].createPickup
          : copy.create}
      </button>
      <p className="order-note">
        {cvsOption && paymentMode === "pay_at_pickup"
          ? cvsCopy[locale].payAtPickupNote
          : copy.unavailable}
      </p>
    </section>
  );
}

// The seller's attestation of dispatch (manual-fulfilment-v1 §5.2): never "in transit"/"delivered".
// Link: plain external anchor, host shown so the buyer sees where it goes (ruling 14, Q6);
// rel="noopener noreferrer nofollow": ruling 25 (manual-fulfilment-v1 §3.2 governs; ruling 14 was incomplete).
function ShipmentBlock({
  shipment,
  locale,
}: {
  shipment: Shipment;
  locale: Locale;
}) {
  const copy = orderCopy[locale];
  const [copied, setCopied] = useState<"" | "ok" | "failed">("");
  const link = shipment.tracking_url;
  // Validated https by validOrder (validTrackingURL) before it reaches an href.
  const host = link ? new URL(link).hostname : "";
  async function copyNumber() {
    try {
      await navigator.clipboard.writeText(shipment.tracking_number);
      setCopied("ok");
    } catch {
      setCopied("failed");
    }
  }
  return (
    <section data-testid="order-shipment" aria-labelledby="shipment-title">
      <h2 id="shipment-title" data-testid="shipment-title">
        {copy.shipped}
      </h2>
      <p data-testid="shipment-carrier">
        {copy.carrier}:{" "}
        {shipment.carrier_name ?? carrierNames[locale][shipment.carrier_code]}
      </p>
      <p>
        {copy.tracking}:{" "}
        <span data-testid="shipment-tracking" className="order-id">
          {shipment.tracking_number}
        </span>{" "}
        <button
          type="button"
          data-testid="copy-tracking"
          onClick={() => void copyNumber()}
        >
          {copy.copyTracking}
        </button>
      </p>
      <p role="status" data-testid="copy-status">
        {copied === "ok" ? copy.copied : copied === "failed" ? copy.copyFailed : ""}
      </p>
      {link && (
        <p>
          <a
            data-testid="shipment-link"
            href={link}
            target="_blank"
            rel="noopener noreferrer nofollow"
          >
            {copy.trackLink}
          </a>{" "}
          <span data-testid="shipment-host">({host})</span>
        </p>
      )}
      <p className="order-note">{copy.shipNote}</p>
    </section>
  );
}

export function OrderDetails({
  context,
  order,
  locale,
  money,
  refresh,
  busy,
  onPaymentBusy,
  isSelected,
}: {
  context: string;
  order: Order;
  locale: Locale;
  money: Money;
  refresh: () => void;
  busy: boolean;
  onPaymentBusy: (busy: boolean) => void;
  isSelected: () => boolean;
}) {
  const [paymentRefresh, setPaymentRefresh] = useState(0);
  // OrderPayment registers here only while a Stripe attempt is live (payment/refresh signal).
  const paymentSignalRef = useRef<(() => Promise<void>) | null>(null);
  const copy = orderCopy[locale],
    common = purchaseCopy[locale],
    destination = order.snapshot.destination;
  return (
    <section
      className="order-section"
      data-testid="order-section"
      aria-labelledby="order-title"
    >
      <h1 id="order-title">{copy.order}</h1>
      <p
        className="order-state"
        data-testid="order-state"
        data-state={order.commercial_state}
      >
        {copy[order.commercial_state]}
      </p>
      <p>
        {copy.orderID}:{" "}
        <span data-testid="order-id" className="order-id">
          {order.order_id}
        </span>
      </p>
      <ul className="order-lines">
        {order.snapshot.quote.lines.map((line) => (
          <li key={line.sku_id}>
            <span>
              {line.name} · {line.code} × {line.quantity}
            </span>
            <span>
              {money(
                line.unit_price_minor * line.quantity,
                order.snapshot.quote.currency,
              )}
            </span>
          </li>
        ))}
      </ul>
      <dl className="order-breakdown" data-testid="order-breakdown">
        {(
          [
            [common.shipping, "shipping_minor"],
            [common.taxes, "tax_minor"],
            [common.discount, "discount_minor"],
          ] as const
        ).map(([label, key]) => (
          <div key={key}>
            <dt>{label}</dt>
            <dd>
              {money(
                order.snapshot.quote.amount[key],
                order.snapshot.quote.currency,
              )}
            </dd>
          </div>
        ))}
      </dl>
      <p className="order-total">
        {common.total}{" "}
        <strong>
          {money(
            order.snapshot.quote.amount.total_minor,
            order.snapshot.quote.currency,
          )}
        </strong>
      </p>
      <h2>{copy.address}</h2>
      <address>
        {destination.recipient_name}
        <br />
        {destination.phone}
        <br />
        {[
          destination.home_address.region,
          destination.home_address.city,
          destination.home_address.postal_code,
          destination.home_address.line1,
          destination.home_address.line2,
        ]
          .filter(Boolean)
          .join(" · ")}
        {destination.pickup &&
          `${destination.pickup.name} · ${destination.pickup.code} · ${destination.pickup.address}`}
        {destination.pickup?.verification_kind === "BUYER_ENTERED" && (
          <>
            <br />
            <span data-testid="order-pickup-entered">{cvsCopy[locale].enteredLabel}</span>
          </>
        )}
        <br />
        {destination.country}
      </address>
      <CvsOrderStatus order={order} locale={locale} money={money} />
      {order.shipment && (
        <ShipmentBlock shipment={order.shipment} locale={locale} />
      )}
      {order.hold_expires_at && (
        <>
          <p>
            {copy.hold}{" "}
            {new Intl.DateTimeFormat(locale, {
              dateStyle: "short",
              timeStyle: "short",
            }).format(new Date(order.hold_expires_at))}
          </p>
          <p className="order-note">{copy.holdNote}</p>
        </>
      )}
      {/* Pay-at-pickup is not a Stripe payment (§16.2): no payment read, no start, no refresh signal. */}
      {order.payment_mode !== "pay_at_pickup" && (
        <OrderPayment
          key={`${context}:${order.order_id}`}
          context={context}
          order={order}
          locale={locale}
          money={money}
          busy={busy}
          refreshToken={paymentRefresh}
          onBusy={onPaymentBusy}
          isSelected={isSelected}
          paymentSignalRef={paymentSignalRef}
        />
      )}
      <button
        data-testid="refresh-order"
        disabled={busy}
        onClick={async () => {
          // Errors are swallowed inside the handler (shown in the payment section).
          const signal = paymentSignalRef.current;
          if (signal) await signal();
          setPaymentRefresh((v) => v + 1);
          refresh();
        }}
      >
        {copy.refresh}
      </button>
    </section>
  );
}
