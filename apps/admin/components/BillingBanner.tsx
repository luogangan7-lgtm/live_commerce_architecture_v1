"use client";

// Standing banner (U5): one shape reused on every workspace page, mounted by the integrator inside WorkspaceFrame.
// BFF GET /api/stores/{store}/billing/standing (store:read, every member) -> Go GET /v1/admin/stores/{id}/billing/standing
// (internal/httpapi/billing.go; identity.read_billing_standing). Without a `store` query it resolves the same default
// store the pages use (first by id) through GET /api/stores. GRACE = neutral warning, RESTRICTED = warning; GOOD and
// UNBILLED show nothing. Any failure shows nothing: the banner is advice, never a gate (server enforces standing, BD4).
import { useEffect, useState } from "react";
import Link from "next/link";
import type { Locale } from "@live-commerce/i18n";
import { billingCopy } from "@/lib/billing-copy";
import { readStanding } from "@/lib/billing-client";
import { bannerKind, type BannerKind } from "@/lib/billing-model";
import "./orders.css";
import "./order-actions.css";
import "./customers.css";

const canonical = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

async function defaultStore(signal: AbortSignal): Promise<string | null> {
  const response = await fetch("/api/stores", { credentials: "same-origin", cache: "no-store", signal });
  if (!response.ok) return null;
  const body: unknown = await response.json().catch(() => null);
  const items = body && typeof body === "object" ? (body as { items?: unknown }).items : null;
  if (!Array.isArray(items)) return null;
  const ids = items
    .map((item) => (item && typeof item === "object" ? (item as { id?: unknown }).id : null))
    .filter((id): id is string => typeof id === "string" && canonical.test(id))
    .sort((a, b) => a.localeCompare(b));
  return ids[0] ?? null;
}

export function BillingBanner({ locale, storeId }: { locale: Locale; storeId: string | null }) {
  const c = billingCopy[locale];
  const [shown, setShown] = useState<{ store: string; kind: BannerKind } | null>(null);
  useEffect(() => {
    const active = new AbortController();
    const load = async () => {
      try {
        const store = storeId && canonical.test(storeId) ? storeId : await defaultStore(active.signal);
        if (!store || active.signal.aborted) return;
        const standing = await readStanding(store, active.signal);
        if (!active.signal.aborted) setShown({ store, kind: bannerKind(standing) });
      } catch {
        if (!active.signal.aborted) setShown(null);
      }
    };
    void load();
    // bfcache restore or returning from Stripe: standing may have changed, and a hidden tab keeps nothing sensitive here.
    const again = () => void load();
    window.addEventListener("pageshow", again);
    return () => {
      active.abort();
      window.removeEventListener("pageshow", again);
    };
  }, [storeId]);
  if (!shown?.kind) return null;
  return (
    <div
      className={`billing-banner ${shown.kind === "restricted" ? "orders-tone-warning" : "orders-tone-neutral"}`}
      role="note"
      aria-label={c.bannerLabel}
      data-testid="billing-banner"
      data-standing={shown.kind}
    >
      <span>{shown.kind === "restricted" ? c.bannerRestricted : c.bannerGrace}</span>
      <Link href={`/${locale}/billing?store=${shown.store}`} data-testid="billing-banner-link">{c.bannerLink}</Link>
    </div>
  );
}
