"use client";

// Inline payment section. BFF routes used (via lib/order-payment.ts -> lib/buyer-client.ts):
//   GET  /api/buyer/orders/{id}/payment          -> Go GET /v1/buyer/orders/{id}/payment
//   POST .../payment/prepare | handoff           -> BeginHosted / TakeHosted (PAYUNi) or Stripe start/take_stripe_handoff
//   POST .../payment/refresh | cancel (Stripe)   -> HostedPaymentStarter.RefreshPayment / CancelPayment
// Stripe states and polling: contracts/stripe-buyer-ui-v1.md §5-§6. PAYUNi rendering is unchanged.
// Refund summary (stripe-refund-v1 §7.2) is read from the same payment view; no refund route is called.
import { useEffect, useRef, useState, type MutableRefObject } from "react";
import type { Locale } from "@live-commerce/i18n";
import type { Order } from "../lib/purchase";
import {
  isStripeView,
  type OrderPayment as PaymentView,
} from "../lib/payment-contract";
import {
  openPaymentDestination,
  payOrder,
  pendingOrderPayment,
  readOrderPayment,
  requestPaymentSignal,
} from "../lib/order-payment";
import { paymentCopy } from "../lib/payment-copy";
import { orderCopy } from "../lib/order-copy";

type Message =
  "failed" | "blocked" | "uncertain" | "submitted" | "creating" | "opFailed";
type Plan = {
  pay: boolean;
  cont: boolean;
  cancel: boolean;
  note: "explain" | "cutoff" | "readOnly" | "cancelling" | "unavailable" | "";
};

// stripe-buyer-ui-v1 §5 terminal set; also the polling/refresh stop condition (§6).
const terminalPayment = (view: PaymentView) =>
  view.payment_state === "CAPTURED" ||
  view.payment_state === "PARTIALLY_REFUNDED" ||
  view.payment_state === "REFUNDED" ||
  view.payment_state === "CLOSED_UNPAID" ||
  view.payment_state === "REVIEW_REQUIRED";
// A prepared Stripe attempt that may still change: the only thing worth polling or refreshing.
const activeAttempt = (view: PaymentView) =>
  view.cancel_requested !== undefined && !terminalPayment(view);
const POLL_STEPS_MS = [5000, 10000, 20000, 30000];
const POLL_WINDOW_MS = 10 * 60 * 1000;

// The §5 state table. Cutoff is only a hint (Go enforces it); UNAVAILABLE is what Go returns past it.
function stripePlan(view: PaymentView, now: number): Plan {
  const none: Plan = { pay: false, cont: false, cancel: false, note: "" };
  const fresh =
    view.payment_state === "NOT_STARTED" &&
    view.commercial_state === "DRAFT" &&
    view.handoff_state === "NONE";
  if (fresh)
    return view.methods.length === 1 &&
      view.methods[0].code === "stripe_checkout"
      ? { ...none, pay: true, note: "explain" }
      : { ...none, note: "unavailable" };
  if (
    terminalPayment(view) ||
    view.commercial_state === "CONFIRMED" ||
    view.commercial_state === "CANCELLED" ||
    view.handoff_state === "CLOSED"
  )
    return none;
  if (view.cancel_requested) return { ...none, note: "cancelling" };
  if (
    view.payment_state !== "PENDING" ||
    view.commercial_state !== "AWAITING_PAYMENT"
  )
    return none;
  const past =
    view.handoff_expires_at !== null &&
    now >= Date.parse(view.handoff_expires_at);
  if (view.handoff_state === "CREATING" || view.handoff_state === "READY")
    return past
      ? { ...none, cancel: true, note: "cutoff" }
      : { ...none, cont: true, cancel: true, note: "explain" };
  if (view.handoff_state === "UNAVAILABLE")
    return { ...none, cancel: true, note: past ? "cutoff" : "readOnly" };
  return none;
}

// Local extension of approved B: one inline payment section, no second checkout
// or generic retry path. The controller owns durable intent and one-shot Take;
// this component owns only the selected-screen lifetime and visible read state.
export default function OrderPayment({
  context,
  order,
  locale,
  money,
  busy,
  refreshToken,
  onBusy,
  isSelected,
  paymentSignalRef,
}: {
  context: string;
  order: Order;
  locale: Locale;
  money: (amount: number, currency: string) => string;
  busy: boolean;
  refreshToken: number;
  onBusy: (busy: boolean) => void;
  isSelected: () => boolean;
  // The parent's Refresh order button awaits this while a Stripe attempt is live (§5 Refresh).
  paymentSignalRef: MutableRefObject<(() => Promise<void>) | null>;
}) {
  const copy = paymentCopy[locale];
  const [view, setView] = useState<PaymentView | null>(null);
  const [marker, setMarker] =
    useState<ReturnType<typeof pendingOrderPayment>>(null);
  const [reading, setReading] = useState(true);
  const [sending, setSending] = useState(false);
  const [message, setMessage] = useState<Message | null>(null);
  const [signalling, setSignalling] = useState(false);
  const epoch = useRef(0);
  const working = useRef(false);
  const readVersion = useRef(0);
  // Non-quiet reads own `reading`; a quiet poll read bumps readVersion but must never
  // strand the flag, so the flag is fenced by its own counter (latest non-quiet read).
  const loudVersion = useRef(0);
  const loudActive = useRef(false);
  const busyCallback = useRef(onBusy);
  busyCallback.current = onBusy;
  const selection = useRef(isSelected);
  selection.current = isSelected;
  const latest = useRef<PaymentView | null>(null);
  const restartPoll = useRef<() => void>(() => {});
  const keepFailure = useRef(false);
  const statusLine = useRef<HTMLParagraphElement>(null);
  const hadAction = useRef(false);
  const viewKey = useRef("");

  // quiet = background poll: no loading flicker, and a failed GET keeps the last view.
  async function read(version: number, quiet = false) {
    const request = ++readVersion.current;
    const loud = quiet ? loudVersion.current : ++loudVersion.current;
    if (!quiet) {
      loudActive.current = true;
      setReading(true);
    }
    try {
      const value = await readOrderPayment(context, order.order_id);
      if (version !== epoch.current || request !== readVersion.current) return;
      if (
        value.currency !== order.snapshot.quote.currency ||
        value.total_minor !== order.snapshot.quote.amount.total_minor
      )
        throw new Error("snapshot");
      const pending = pendingOrderPayment(context, order.order_id);
      latest.current = value;
      setView(value);
      setMarker(pending);
      setMessage((old) => (old === "failed" ? null : old));
    } catch {
      if (
        !quiet &&
        version === epoch.current &&
        request === readVersion.current
      ) {
        latest.current = null;
        setView(null);
        setMessage("failed");
      }
    } finally {
      // Clear when this is the latest non-quiet read, even if a quiet read superseded
      // its result (else reading stays true and Continue/Cancel/Pay stay blocked).
      if (!quiet && version === epoch.current && loud === loudVersion.current) {
        loudActive.current = false;
        setReading(false);
      }
    }
  }

  useEffect(() => {
    const version = ++epoch.current;
    // §8: a Refresh (refreshToken only) keeps the last view so an unchanged state is not
    // unmounted and re-announced; a different order/context must never show the old one.
    const key = `${context}:${order.order_id}`;
    if (viewKey.current !== key) {
      viewKey.current = key;
      latest.current = null;
      setView(null);
      setMarker(null);
    }
    // A failed Refresh-order signal must survive the remount that click causes.
    const keep = keepFailure.current;
    keepFailure.current = false;
    if (!keep) setMessage(null);
    void read(version).then(() => restart());
    // Stripe poll (§6): ONE setTimeout chain owned by this epoch, GET-only. It stops 10 min
    // after the last restart (mount, Pay/Continue/Refresh/Cancel, focus, pageshow, visible),
    // on a terminal state, when hidden, or at cleanup. It never POSTs; a click-driven
    // READY wait (working.current) pauses it.
    let timer: ReturnType<typeof setTimeout> | undefined;
    let step = 0;
    let started = Date.now();
    const schedule = () => {
      clearTimeout(timer);
      timer = undefined;
      const current = latest.current;
      if (
        version !== epoch.current ||
        !current ||
        !activeAttempt(current) ||
        document.visibilityState === "hidden" ||
        Date.now() - started >= POLL_WINDOW_MS
      )
        return;
      timer = setTimeout(
        async () => {
          timer = undefined;
          if (version !== epoch.current) return;
          // Skip while a non-quiet read is in flight: a quiet read would supersede it and
          // discard its result (or its failure message) on the first load.
          if (
            !working.current &&
            !loudActive.current &&
            document.visibilityState !== "hidden"
          )
            await read(version, true);
          step++;
          schedule();
        },
        POLL_STEPS_MS[Math.min(step, POLL_STEPS_MS.length - 1)],
      );
    };
    const restart = () => {
      step = 0;
      started = Date.now();
      schedule();
    };
    restartPoll.current = restart;
    const refresh = () => {
      if (!working.current) void read(version).then(() => restart());
    };
    const visible = () => {
      if (document.visibilityState === "visible") restart();
    };
    // Returning from the provider can only GET. No mutation belongs to a focus,
    // pageshow, storage event or effect (including React StrictMode remount).
    window.addEventListener("focus", refresh);
    window.addEventListener("pageshow", refresh);
    document.addEventListener("visibilitychange", visible);
    const changed = (event: StorageEvent) => {
      if (
        event.key === null ||
        event.key === `commerce-order-payment-v1:${context}:${order.order_id}`
      )
        refresh();
    };
    window.addEventListener("storage", changed);
    return () => {
      epoch.current++;
      clearTimeout(timer);
      restartPoll.current = () => {};
      window.removeEventListener("focus", refresh);
      window.removeEventListener("pageshow", refresh);
      window.removeEventListener("storage", changed);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [context, order, refreshToken]);

  const stripeMarker = marker?.body.method_code === "stripe_checkout";
  const plan =
    view && (isStripeView(view) || stripeMarker)
      ? stripePlan(view, Date.now())
      : null;
  const attempt = !!view && activeAttempt(view);

  // Refresh order's extra signal: registered only while a non-terminal Stripe attempt is
  // live and cleared with the epoch. A click is the only trigger; scheduled=false is silent.
  useEffect(() => {
    if (!attempt) return;
    const version = epoch.current;
    const signal = async () => {
      try {
        await requestPaymentSignal(context, order.order_id, "refresh");
      } catch {
        if (version === epoch.current) {
          keepFailure.current = true;
          setMessage("opFailed");
        }
      }
      restartPoll.current();
    };
    paymentSignalRef.current = signal;
    return () => {
      if (paymentSignalRef.current === signal) paymentSignalRef.current = null;
    };
  }, [attempt, context, order.order_id, paymentSignalRef, refreshToken]);

  // §8: when Continue/Cancel disappears, focus the status line instead of dropping it on <body>.
  const hasAction = !!plan && (plan.cont || plan.cancel);
  useEffect(() => {
    if (hadAction.current && !hasAction) {
      const active = document.activeElement;
      if (
        !active ||
        active === document.body ||
        statusLine.current?.closest("section")?.contains(active)
      )
        statusLine.current?.focus();
    }
    hadAction.current = hasAction;
  }, [hasAction]);

  // PAYUNi gate (unchanged); Stripe views use `plan` instead.
  const fresh =
    !marker &&
    view?.payment_state === "NOT_STARTED" &&
    view.commercial_state === "DRAFT" &&
    view.handoff_state === "NONE" &&
    view.methods.length === 1;
  const recover =
    marker?.stage === "prepare" &&
    marker.body.method_code === "payuni_credit" &&
    view &&
    ((view.payment_state === "NOT_STARTED" &&
      view.commercial_state === "DRAFT" &&
      view.handoff_state === "NONE") ||
      (view.payment_state === "PENDING" &&
        view.commercial_state === "AWAITING_PAYMENT" &&
        view.handoff_state === "PREPARED"));
  const available = plan ? plan.pay || plan.cont : fresh || recover;

  async function pay() {
    if (working.current || busy || reading || signalling || !available) return;
    working.current = true;
    setSending(true);
    busyCallback.current(true);
    setMessage(null);
    const version = epoch.current;
    const selectedAtStart = selection.current;
    let destination: ReturnType<typeof openPaymentDestination> | undefined;
    const stripe = !!plan;
    // One handoff POST per click: payOrder is reached only from this click handler, never from
    // an effect, poll, focus or retry, so a double click cannot issue two sessions.
    try {
      // Must run in the click's activation BEFORE the first await.
      destination = openPaymentDestination(locale);
      const outcome = await payOrder({
        context,
        order,
        locale,
        method: view?.methods[0],
        destination,
        isCurrent: () => epoch.current === version && selectedAtStart(),
      });
      // PAYUNi resolves void => "submitted" as before; Stripe reports its own outcome.
      if (epoch.current === version)
        setMessage(
          outcome === "creating"
            ? "creating"
            : outcome === "aborted"
              ? null
              : "submitted",
        );
    } catch {
      destination?.close();
      // Stripe never shows the PAYUNi-only "uncertain" copy (a Stripe page may be reopened).
      if (epoch.current === version)
        setMessage(
          destination ? (stripe ? "opFailed" : "uncertain") : "blocked",
        );
    } finally {
      working.current = false;
      busyCallback.current(false);
      if (epoch.current === version) {
        setSending(false);
        await read(version);
        restartPoll.current();
      }
    }
  }

  // Cancel: native confirm, exactly one POST, no retry. Go's effect is expire-then-close
  // after Stripe confirms, so success only means "requested"; scheduled=false is fine.
  async function cancel() {
    if (working.current || busy || reading || sending || signalling) return;
    if (!plan?.cancel || !window.confirm(copy.cancelConfirm)) return;
    working.current = true;
    setSignalling(true);
    busyCallback.current(true);
    setMessage(null);
    const version = epoch.current;
    try {
      await requestPaymentSignal(context, order.order_id, "cancel");
    } catch {
      if (epoch.current === version) setMessage("opFailed");
    } finally {
      working.current = false;
      busyCallback.current(false);
      if (epoch.current === version) {
        setSignalling(false);
        await read(version);
        restartPoll.current();
      }
    }
  }

  return (
    <section
      className="order-payment"
      data-testid="order-payment"
      aria-labelledby="payment-title"
      aria-busy={reading || sending || signalling}
    >
      <h2 id="payment-title">{copy.title}</h2>
      {view && (
        <>
          {view.test_mode && (
            <p className="payment-test-mode" data-testid="payment-test-mode">
              {copy.test}
            </p>
          )}
          <p
            className="payment-state"
            data-testid="payment-status"
            data-state={view.payment_state}
            {...(plan ? { role: "status", tabIndex: -1, ref: statusLine } : {})}
          >
            {copy.paymentState}: {copy[view.payment_state]}
          </p>
          <p className="order-note" data-testid="payment-commercial-status">
            {copy.orderState}: {orderCopy[locale][view.commercial_state]}
          </p>
          {view.refund && view.refund.pending_minor > 0 && (
            <p role="status" data-testid="refund-processing">
              {copy.refundProcessing}
            </p>
          )}
          {view.refund && view.refund.refunded_minor > 0 && (
            <p data-testid="refund-succeeded">
              {copy.refunded.replace(
                "{amount}",
                money(view.refund.refunded_minor, view.currency),
              )}
            </p>
          )}
        </>
      )}
      {message &&
        // Stripe only: a stale "handoff requested" must not sit beside a final payment state.
        !(
          plan &&
          view &&
          terminalPayment(view) &&
          (message === "submitted" || message === "creating")
        ) && (
          <p
            role={
              message === "submitted" || message === "creating"
                ? "status"
                : "alert"
            }
            data-testid="payment-error"
          >
            {copy[message === "opFailed" ? "failed" : message]}
          </p>
        )}
      {sending ? (
        <p role="status">{copy.sending}</p>
      ) : reading ? (
        // §8: reads are silent (aria-busy on the section covers them). One user action (Cancel,
        // Refresh) legitimately runs several GETs (focus after the native confirm + the remount
        // read); a role=status here re-announced the same loading text for each of them.
        <p>{copy.loading}</p>
      ) : plan ? (
        <>
          {plan.pay && view && (
            <p>
              {
                view.methods[0][
                  locale === "en"
                    ? "name_en"
                    : locale === "zh-TW"
                      ? "name_hant"
                      : "name_hans"
                ]
              }
            </p>
          )}
          {plan.note && <p className="order-note">{copy[plan.note]}</p>}
          {(plan.pay || plan.cont) && (
            <button
              className="payment-pay"
              data-testid="pay-order"
              disabled={busy || signalling}
              onClick={() => void pay()}
            >
              {plan.cont ? copy.recover : copy.pay}
            </button>
          )}
          {plan.cancel && (
            <button
              data-testid="cancel-payment"
              disabled={busy || signalling}
              onClick={() => void cancel()}
            >
              {copy.cancel}
            </button>
          )}
        </>
      ) : available ? (
        <>
          {fresh && view && (
            <p>
              {
                view.methods[0][
                  locale === "en"
                    ? "name_en"
                    : locale === "zh-TW"
                      ? "name_hant"
                      : "name_hans"
                ]
              }
            </p>
          )}
          <p className="order-note">{copy.explain}</p>
          <button
            className="payment-pay"
            data-testid="pay-order"
            disabled={busy}
            onClick={() => void pay()}
          >
            {recover ? copy.recover : copy.pay}
          </button>
        </>
      ) : (
        view && (
          <p className="order-note">
            {view.payment_state === "NOT_STARTED" && !marker
              ? copy.unavailable
              : view.payment_state === "PENDING"
                ? copy.readOnly
                : ""}
          </p>
        )
      )}
    </section>
  );
}
