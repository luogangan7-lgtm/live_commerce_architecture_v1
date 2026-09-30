import type { APIError, Page, Store } from "./model";

export type Account = {
  id: string;
  provider: "payuni";
  environment: "SANDBOX" | "LIVE";
  account_id: string;
  binding_id: string;
  binding_version: number;
  credential_version: number;
  enabled: boolean;
  state: string;
};
export type Market = {
  id: string;
  code: string;
  name: string;
  currency: string;
  version: number;
  active: boolean;
};
export type Method = {
  market_id: string;
  country: string;
  code: MethodCode;
  version: number;
  provider: string;
  environment: "SANDBOX" | "LIVE";
  connection_id: string;
  binding_version: number;
  currency: string;
  name_hans: string;
  name_hant: string;
  name_en: string;
  enabled: boolean;
  visible: boolean;
  sort_order: number;
  min_amount_minor: number;
  max_amount_minor: number;
};
export type Policy = {
  market_id: string;
  country: string;
  method: string;
  currency: string;
  shipping_mode: "country_flat";
  tax_mode: "none" | "inclusive" | "exclusive";
  tax_basis: "goods" | "goods_and_shipping";
  version: number;
  shipping_minor: number;
  tax_rate_bps: number;
  quote_ttl_seconds: number;
  enabled: boolean;
};
export type Service = {
  market_id: string;
  country: string;
  code: string;
  version: number;
  policy_method: string;
  policy_version: number;
  currency: string;
  name_hans: string;
  name_hant: string;
  name_en: string;
  delivery_kind: "home" | "cvs_711" | "cvs_familymart" | "cvs_hilife" | "cvs_okmart";
  mode: "MANUAL" | "API";
  enabled: boolean;
  visible: boolean;
  sort_order: number;
};
export type Availability = {
  available: boolean;
  method_version: number;
  binding_version: number;
  credential_version: number;
  reasons: string[];
};
export const methodCodes = [
  "payuni_credit",
  "payuni_installment",
  "payuni_atm",
  "payuni_cvs",
  "payuni_linepay",
] as const;
export type MethodCode = (typeof methodCodes)[number];
export type SettingsInitial = { store: Store | null; error: APIError | null };
export type SettingsPages = {
  accounts: Page<Account>;
  markets: Page<Market>;
  methods: Page<Method>;
  services: Page<Service>;
};
