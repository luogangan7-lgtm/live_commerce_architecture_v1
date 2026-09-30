"use client";

// Checkout consent boxes (U7): two UNTICKED checkboxes (marketing DM; ads personalization with the purchase-events
// disclosure "already-sent events cannot be recalled") and the privacy-notice link carrying the notice version.
// Rendered by OrderFlow (integrator hook, <= 5 lines) inside the checkout form; `submitCheckoutConsents` runs after the
// order was placed: one `PUT consents` per ticked box, context "checkout".
// BFF PUT /api/buyer/consents (Idempotency-Key, body purpose/channel/granted/context) -> Go PUT /v1/buyer/consents
// (internal/buyerhttp/privacy.go; internal/customers.BuyerSetConsent). Go sets source and policy_version; the client
// only says "checkout". A consent failure never blocks or undoes the order (contract §5, U7): it is reported, not thrown.
import type { Locale } from "@live-commerce/i18n";
import { buyerRequest } from "../lib/buyer-client";
import { privacyCopy } from "../lib/privacy-copy";
import {
  consentBody,
  consentPairs,
  LC_PRIVACY_POLICY_VERSION,
  validConsentResult,
  type ConsentPurpose,
} from "../lib/privacy-contract";

export type ConsentChoicesValue = { marketing: boolean; ads: boolean };
export const noConsentChoices: ConsentChoicesValue = { marketing: false, ads: false };

export function ConsentChoices({
  locale,
  value,
  onChange,
  disabled = false,
}: {
  locale: Locale;
  value: ConsentChoicesValue;
  onChange: (next: ConsentChoicesValue) => void;
  disabled?: boolean;
}) {
  const copy = privacyCopy[locale];
  return (
    <fieldset className="consent-choices" data-testid="consent-choices" disabled={disabled}
      style={{ border: 0, padding: 0, margin: "24px 0 16px", minWidth: 0 }}>
      <legend>{copy.choicesLegend}</legend>
      <p className="order-note">{copy.choicesIntro}</p>
      <label style={{ display: "flex", gap: 8, alignItems: "flex-start", margin: "10px 0" }}>
        <input
          type="checkbox"
          data-testid="consent-marketing"
          checked={value.marketing}
          onChange={(event) => onChange({ ...value, marketing: event.target.checked })}
        />
        <span>{copy.marketing}</span>
      </label>
      <label style={{ display: "flex", gap: 8, alignItems: "flex-start", margin: "10px 0" }}>
        <input
          type="checkbox"
          data-testid="consent-ads"
          checked={value.ads}
          aria-describedby="consent-ads-disclosure"
          onChange={(event) => onChange({ ...value, ads: event.target.checked })}
        />
        <span>{copy.ads}</span>
      </label>
      <p id="consent-ads-disclosure" className="order-note" data-testid="consent-ads-disclosure">
        {copy.adsDisclosure}
      </p>
      <p className="order-note">
        <a
          href={`/${locale}/privacy?notice=${encodeURIComponent(LC_PRIVACY_POLICY_VERSION)}`}
          target="_blank"
          rel="noopener noreferrer"
          data-testid="consent-notice-link"
        >
          {copy.noticeLink}
        </a>{" "}
        ({copy.noticeVersion(LC_PRIVACY_POLICY_VERSION)})
      </p>
    </fieldset>
  );
}
export default ConsentChoices;

export type ConsentSubmitResult = Record<"marketing" | "ads", "skipped" | "saved" | "failed">;

// `orderContext` is the buyer session context OrderFlow already holds (X-Buyer-Context). One PUT per ticked box, each
// with its own key; sequential so a session change stops the rest. Never throws: the order already exists (U7).
export async function submitCheckoutConsents(orderContext: string, choices: ConsentChoicesValue): Promise<ConsentSubmitResult> {
  const result: ConsentSubmitResult = { marketing: "skipped", ads: "skipped" };
  const wanted: [keyof ConsentSubmitResult, ConsentPurpose][] = [
    ["marketing", "marketing_messages"],
    ["ads", "ads_personalization"],
  ];
  for (const [name, purpose] of wanted) {
    if (!choices[name]) continue;
    const pair = consentPairs.find((item) => item.purpose === purpose)!;
    try {
      const response = await buyerRequest(
        "PUT",
        "consents",
        orderContext,
        consentBody(pair.purpose, pair.channel, true, "checkout"),
        `consent-${crypto.randomUUID()}`,
      );
      const body: unknown = response.ok ? await response.json().catch(() => null) : null;
      result[name] = response.ok && validConsentResult(body) ? "saved" : "failed";
    } catch {
      // Uncertain or refused: no retry here (a duplicate consent row is harmless but a loop is not); the buyer can set it on /privacy.
      result[name] = "failed";
    }
  }
  return result;
}
