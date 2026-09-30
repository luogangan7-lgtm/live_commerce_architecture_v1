// Owns the /{locale}/studio/claims?store=&scene= route: server-side locale/query
// validation and signed-session store resolution for Studio › Claims (T10b).
// Non-goals: no claims data on the server (the client reads M1–M7 through the BFF after
// hydration, so nothing claim-related is rendered into cached HTML), no store fallback
// for an unknown requested store (404), no fixture authority.
// Depends on: lib/auth.ts (authConfig, authenticatedStores, cookie parsing — the same
// path the Studio page uses) and components/StudioClaims.tsx.

import { headers } from "next/headers";
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import {
  authConfig, authenticatedStores, exactCookieHeader, isBase64URL32,
  safeError, SESSION_COOKIE,
} from "@/lib/auth";
import type { Store } from "@/lib/model";
import { studioUUID } from "@/lib/studio-model";
import type { StudioErrorCode } from "@/lib/studio-client";
import { StudioClaims } from "@/components/StudioClaims";

export default async function StudioClaimsPage({
  params, searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const query = await searchParams;
  if (Object.keys(query).some((key) => key !== "store" && key !== "scene")) notFound();
  const store = query.store, scene = query.scene;
  if (typeof store !== "string" || typeof scene !== "string" || !studioUUID.test(store) || !studioUUID.test(scene)) notFound();

  let selected: Store | null = null;
  let error: StudioErrorCode | null = null;
  if (!authConfig) error = "signed-out";
  else {
    const token = exactCookieHeader((await headers()).get("cookie"), SESSION_COOKIE);
    if (!token || !isBase64URL32(token)) error = "signed-out";
    else {
      const listed = await authenticatedStores(token);
      if (!listed.stores) {
        const safe = await safeError(listed.response);
        error = safe.status === 401 ? "signed-out" : safe.status === 403 ? "forbidden" : "unavailable";
      } else {
        selected = listed.stores.find((item) => item.id === store) ?? null;
        if (!selected) notFound();
      }
    }
  }
  return <StudioClaims locale={locale} store={selected} scene={scene} initialError={error} />;
}
