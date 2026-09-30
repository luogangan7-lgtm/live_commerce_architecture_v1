// GET /[locale]/reset (server). Renders PasswordAuth mode="reset", which calls POST /api/auth/password/reset and
// /api/auth/password/verify → Go /v1/identity/password/reset and /complete (internal/identityhttp/password.go).
// 404 unless COMMERCE_PASSWORD_LOGIN_ENABLED=1; a valid session redirects to /<locale>/ (U6).
import { notFound, redirect } from "next/navigation";
import { headers } from "next/headers";
import { isLocale } from "@live-commerce/i18n";
import {
  SESSION_COOKIE,
  authConfig,
  authenticatedStores,
  exactCookieHeader,
  isBase64URL32,
} from "@/lib/auth";
import { onboardingPolicy } from "@/lib/backend";
import { Entry } from "@/components/Entry";

export default async function Page({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  if (!authConfig?.passwordLogin) notFound();
  const token =
    exactCookieHeader((await headers()).get("cookie"), SESSION_COOKIE) ?? "";
  // Any authenticated store list (even empty) proves the session; workspace pages own the rest.
  if (isBase64URL32(token) && (await authenticatedStores(token)).stores)
    redirect(`/${locale}/`);
  const policy = onboardingPolicy();
  return (
    <Entry
      locale={locale}
      status="signed-out"
      authResult=""
      onboardingEnabled={policy.enabled}
      currencies={policy.currencies}
      passwordMode="reset"
      oidc={!!authConfig.issuer}
      path="reset"
    />
  );
}
