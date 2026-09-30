// GET /[locale]/ (server). Signed-out state renders PasswordAuth when COMMERCE_PASSWORD_LOGIN_ENABLED=1,
// else the OIDC entry; data comes from lib/backend.ts workspaceData → GET /v1/admin/stores (Go).
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import { onboardingPolicy, workspaceData } from "@/lib/backend";
import { authConfig } from "@/lib/auth";
import { Ledger } from "@/components/Ledger";
import { Entry } from "@/components/Entry";

export default async function Page({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const query = await searchParams;
  const one = (key: string) =>
    typeof query[key] === "string" ? (query[key] as string) : "";
  const initial = await workspaceData(
    one("warehouse"),
    one("q"),
    one("status") || "all",
    one("cursor"),
  );
  if (initial.storeID || initial.fixture)
    return <Ledger locale={locale} initial={initial} />;
  const policy = onboardingPolicy();
  const status = !authConfig
    ? "disabled"
    : initial.error?.code === "unauthorized"
      ? "signed-out"
      : initial.error
        ? "unavailable"
        : "onboarding";
  return (
    <Entry
      locale={locale}
      status={status}
      authResult={one("auth")}
      onboardingEnabled={policy.enabled}
      currencies={policy.currencies}
      passwordMode={status === "signed-out" && authConfig?.passwordLogin ? "signin" : undefined}
      oidc={!!authConfig?.issuer}
    />
  );
}
