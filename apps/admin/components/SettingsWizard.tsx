"use client";

// Merchant settings wizard (approved A four-step sequence): BFF /api/stores/{store}/{provider-accounts,markets/...}
// -> Go internal/httpapi settings routes. The merchant-arranged (manual) branch also hosts the logistics cards
// (<LogisticsSettings>: BFF logistics/ecpay, logistics/ecpay/enabled, logistics/cvs-settings -> Go
// internal/httpapi/cvs.go, taiwan-cvs-logistics-v1 §8/§16.5) in step 2, and its delivery-service editor offers
// mode "API (ECPay)" for CVS kinds only while the ECPay connection is enabled and checked (§4.1 predicate).
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import type { Locale } from "@live-commerce/i18n";
import { WorkspaceFrame } from "./WorkspaceFrame";
import { LogisticsSettings } from "./LogisticsSettings";
import { availabilityReason, settingsCopy } from "@/lib/settings-copy";
import {
  csrfCookie,
  integer,
  readSettings,
  safeError,
  sessionBoundary,
  validCode,
  validCountry,
  writeSettings,
  type Pending,
} from "@/lib/settings-client";
import {
  methodCodes,
  type Account,
  type Availability,
  type Market,
  type Method,
  type MethodCode,
  type Policy,
  type Service,
  type SettingsInitial,
} from "@/lib/settings-model";
import { readEcpay } from "@/lib/logistics-client";
import { ecpayQualified } from "@/lib/logistics-model";
import { logisticsCopy } from "@/lib/logistics-copy";
import type { APIError, Page } from "@/lib/model";
import "./settings.css";

type Branch = "payuni" | "manual";
type Observation = { target: string; version: number; dirty: boolean };
type MethodBinding = {
  connectionID: string;
  bindingVersion: number;
  environment: "SANDBOX" | "LIVE";
};
type Draft = {
  branch: Branch;
  accountID: string;
  accountChoiceTouched: boolean;
  environment: "SANDBOX" | "LIVE";
  merchantID: string;
  rotate: boolean;
  marketID: string;
  marketCode: string;
  marketName: string;
  country: string;
  methodCode: MethodCode;
  nameHans: string;
  nameHant: string;
  nameEN: string;
  visible: boolean;
  sort: string;
  min: string;
  max: string;
  serviceCode: string;
  serviceKind: Service["delivery_kind"];
  serviceMode: Service["mode"];
  shipping: string;
  taxMode: "none" | "inclusive" | "exclusive";
  taxBasis: "goods" | "goods_and_shipping";
  taxRate: string;
  ttl: string;
  policyEnabled: boolean;
  serviceEnabled: boolean;
  serviceVisible: boolean;
  reference: string;
  methodObservation: Observation | null;
  methodBinding: MethodBinding | null;
  policyObservation: Observation | null;
  serviceObservation: Observation | null;
};
const emptyDraft: Draft = {
  branch: "payuni",
  accountID: "",
  accountChoiceTouched: false,
  environment: "SANDBOX",
  merchantID: "",
  rotate: false,
  marketID: "",
  marketCode: "",
  marketName: "",
  country: "TW",
  methodCode: "payuni_credit",
  nameHans: "信用卡",
  nameHant: "信用卡",
  nameEN: "Credit card",
  visible: false,
  sort: "10",
  min: "1",
  max: "1000000000000",
  serviceCode: "",
  serviceKind: "home",
  serviceMode: "MANUAL",
  shipping: "0",
  taxMode: "none",
  taxBasis: "goods",
  taxRate: "0",
  ttl: "300",
  policyEnabled: false,
  serviceEnabled: false,
  serviceVisible: false,
  reference: "",
  methodObservation: null,
  methodBinding: null,
  policyObservation: null,
  serviceObservation: null,
};
const names: Record<MethodCode, [string, string, string]> = {
  payuni_credit: ["信用卡", "信用卡", "Credit card"],
  payuni_installment: ["信用卡分期", "信用卡分期", "Card installments"],
  payuni_atm: ["ATM 转账", "ATM 轉帳", "ATM transfer"],
  payuni_cvs: ["超商代码缴费", "超商代碼繳費", "Convenience store code"],
  payuni_linepay: ["LINE Pay", "LINE Pay", "LINE Pay"],
};
const unknown: APIError = {
  code: "retry_later",
  message: "",
  request_id: "",
  retryable: true,
  details: {},
};

export function SettingsWizard({
  locale,
  initial,
}: {
  locale: Locale;
  initial: SettingsInitial;
}) {
  const c = settingsCopy[locale],
    lc = logisticsCopy[locale],
    store = initial.store;
  const [step, setStep] = useState<1 | 2 | 3 | 4>(1);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [accounts, setAccounts] = useState<Account[]>([]),
    [accountCursor, setAccountCursor] = useState("");
  const [markets, setMarkets] = useState<Market[]>([]),
    [marketCursor, setMarketCursor] = useState("");
  const [methods, setMethods] = useState<Method[]>([]),
    [services, setServices] = useState<Service[]>([]);
  const [serviceCursor, setServiceCursor] = useState("");
  const [policy, setPolicy] = useState<Policy | null>(null),
    [availability, setAvailability] = useState<Availability | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [ready, setReady] = useState(false),
    [loading, setLoading] = useState(false),
    [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState(""),
    [error, setError] = useState<APIError | null>(initial.error);
  const boundary = useRef(""),
    draftKey = useRef(""),
    journalKey = useRef("");
  const epoch = useRef(0),
    mounted = useRef(true),
    busyRef = useRef(false);
  const marketRead = useRef(0),
    policyRead = useRef(0);
  const serviceRead = useRef(0),
    accountPageRead = useRef(0),
    marketPageRead = useRef(0),
    servicePageRead = useRef(0);
  const draftRef = useRef(draft);
  draftRef.current = draft;
  const accountsRef = useRef(accounts);
  accountsRef.current = accounts;
  const stepRef = useRef(step);
  stepRef.current = step;
  const [methodListTarget, setMethodListTarget] = useState("");
  // Whether "API (ECPay)" may be chosen for a CVS service: the store's ECPay connection is enabled and checked.
  // Read-only probe (GET logistics/ecpay, integration:read); a 403/404/failure simply leaves API mode unavailable.
  const [ecpayReady, setEcpayReady] = useState(false);
  const hashKey = useRef<HTMLInputElement>(null),
    hashIV = useRef<HTMLInputElement>(null);
  const activeAccount =
    accounts.find((item) => item.id === draft.accountID) ?? null;
  const activeMarket =
    markets.find((item) => item.id === draft.marketID) ?? null;
  const activeMethod =
    methods.find((item) => item.code === draft.methodCode) ?? null;
  const activeService =
    services.find((item) => item.code === draft.serviceCode) ?? null;
  const methodTarget = draft.marketID
    ? `${draft.marketID}:TW:${draft.methodCode}`
    : "";
  const serviceTarget =
    draft.marketID &&
    validCountry(draft.country) &&
    validCode(draft.serviceCode)
      ? `${draft.marketID}:${draft.country}:${draft.serviceCode}`
      : "";

  const probeEcpay = store?.id ?? "";
  const probeStep = ready && draft.branch === "manual" && step === 3;
  useEffect(() => {
    if (!probeEcpay || !probeStep) return;
    const active = new AbortController();
    readEcpay(probeEcpay, active.signal).then(
      (value) => setEcpayReady(ecpayQualified(value)),
      () => !active.signal.aborted && setEcpayReady(false),
    );
    return () => active.abort();
  }, [probeEcpay, probeStep]);

  function saveDraft(next: Draft, atStep = stepRef.current) {
    persistDraft(next, atStep);
    draftRef.current = next;
    setDraft(next);
  }
  function hydrateMethod(
    current: Draft,
    saved: Method | null,
    fetchedAccount?: Account | null,
  ): Draft {
    const labels = names[current.methodCode];
    const rebind =
      !!saved &&
      current.accountChoiceTouched &&
      !!current.accountID &&
      (current.accountID !== saved.connection_id ||
        (!!current.methodBinding &&
          current.methodBinding.bindingVersion !== saved.binding_version));
    const boundAccount = saved
      ? (fetchedAccount ??
        accountsRef.current.find((item) => item.id === saved.connection_id))
      : null;
    return {
      ...current,
      accountID:
        saved && !current.accountChoiceTouched
          ? saved.connection_id
          : current.accountID,
      environment:
        saved && !current.accountChoiceTouched
          ? saved.environment
          : current.environment,
      merchantID:
        saved && !current.accountChoiceTouched
          ? (boundAccount?.account_id ?? "")
          : current.merchantID,
      methodBinding: rebind
        ? current.methodBinding
        : saved
          ? {
              connectionID: saved.connection_id,
              bindingVersion: saved.binding_version,
              environment: saved.environment,
            }
          : current.methodBinding,
      nameHans: saved?.name_hans ?? labels[0],
      nameHant: saved?.name_hant ?? labels[1],
      nameEN: saved?.name_en ?? labels[2],
      visible: saved?.visible ?? false,
      sort: String(saved?.sort_order ?? 10),
      min: String(saved?.min_amount_minor ?? 1),
      max: String(saved?.max_amount_minor ?? 1000000000000),
      methodObservation: {
        target: `${current.marketID}:TW:${current.methodCode}`,
        version: saved?.version ?? 0,
        dirty: rebind,
      },
    };
  }
  function withAccountBinding(current: Draft, account: Account | null): Draft {
    const binding: MethodBinding | null = account
      ? {
          connectionID: account.id,
          bindingVersion: account.binding_version,
          environment: account.environment,
        }
      : null;
    const old = current.methodBinding;
    const changed =
      old?.connectionID !== binding?.connectionID ||
      old?.bindingVersion !== binding?.bindingVersion ||
      old?.environment !== binding?.environment;
    const target = `${current.marketID}:TW:${current.methodCode}`;
    return {
      ...current,
      accountID: account?.id ?? "",
      accountChoiceTouched: true,
      environment: account?.environment ?? "SANDBOX",
      merchantID: account?.account_id ?? "",
      rotate: false,
      methodBinding: binding,
      methodObservation:
        changed && current.marketID
          ? {
              target,
              version:
                current.methodObservation?.target === target
                  ? current.methodObservation.version
                  : -1,
              dirty: true,
            }
          : current.methodObservation,
    };
  }
  function hydrateService(current: Draft, saved: Service | null): Draft {
    return {
      ...current,
      nameHans: saved?.name_hans ?? "",
      nameHant: saved?.name_hant ?? "",
      nameEN: saved?.name_en ?? "",
      serviceKind: saved?.delivery_kind ?? current.serviceKind,
      serviceMode: saved?.mode ?? "MANUAL",
      serviceEnabled: saved?.enabled ?? false,
      serviceVisible: saved?.visible ?? false,
      sort: String(saved?.sort_order ?? 10),
      serviceObservation: {
        target: `${current.marketID}:${current.country}:${current.serviceCode}`,
        version: saved?.version ?? 0,
        dirty: false,
      },
    };
  }
  function hydratePolicy(current: Draft, saved: Policy | null): Draft {
    return {
      ...current,
      shipping: String(saved?.shipping_minor ?? 0),
      taxMode: saved?.tax_mode ?? "none",
      taxBasis: saved?.tax_basis ?? "goods",
      taxRate: String(saved?.tax_rate_bps ?? 0),
      ttl: String(saved?.quote_ttl_seconds ?? 300),
      policyEnabled: saved?.enabled ?? false,
      reference: "",
      policyObservation: {
        target: `${current.marketID}:${current.country}:${current.serviceCode}`,
        version: saved?.version ?? 0,
        dirty: false,
      },
    };
  }

  function clearSecrets() {
    if (hashKey.current) hashKey.current.value = "";
    if (hashIV.current) hashIV.current.value = "";
  }
  async function stillSession() {
    try {
      return (await sessionBoundary()) === boundary.current;
    } catch {
      return false;
    }
  }
  function displayError(value: APIError | null) {
    if (!value) return "";
    if (value.code === "command_storage_unavailable") return c.storage;
    if (value.code === "secret_retry_mismatch") return c.secretRetryMismatch;
    if (value.code === "session_changed" || value.code === "unauthorized")
      return c.session;
    if (value.code === "conflict") return c.conflict;
    if (value.code === "forbidden") return c.forbidden;
    if (value.code === "invalid_request" || value.code === "invalid_json")
      return c.invalid;
    return c.failed;
  }
  function storageError() {
    setError({
      ...unknown,
      code: "command_storage_unavailable",
      retryable: false,
    });
    setReady(false);
  }
  function persistDraft(next: Draft, nextStep: number) {
    if (!draftKey.current) throw new Error("storage");
    const encoded = JSON.stringify({ version: 1, draft: next, step: nextStep });
    sessionStorage.setItem(draftKey.current, encoded);
    if (sessionStorage.getItem(draftKey.current) !== encoded)
      throw new Error("storage");
  }
  function update<K extends keyof Draft>(field: K, value: Draft[K]) {
    const next = { ...draft, [field]: value };
    // API (ECPay) is a CVS-only mode: choosing home delivery puts the service back to manual.
    if (field === "serviceKind" && value === "home") next.serviceMode = "MANUAL";
    if (
      field === "serviceCode" ||
      field === "marketID" ||
      field === "country"
    ) {
      setPolicy(null);
      next.policyObservation = null;
      next.serviceObservation = null;
      if (field !== "serviceCode") next.methodObservation = null;
      policyRead.current += 1;
      serviceRead.current += 1;
      marketRead.current += 1;
    }
    if (
      [
        "nameHans",
        "nameHant",
        "nameEN",
        "visible",
        "sort",
        "min",
        "max",
      ].includes(field) &&
      draft.branch === "payuni"
    )
      next.methodObservation = {
        target: methodTarget,
        version: next.methodObservation?.version ?? -1,
        dirty: true,
      };
    if (
      [
        "nameHans",
        "nameHant",
        "nameEN",
        "serviceKind",
        "serviceMode",
        "serviceEnabled",
        "serviceVisible",
        "sort",
      ].includes(field) &&
      draft.branch === "manual"
    )
      next.serviceObservation = {
        target: serviceTarget,
        version: next.serviceObservation?.version ?? -1,
        dirty: true,
      };
    if (
      [
        "shipping",
        "taxMode",
        "taxBasis",
        "taxRate",
        "ttl",
        "policyEnabled",
        "reference",
      ].includes(field) &&
      draft.branch === "manual"
    )
      next.policyObservation = {
        target: serviceTarget,
        version: next.policyObservation?.version ?? -1,
        dirty: true,
      };
    try {
      persistDraft(next, step);
      draftRef.current = next;
      setDraft(next);
      setError(null);
    } catch {
      storageError();
    }
  }
  function goStep(next: 1 | 2 | 3 | 4) {
    clearSecrets();
    try {
      persistDraft(draft, next);
      setStep(next);
      setError(null);
      setNotice("");
    } catch {
      storageError();
    }
  }
  function changeBranch(branch: Branch) {
    clearSecrets();
    const next = {
      ...draft,
      branch,
      methodObservation: null,
      policyObservation: null,
      serviceObservation: null,
    };
    try {
      saveDraft(next);
      setPolicy(null);
      setError(null);
    } catch {
      storageError();
    }
  }

  useEffect(() => {
    mounted.current = true;
    if (!store) return;
    let active = true;
    void (async () => {
      try {
        if (!navigator.locks) throw new Error("storage");
        const hash = await sessionBoundary();
        if (!active) return;
        boundary.current = hash;
        draftKey.current = `commerce-settings-draft:${store.id}:${hash}`;
        journalKey.current = `commerce-settings-command:${store.id}:${hash}`;
        const rawDraft = sessionStorage.getItem(draftKey.current);
        const rawPending = localStorage.getItem(journalKey.current);
        if (rawDraft) {
          const saved = JSON.parse(rawDraft) as {
            version?: number;
            draft?: Draft;
            step?: number;
          };
          if (
            saved.version !== 1 ||
            !saved.draft ||
            ![1, 2, 3, 4].includes(saved.step ?? 0)
          )
            throw new Error("storage");
          // A draft saved before serviceMode existed reads as MANUAL.
          setDraft({ ...emptyDraft, ...saved.draft });
          setStep(saved.step as 1 | 2 | 3 | 4);
        }
        if (rawPending) {
          const saved = JSON.parse(rawPending) as Pending;
          if (
            !saved ||
            !/^[A-Za-z0-9_.:-]{8,128}$/.test(saved.key) ||
            !/^(provider-accounts(?:\/[0-9a-f-]{36}\/rotate)?|markets(?:\/[0-9a-f-]{36}\/countries\/[A-Z]{2}\/(?:payment-methods|delivery-services)\/[a-z][a-z0-9_-]{0,39}(?:\/policy)?)?)$/.test(
              saved.resource,
            ) ||
            (saved.secret && saved.body) ||
            (!saved.secret && typeof saved.body !== "string")
          )
            throw new Error("storage");
          setPending(saved);
          setNotice(c.unknown);
        }
        setReady(true);
      } catch {
        if (active) storageError();
      }
    })();
    return () => {
      active = false;
      mounted.current = false;
      epoch.current += 1;
      clearSecrets();
    };
    // Locale changes remount but retain the session-scoped nonsecret draft.
  }, [store?.id]);

  const loadBase = useCallback(async () => {
    if (!store) return;
    const token = epoch.current;
    accountPageRead.current += 1;
    marketPageRead.current += 1;
    setLoading(true);
    try {
      const [accountPage, marketPage] = await Promise.all([
        readSettings<Page<Account>>(store.id, "provider-accounts"),
        readSettings<Page<Market>>(store.id, "markets"),
      ]);
      const selectedID = draftRef.current.accountID;
      const selected = selectedID
        ? (accountPage.items.find((item) => item.id === selectedID) ??
          (await readSettings<Account>(
            store.id,
            `provider-accounts/${selectedID}`,
          )))
        : null;
      if (
        !mounted.current ||
        token !== epoch.current ||
        (await sessionBoundary()) !== boundary.current
      )
        return;
      setAccounts((rows) => {
        const page =
          selected && !accountPage.items.some((item) => item.id === selected.id)
            ? [...accountPage.items, selected]
            : accountPage.items;
        return [
          ...page,
          ...rows.filter((item) => !page.some((row) => row.id === item.id)),
        ];
      });
      setMarkets(marketPage.items);
      setAccountCursor(accountPage.next_cursor);
      setMarketCursor(marketPage.next_cursor);
      const chosen =
        selected ??
        (!selectedID &&
        !draftRef.current.accountChoiceTouched &&
        !draftRef.current.marketID
          ? (accountPage.items[0] ?? null)
          : null);
      if (
        chosen &&
        (!draftRef.current.accountID ||
          draftRef.current.accountID === chosen.id)
      ) {
        const next = {
          ...draftRef.current,
          accountID: chosen.id,
          accountChoiceTouched: draftRef.current.accountChoiceTouched,
          environment: chosen.environment,
          merchantID: chosen.account_id,
          methodBinding: draftRef.current.methodBinding ?? {
            connectionID: chosen.id,
            bindingVersion: chosen.binding_version,
            environment: chosen.environment,
          },
        };
        saveDraft(next);
      }
    } catch (value) {
      if (mounted.current && token === epoch.current && (await stillSession()))
        setError(safeError(value));
    } finally {
      if (mounted.current && token === epoch.current) setLoading(false);
    }
  }, [store]);
  useEffect(() => {
    if (ready) void loadBase();
  }, [ready, loadBase]);

  const loadMarket = useCallback(
    async (marketID: string, country: string) => {
      if (!store || !marketID || !validCountry(country)) return;
      const token = epoch.current,
        request = ++marketRead.current;
      servicePageRead.current += 1;
      setLoading(true);
      try {
        const base = `markets/${marketID}/countries/${country}`;
        const [methodPage, servicePage] = await Promise.all([
          country === "TW"
            ? readSettings<Page<Method>>(store.id, `${base}/payment-methods`)
            : Promise.resolve({ items: [], next_cursor: "" }),
          readSettings<Page<Service>>(store.id, `${base}/delivery-services`),
        ]);
        const methodCode = draftRef.current.methodCode;
        const savedMethod =
          methodPage.items.find((item) => item.code === methodCode) ?? null;
        const boundAccount =
          savedMethod &&
          !accountsRef.current.some(
            (item) => item.id === savedMethod.connection_id,
          )
            ? await readSettings<Account>(
                store.id,
                `provider-accounts/${savedMethod.connection_id}`,
              )
            : null;
        if (
          !mounted.current ||
          token !== epoch.current ||
          request !== marketRead.current ||
          draftRef.current.marketID !== marketID ||
          draftRef.current.country !== country ||
          draftRef.current.methodCode !== methodCode ||
          (await sessionBoundary()) !== boundary.current
        )
          return;
        setMethods(methodPage.items);
        if (boundAccount)
          setAccounts((rows) =>
            rows.some((item) => item.id === boundAccount.id)
              ? rows
              : [...rows, boundAccount],
          );
        setServices(servicePage.items);
        setMethodListTarget(`${marketID}:${country}`);
        setServiceCursor(servicePage.next_cursor);
        setAvailability(null);
        const current = draftRef.current;
        if (current.branch === "payuni" && country === "TW") {
          const target = `${marketID}:TW:${current.methodCode}`;
          const observed = current.methodObservation;
          const saved = savedMethod;
          if (!observed || observed.target !== target || !observed.dirty)
            saveDraft(hydrateMethod(current, saved, boundAccount));
          else if (observed.version === -1 && !saved)
            saveDraft({
              ...current,
              methodObservation: { ...observed, version: 0 },
            });
          else if (observed.version !== (saved?.version ?? 0))
            setError({ ...unknown, code: "conflict" });
        }
      } catch (value) {
        if (
          mounted.current &&
          token === epoch.current &&
          request === marketRead.current &&
          draftRef.current.marketID === marketID &&
          draftRef.current.country === country &&
          (await stillSession())
        )
          setError(safeError(value));
      } finally {
        if (
          mounted.current &&
          token === epoch.current &&
          request === marketRead.current
        )
          setLoading(false);
      }
    },
    [store],
  );
  useEffect(() => {
    if (ready && draft.marketID) void loadMarket(draft.marketID, draft.country);
  }, [
    ready,
    draft.marketID,
    draft.country,
    draft.branch,
    draft.methodCode,
    loadMarket,
  ]);

  async function moreAccounts() {
    if (!store || !ready || !accountCursor) return;
    const token = epoch.current,
      request = ++accountPageRead.current,
      cursor = accountCursor;
    try {
      const page = await readSettings<Page<Account>>(
        store.id,
        `provider-accounts?cursor=${encodeURIComponent(cursor)}`,
      );
      if (
        !mounted.current ||
        token !== epoch.current ||
        request !== accountPageRead.current ||
        (await sessionBoundary()) !== boundary.current
      )
        return;
      setAccounts((rows) => [
        ...rows,
        ...page.items.filter((item) => !rows.some((row) => row.id === item.id)),
      ]);
      setAccountCursor(page.next_cursor);
    } catch (issue) {
      if (
        mounted.current &&
        request === accountPageRead.current &&
        (await stillSession())
      )
        setError(safeError(issue));
    }
  }
  async function moreMarkets() {
    if (!store || !ready || !marketCursor) return;
    const token = epoch.current,
      request = ++marketPageRead.current,
      cursor = marketCursor;
    try {
      const page = await readSettings<Page<Market>>(
        store.id,
        `markets?cursor=${encodeURIComponent(cursor)}`,
      );
      if (
        !mounted.current ||
        token !== epoch.current ||
        request !== marketPageRead.current ||
        (await sessionBoundary()) !== boundary.current
      )
        return;
      setMarkets((rows) => [
        ...rows,
        ...page.items.filter((item) => !rows.some((row) => row.id === item.id)),
      ]);
      setMarketCursor(page.next_cursor);
    } catch (issue) {
      if (
        mounted.current &&
        request === marketPageRead.current &&
        (await stillSession())
      )
        setError(safeError(issue));
    }
  }
  async function moreServices() {
    if (!store || !ready || !activeMarket || !serviceCursor) return;
    const token = epoch.current,
      request = ++servicePageRead.current;
    const marketID = activeMarket.id,
      country = draft.country,
      cursor = serviceCursor;
    try {
      const page = await readSettings<Page<Service>>(
        store.id,
        `markets/${marketID}/countries/${country}/delivery-services?cursor=${encodeURIComponent(cursor)}`,
      );
      if (
        !mounted.current ||
        token !== epoch.current ||
        request !== servicePageRead.current ||
        draftRef.current.marketID !== marketID ||
        draftRef.current.country !== country ||
        (await sessionBoundary()) !== boundary.current
      )
        return;
      setServices((rows) => [
        ...rows,
        ...page.items.filter(
          (item) => !rows.some((row) => row.code === item.code),
        ),
      ]);
      setServiceCursor(page.next_cursor);
    } catch (issue) {
      if (
        mounted.current &&
        request === servicePageRead.current &&
        draftRef.current.marketID === marketID &&
        draftRef.current.country === country &&
        (await stillSession())
      )
        setError(safeError(issue));
    }
  }

  const loadPolicy = useCallback(
    async (marketID: string, country: string, code: string) => {
      if (!store || !marketID || !validCountry(country) || !validCode(code)) {
        setPolicy(null);
        return;
      }
      const token = epoch.current,
        request = ++policyRead.current;
      const target = `${marketID}:${country}:${code}`;
      try {
        const result = await readSettings<Policy>(
          store.id,
          `markets/${marketID}/countries/${country}/delivery-services/${code}/policy`,
        );
        if (
          mounted.current &&
          token === epoch.current &&
          request === policyRead.current &&
          `${draftRef.current.marketID}:${draftRef.current.country}:${draftRef.current.serviceCode}` ===
            target &&
          (await sessionBoundary()) === boundary.current
        ) {
          setPolicy(result);
          const current = draftRef.current,
            observed = current.policyObservation;
          if (!observed || observed.target !== target || !observed.dirty)
            saveDraft(hydratePolicy(current, result));
          else if (observed.version !== result.version)
            setError({ ...unknown, code: "conflict" });
        }
      } catch (value) {
        const issue = safeError(value);
        if (
          mounted.current &&
          token === epoch.current &&
          request === policyRead.current &&
          `${draftRef.current.marketID}:${draftRef.current.country}:${draftRef.current.serviceCode}` ===
            target &&
          (await stillSession())
        ) {
          setPolicy(null);
          if (issue.code === "not_found") {
            const current = draftRef.current,
              observed = current.policyObservation;
            if (!observed || observed.target !== target || !observed.dirty)
              saveDraft(hydratePolicy(current, null));
            else if (observed.version === -1)
              saveDraft({
                ...current,
                policyObservation: { ...observed, version: 0 },
              });
            else if (observed.version !== 0)
              setError({ ...unknown, code: "conflict" });
          } else setError(issue);
        }
      }
    },
    [store],
  );
  useEffect(() => {
    if (ready && draft.branch === "manual")
      void loadPolicy(draft.marketID, draft.country, draft.serviceCode);
  }, [
    ready,
    draft.branch,
    draft.marketID,
    draft.country,
    draft.serviceCode,
    loadPolicy,
  ]);

  const loadService = useCallback(
    async (marketID: string, country: string, code: string) => {
      if (!store || !marketID || !validCountry(country) || !validCode(code))
        return;
      const token = epoch.current,
        request = ++serviceRead.current;
      const target = `${marketID}:${country}:${code}`;
      try {
        const result = await readSettings<Service>(
          store.id,
          `markets/${marketID}/countries/${country}/delivery-services/${code}`,
        );
        if (
          !mounted.current ||
          token !== epoch.current ||
          request !== serviceRead.current ||
          `${draftRef.current.marketID}:${draftRef.current.country}:${draftRef.current.serviceCode}` !==
            target ||
          (await sessionBoundary()) !== boundary.current
        )
          return;
        setServices((rows) => [
          ...rows.filter((row) => row.code !== result.code),
          result,
        ]);
        const current = draftRef.current,
          observed = current.serviceObservation;
        if (!observed || observed.target !== target || !observed.dirty)
          saveDraft(hydrateService(current, result));
        else if (observed.version !== result.version)
          setError({ ...unknown, code: "conflict" });
      } catch (value) {
        const issue = safeError(value);
        if (
          !mounted.current ||
          token !== epoch.current ||
          request !== serviceRead.current ||
          `${draftRef.current.marketID}:${draftRef.current.country}:${draftRef.current.serviceCode}` !==
            target ||
          !(await stillSession())
        )
          return;
        if (issue.code === "not_found") {
          setServices((rows) => rows.filter((row) => row.code !== code));
          const current = draftRef.current,
            observed = current.serviceObservation;
          if (!observed || observed.target !== target || !observed.dirty)
            saveDraft(hydrateService(current, null));
          else if (observed.version === -1)
            saveDraft({
              ...current,
              serviceObservation: { ...observed, version: 0 },
            });
          else if (observed.version !== 0)
            setError({ ...unknown, code: "conflict" });
        } else setError(issue);
      }
    },
    [store],
  );
  useEffect(() => {
    if (ready && draft.branch === "manual")
      void loadService(draft.marketID, draft.country, draft.serviceCode);
  }, [
    ready,
    draft.branch,
    draft.marketID,
    draft.country,
    draft.serviceCode,
    loadService,
  ]);

  async function savePending(
    command: Pending,
    body: string,
    after: (value: unknown) => void,
  ) {
    if (!store || busyRef.current || !ready) return;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    setNotice("");
    const startEpoch = epoch.current;
    try {
      await navigator.locks.request(journalKey.current, async () => {
        if ((await sessionBoundary()) !== boundary.current)
          throw new Error("session_changed");
        const raw = localStorage.getItem(journalKey.current);
        if (raw) {
          const previous = JSON.parse(raw) as Pending;
          if (previous.key !== command.key) {
            setPending(previous);
            setNotice(c.unknown);
            return;
          }
          command = previous;
          if (!command.secret) body = command.body!;
        } else {
          // A secret may have committed before this tab receives its response.
          // Persist that uncertainty before dispatch; the first in-memory
          // attempt still distinguishes a definite server rejection.
          const encoded = JSON.stringify(
            command.secret ? { ...command, uncertain: true } : command,
          );
          localStorage.setItem(journalKey.current, encoded);
          if (localStorage.getItem(journalKey.current) !== encoded)
            throw new Error("storage");
        }
        setPending(command);
        const result = await writeSettings<unknown>(
          store.id,
          command,
          body,
          boundary.current,
        );
        if ((await sessionBoundary()) !== boundary.current)
          throw new Error("session_changed");
        if (!mounted.current || startEpoch !== epoch.current) return;
        if (result.uncertain) {
          const retry = { ...command, uncertain: true };
          const encoded = JSON.stringify(retry);
          localStorage.setItem(journalKey.current, encoded);
          if (localStorage.getItem(journalKey.current) !== encoded)
            throw new Error("storage");
          setPending(retry);
          setNotice(c.unknown);
          setError(result.error);
          return;
        }
        if (
          command.secret &&
          command.uncertain &&
          result.error?.code === "conflict"
        ) {
          setPending(command);
          setError({ ...result.error, code: "secret_retry_mismatch" });
          return;
        }
        localStorage.removeItem(journalKey.current);
        setPending(null);
        if (result.error) {
          setError(result.error);
          return;
        }
        after(result.value);
      });
    } catch (value) {
      if (value instanceof Error && value.message === "session_changed") {
        try {
          localStorage.removeItem(journalKey.current);
          sessionStorage.removeItem(draftKey.current);
        } catch {
          /* session is lost */
        }
        setPending(null);
        setDraft(emptyDraft);
        setReady(false);
        setError({ ...unknown, code: "session_changed", retryable: false });
      } else storageError();
    } finally {
      busyRef.current = false;
      if (mounted.current) setBusy(false);
    }
  }
  function command(
    method: "POST" | "PUT",
    resource: string,
    payload: unknown,
    after: (value: unknown) => void,
  ) {
    if (pending) {
      setNotice(c.unknown);
      return;
    }
    const body = JSON.stringify(payload);
    void savePending(
      { key: crypto.randomUUID(), method, resource, body },
      body,
      after,
    );
  }
  function secretCommand(
    secret: "create" | "rotate",
    resource: string,
    context: Pending["context"],
    after: (value: unknown) => void,
  ) {
    if (pending) {
      setNotice(c.unknown);
      return;
    }
    const credentials = {
      hash_key: hashKey.current?.value ?? "",
      hash_iv: hashIV.current?.value ?? "",
    };
    if (!credentials.hash_key || !credentials.hash_iv) {
      setError({ ...unknown, code: "invalid_request" });
      return;
    }
    const body = JSON.stringify(
      secret === "create"
        ? {
            provider: "payuni",
            environment: context?.environment,
            account_id: context?.account_id,
            credentials,
          }
        : { expected_version: context?.expected_version, credentials },
    );
    clearSecrets();
    void savePending(
      { key: crypto.randomUUID(), method: "POST", resource, secret, context },
      body,
      after,
    );
  }
  function retryPending() {
    if (!pending) return;
    let body = pending.body ?? "";
    if (pending.secret) {
      const credentials = {
        hash_key: hashKey.current?.value ?? "",
        hash_iv: hashIV.current?.value ?? "",
      };
      if (!credentials.hash_key || !credentials.hash_iv) {
        setError({ ...unknown, code: "invalid_request" });
        return;
      }
      body = JSON.stringify(
        pending.secret === "create"
          ? {
              provider: "payuni",
              environment: pending.context?.environment,
              account_id: pending.context?.account_id,
              credentials,
            }
          : {
              expected_version: pending.context?.expected_version,
              credentials,
            },
      );
      clearSecrets();
    }
    void savePending(pending, body, (value) => {
      const resource = pending.resource;
      if (pending.secret) {
        const saved = value as Account;
        setAccounts((rows) => [
          ...rows.filter((row) => row.id !== saved.id),
          saved,
        ]);
        commitDraft(withAccountBinding(draftRef.current, saved), 3);
        setNotice(c.accountSaved);
      } else if (resource === "markets") {
        const saved = value as Market;
        setMarkets((rows) => [
          ...rows.filter((row) => row.id !== saved.id),
          saved,
        ]);
        commitDraft(
          {
            ...draft,
            marketID: saved.id,
            methodObservation: null,
            policyObservation: null,
            serviceObservation: null,
          },
          3,
        );
        setNotice(c.saved);
      } else if (resource.endsWith("/policy")) {
        const saved = value as Policy;
        setPolicy(saved);
        saveDraft({
          ...draft,
          reference: "",
          policyObservation: {
            target: `${saved.market_id}:${saved.country}:${draft.serviceCode}`,
            version: saved.version,
            dirty: false,
          },
        });
        setNotice(c.policySaved);
      } else if (resource.includes("payment-methods")) {
        const saved = value as Method;
        setMethods((rows) => [
          ...rows.filter((row) => row.code !== saved.code),
          saved,
        ]);
        commitDraft(
          {
            ...draft,
            methodObservation: {
              target: `${saved.market_id}:${saved.country}:${saved.code}`,
              version: saved.version,
              dirty: false,
            },
          },
          4,
        );
        setNotice(c.methodSaved);
      } else if (resource.includes("delivery-services")) {
        const saved = value as Service;
        setServices((rows) => [
          ...rows.filter((row) => row.code !== saved.code),
          saved,
        ]);
        commitDraft(
          {
            ...draft,
            serviceObservation: {
              target: `${saved.market_id}:${saved.country}:${saved.code}`,
              version: saved.version,
              dirty: false,
            },
          },
          4,
        );
        setNotice(c.serviceSaved);
      }
    });
  }
  function commitDraft(next: Draft, nextStep: 1 | 2 | 3 | 4) {
    clearSecrets();
    try {
      persistDraft(next, nextStep);
      draftRef.current = next;
      setDraft(next);
      setStep(nextStep);
    } catch {
      storageError();
    }
  }
  function baseline(
    observed: Observation | null,
    target: string,
    currentVersion: number,
  ) {
    if (
      !observed ||
      !target ||
      observed.target !== target ||
      observed.version < 0 ||
      observed.version !== currentVersion
    ) {
      setError({ ...unknown, code: "conflict" });
      return null;
    }
    return observed.version;
  }
  async function reloadCurrent() {
    if (!store || !ready || pending || busyRef.current) return;
    clearSecrets();
    const next = {
      ...draft,
      // Explicit discard must drop the unsaved account choice too. Otherwise
      // hydration would pair that old binding with a newly read method version.
      ...(draft.branch === "payuni"
        ? {
            accountChoiceTouched: false,
            accountID: "",
            merchantID: "",
            methodBinding: null,
          }
        : {}),
      methodObservation: null,
      policyObservation: null,
      serviceObservation: null,
      reference: "",
    };
    try {
      saveDraft(next);
      setError(null);
      setNotice("");
    } catch {
      storageError();
      return;
    }
    await loadBase();
    if (next.marketID) await loadMarket(next.marketID, next.country);
    if (next.branch === "manual" && validCode(next.serviceCode))
      await Promise.all([
        loadPolicy(next.marketID, next.country, next.serviceCode),
        loadService(next.marketID, next.country, next.serviceCode),
      ]);
    if (mounted.current) setNotice(c.reloaded);
  }
  function accountSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (draft.accountID && !draft.rotate) {
      if (!activeAccount) {
        setError({ ...unknown, code: "conflict" });
        return;
      }
      goStep(3);
      return;
    }
    if (draft.rotate && activeAccount) {
      secretCommand(
        "rotate",
        `provider-accounts/${activeAccount.id}/rotate`,
        { expected_version: activeAccount.credential_version },
        (value) => {
          const saved = value as Account;
          setAccounts((rows) =>
            rows.map((row) => (row.id === saved.id ? saved : row)),
          );
          commitDraft(withAccountBinding(draftRef.current, saved), 3);
          setNotice(c.accountSaved);
        },
      );
      return;
    }
    if (!draft.merchantID.trim()) {
      setError({ ...unknown, code: "invalid_request" });
      return;
    }
    secretCommand(
      "create",
      "provider-accounts",
      {
        provider: "payuni",
        environment: draft.environment,
        account_id: draft.merchantID.trim(),
      },
      (value) => {
        const saved = value as Account;
        setAccounts((rows) => [...rows, saved]);
        commitDraft(withAccountBinding(draftRef.current, saved), 3);
        setNotice(c.accountSaved);
      },
    );
  }
  function marketSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!store || !validCode(draft.marketCode) || !draft.marketName.trim()) {
      setError({ ...unknown, code: "invalid_request" });
      return;
    }
    command(
      "POST",
      "markets",
      {
        code: draft.marketCode,
        name: draft.marketName.trim(),
        currency: store.currency,
      },
      (value) => {
        const saved = value as Market;
        setMarkets((rows) => [...rows, saved]);
        commitDraft(
          {
            ...draft,
            marketID: saved.id,
            methodObservation: null,
            policyObservation: null,
            serviceObservation: null,
          },
          3,
        );
        setNotice(c.saved);
      },
    );
  }
  function selectMethod(code: MethodCode) {
    const saved = methods.find((item) => item.code === code);
    const labels = names[code];
    const nextBase = {
      ...draft,
      methodCode: code,
      methodObservation: null,
    };
    const next =
      methodListTarget === `${draft.marketID}:TW`
        ? hydrateMethod(nextBase, saved ?? null)
        : {
            ...nextBase,
            nameHans: labels[0],
            nameHant: labels[1],
            nameEN: labels[2],
            visible: false,
            sort: "10",
            min: "1",
            max: "1000000000000",
          };
    try {
      saveDraft(next);
      setAvailability(null);
    } catch {
      storageError();
    }
  }
  function methodSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const sort = integer(draft.sort, 0, 1000),
      min = integer(draft.min, 1, 1000000000000),
      max = integer(draft.max, 1, 1000000000000);
    if (
      !activeMarket ||
      !activeAccount ||
      activeMarket.currency !== "TWD" ||
      draft.country !== "TW" ||
      sort === null ||
      min === null ||
      max === null ||
      min > max ||
      !draft.nameHans.trim() ||
      !draft.nameHant.trim() ||
      !draft.nameEN.trim()
    ) {
      setError({ ...unknown, code: "invalid_request" });
      return;
    }
    const binding = draft.methodBinding;
    if (
      !binding ||
      binding.connectionID !== activeAccount.id ||
      binding.bindingVersion !== activeAccount.binding_version ||
      binding.environment !== activeAccount.environment
    ) {
      setError({ ...unknown, code: "conflict" });
      return;
    }
    const expected = baseline(
      draft.methodObservation,
      methodTarget,
      activeMethod?.version ?? 0,
    );
    if (expected === null) return;
    const resource = `markets/${activeMarket.id}/countries/TW/payment-methods/${draft.methodCode}`;
    command(
      "PUT",
      resource,
      {
        market_id: activeMarket.id,
        country: "TW",
        code: draft.methodCode,
        environment: binding.environment,
        connection_id: binding.connectionID,
        binding_version: binding.bindingVersion,
        expected_version: expected,
        name_hans: draft.nameHans.trim(),
        name_hant: draft.nameHant.trim(),
        name_en: draft.nameEN.trim(),
        enabled: false,
        visible: draft.visible,
        sort_order: sort,
        min_amount_minor: min,
        max_amount_minor: max,
      },
      (value) => {
        const saved = value as Method;
        setMethods((rows) => [
          ...rows.filter((row) => row.code !== saved.code),
          saved,
        ]);
        commitDraft(
          {
            ...draft,
            methodObservation: {
              target: methodTarget,
              version: saved.version,
              dirty: false,
            },
          },
          4,
        );
        setNotice(c.methodSaved);
      },
    );
  }
  function policySubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const shipping = integer(draft.shipping, 0, 1000000000000),
      tax = integer(draft.taxRate, 0, 10000),
      ttl = integer(draft.ttl, 60, 1800);
    const ref = draft.reference.trim();
    if (
      !activeMarket ||
      !validCountry(draft.country) ||
      !validCode(draft.serviceCode) ||
      (draft.serviceKind !== "home" && draft.country !== "TW") ||
      shipping === null ||
      tax === null ||
      ttl === null ||
      (draft.taxMode === "none" && tax !== 0) ||
      !ref ||
      [...ref].length > 240 ||
      [...ref].some((char) => /[\x00-\x1f\x7f]/.test(char))
    ) {
      setError({ ...unknown, code: "invalid_request" });
      return;
    }
    const expected = baseline(
      draft.policyObservation,
      serviceTarget,
      policy?.version ?? 0,
    );
    if (expected === null) return;
    const resource = `markets/${activeMarket.id}/countries/${draft.country}/delivery-services/${draft.serviceCode}/policy`;
    command(
      "PUT",
      resource,
      {
        market_id: activeMarket.id,
        country: draft.country,
        method: `delivery:${draft.serviceCode}`,
        currency: activeMarket.currency,
        shipping_mode: "country_flat",
        shipping_minor: shipping,
        tax_mode: draft.taxMode,
        tax_basis: draft.taxBasis,
        tax_rate_bps: tax,
        quote_ttl_seconds: ttl,
        enabled: draft.policyEnabled,
        expected_version: expected,
        configuration_ref: ref,
      },
      (value) => {
        const saved = value as Policy;
        setPolicy(saved);
        saveDraft({
          ...draft,
          reference: "",
          policyObservation: {
            target: serviceTarget,
            version: saved.version,
            dirty: false,
          },
        });
        setNotice(c.policySaved);
      },
    );
  }
  function serviceSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const sort = integer(draft.sort, 0, 1000);
    if (
      !activeMarket?.active ||
      !policy?.enabled ||
      !validCode(draft.serviceCode) ||
      !validCountry(draft.country) ||
      (draft.serviceKind !== "home" && draft.country !== "TW") ||
      // API (ECPay) exists only for a CVS kind while the ECPay connection is enabled and checked.
      (draft.serviceMode === "API" && (draft.serviceKind === "home" || !ecpayReady)) ||
      sort === null ||
      !draft.nameHans.trim() ||
      !draft.nameHant.trim() ||
      !draft.nameEN.trim()
    ) {
      setError({ ...unknown, code: "invalid_request" });
      return;
    }
    const expected = baseline(
      draft.serviceObservation,
      serviceTarget,
      activeService?.version ?? 0,
    );
    if (
      expected === null ||
      baseline(draft.policyObservation, serviceTarget, policy.version) === null
    )
      return;
    const resource = `markets/${activeMarket.id}/countries/${draft.country}/delivery-services/${draft.serviceCode}`;
    command(
      "PUT",
      resource,
      {
        market_id: activeMarket.id,
        country: draft.country,
        code: draft.serviceCode,
        expected_version: expected,
        policy_version: draft.policyObservation!.version,
        name_hans: draft.nameHans.trim(),
        name_hant: draft.nameHant.trim(),
        name_en: draft.nameEN.trim(),
        delivery_kind: draft.serviceKind,
        mode: draft.serviceMode,
        enabled: draft.serviceEnabled,
        visible: draft.serviceVisible,
        sort_order: sort,
      },
      (value) => {
        const saved = value as Service;
        setServices((rows) => [
          ...rows.filter((row) => row.code !== saved.code),
          saved,
        ]);
        commitDraft(
          {
            ...draft,
            serviceObservation: {
              target: serviceTarget,
              version: saved.version,
              dirty: false,
            },
          },
          4,
        );
        setNotice(c.serviceSaved);
      },
    );
  }
  async function inspect() {
    if (!store || !activeMarket || !activeMethod || !ready || busyRef.current)
      return;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    setAvailability(null);
    const token = epoch.current;
    try {
      const csrf = csrfCookie();
      if (
        (await sessionBoundary(csrf)) !== boundary.current ||
        csrfCookie() !== csrf
      )
        throw new Error("session_changed");
      const response = await fetch(
        `/api/stores/${store.id}/markets/${activeMarket.id}/countries/TW/payment-methods/${activeMethod.code}/inspect`,
        {
          method: "POST",
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/json",
            "X-CSRF-Token": csrf,
          },
          body: JSON.stringify({
            market_id: activeMarket.id,
            country: "TW",
            code: activeMethod.code,
            expected_version: activeMethod.version,
            environment: activeMethod.environment,
            currency: activeMarket.currency,
            amount_minor: activeMethod.min_amount_minor,
          }),
        },
      );
      const value: unknown = await response.json();
      if (
        !mounted.current ||
        token !== epoch.current ||
        (await sessionBoundary()) !== boundary.current
      )
        return;
      if (!response.ok) setError(safeError(value));
      else setAvailability(value as Availability);
    } catch (value) {
      if (mounted.current)
        setError(
          value instanceof Error && value.message === "session_changed"
            ? { ...unknown, code: "session_changed" }
            : safeError(value),
        );
    } finally {
      busyRef.current = false;
      if (mounted.current) setBusy(false);
    }
  }
  const needsPolicySave =
    !policy ||
    draft.reference.trim() !== "" ||
    policy.shipping_minor !== Number(draft.shipping) ||
    policy.tax_mode !== draft.taxMode ||
    policy.tax_basis !== draft.taxBasis ||
    policy.tax_rate_bps !== Number(draft.taxRate) ||
    policy.quote_ttl_seconds !== Number(draft.ttl) ||
    policy.enabled !== draft.policyEnabled;
  const paymentNames = names[draft.methodCode];
  const selectedEnvironment = activeAccount?.environment ?? draft.environment;
  return (
    <WorkspaceFrame
      locale={locale}
      storeName={store?.name ?? c.noStore}
      active="settings"
      locked={busy}
    >
      <div className="settings-page" data-testid="settings-wizard">
        <div className="settings-heading">
          <h1>{c.title}</h1>
          <p>{c.subtitle}</p>
        </div>
        <ol className="settings-steps" aria-label={c.title}>
          {c.steps.map((label, index) => {
            const number = (index + 1) as 1 | 2 | 3 | 4;
            return (
              <li
                key={label}
                className={
                  number === step
                    ? "current"
                    : number < step
                      ? "complete"
                      : "future"
                }
              >
                <button
                  type="button"
                  disabled={!ready || busy || number > step}
                  aria-current={number === step ? "step" : undefined}
                  onClick={() => goStep(number)}
                >
                  <span className="step-number">{number}</span>
                  <span>{label}</span>
                </button>
              </li>
            );
          })}
        </ol>
        <div className="settings-surface">
          <section
            className="settings-form-area"
            aria-label={c.steps[step - 1]}
          >
            {loading && (
              <p className="message" role="status">
                {c.loading}
              </p>
            )}
            {error && (
              <div className="message error" role="alert">
                <strong>{displayError(error)}</strong>
                {error.request_id && (
                  <small>
                    {c.requestID}: {error.request_id}
                  </small>
                )}
              </div>
            )}
            {notice && (
              <p className="message pending" role="status">
                {notice}
              </p>
            )}
            {pending && !busy && (
              <div className="settings-pending" data-testid="settings-pending">
                <p>{c.unknown}</p>
                {pending.secret && (
                  <div className="settings-secret-retry">
                    <label>
                      {c.hashKey}
                      <input ref={hashKey} type="password" autoComplete="off" />
                    </label>
                    <label>
                      {c.hashIV}
                      <input ref={hashIV} type="password" autoComplete="off" />
                    </label>
                  </div>
                )}
                <button type="button" onClick={retryPending}>
                  {c.retry}
                </button>
              </div>
            )}
            {!store ? (
              <div className="settings-empty">
                <h2>{c.noStore}</h2>
                <p>{displayError(initial.error)}</p>
                <a href={`/${locale}/`}>{c.recoverWorkspace}</a>
              </div>
            ) : null}
            {store && step === 1 && (
              <>
                <h2 className="settings-section-title">{c.platformTitle}</h2>
                <fieldset
                  className="settings-platforms"
                  disabled={!ready || busy || !!pending}
                >
                  <legend className="sr-only">{c.platformTitle}</legend>
                  <label className={draft.branch === "payuni" ? "chosen" : ""}>
                    <input
                      type="radio"
                      name="branch"
                      aria-label={c.payuni}
                      checked={draft.branch === "payuni"}
                      onChange={() => changeBranch("payuni")}
                    />
                    <strong>{c.payuni}</strong>
                    <span>{c.payuniHint}</span>
                  </label>
                  <label className={draft.branch === "manual" ? "chosen" : ""}>
                    <input
                      type="radio"
                      name="branch"
                      aria-label={c.manual}
                      checked={draft.branch === "manual"}
                      onChange={() => changeBranch("manual")}
                    />
                    <strong>{c.manual}</strong>
                    <span>{c.manualHint}</span>
                  </label>
                </fieldset>
                <div className="settings-actions">
                  <button
                    className="primary"
                    type="button"
                    disabled={!ready || !!pending || busy}
                    onClick={() => goStep(2)}
                  >
                    {c.next}
                  </button>
                </div>
              </>
            )}
            {store &&
              step === 2 &&
              (draft.branch === "manual" ? (
                <>
                  <h2 className="settings-section-title">{c.manual}</h2>
                  <p className="settings-note">{c.manualHint}</p>
                  <LogisticsSettings store={store.id} locale={locale} />
                  <div className="settings-actions">
                    <button type="button" onClick={() => goStep(1)}>
                      {c.back}
                    </button>
                    <button
                      className="primary"
                      type="button"
                      disabled={!ready || !!pending || busy}
                      onClick={() => goStep(3)}
                    >
                      {c.next}
                    </button>
                  </div>
                </>
              ) : (
                <>
                  <h2 className="settings-section-title">{c.accountTitle}</h2>
                  <p className="settings-intro">{c.accountHint}</p>
                  <form
                    data-testid="settings-account-form"
                    onSubmit={accountSubmit}
                  >
                    <fieldset
                      disabled={!ready || busy || !!pending}
                      className="settings-fields"
                    >
                      <label>
                        {c.accountChoice}
                        <select
                          value={draft.accountID}
                          onChange={(event) => {
                            const selected = accounts.find(
                              (item) => item.id === event.target.value,
                            );
                            const next = withAccountBinding(
                              draft,
                              selected ?? null,
                            );
                            try {
                              saveDraft(next);
                              clearSecrets();
                            } catch {
                              storageError();
                            }
                          }}
                        >
                          <option value="">{c.newAccount}</option>
                          {accounts.map((item) => (
                            <option key={item.id} value={item.id}>
                              {item.account_id} · {item.environment} · v
                              {item.credential_version}
                            </option>
                          ))}
                        </select>
                      </label>
                      {accountCursor && (
                        <button
                          type="button"
                          disabled={busy || !!pending}
                          onClick={() => void moreAccounts()}
                        >
                          {c.loadMore}
                        </button>
                      )}
                      {activeAccount ? (
                        <>
                          <p className="settings-note">
                            {activeAccount.account_id} ·{" "}
                            {activeAccount.environment} · {c.saved} v
                            {activeAccount.credential_version}
                          </p>
                          <label className="settings-check">
                            <input
                              type="checkbox"
                              checked={draft.rotate}
                              onChange={(event) =>
                                update("rotate", event.target.checked)
                              }
                            />
                            {c.rotate}
                          </label>
                        </>
                      ) : (
                        <>
                          <fieldset className="settings-radio">
                            <legend>{c.environment}</legend>
                            <label>
                              <input
                                type="radio"
                                name="environment"
                                aria-label={c.sandbox}
                                checked={draft.environment === "SANDBOX"}
                                onChange={() =>
                                  update("environment", "SANDBOX")
                                }
                              />
                              <strong>{c.sandbox}</strong>
                              <small>{c.sandboxHint}</small>
                            </label>
                            <label>
                              <input
                                type="radio"
                                name="environment"
                                aria-label={c.live}
                                checked={draft.environment === "LIVE"}
                                onChange={() => update("environment", "LIVE")}
                              />
                              <strong>{c.live}</strong>
                              <small>{c.liveHint}</small>
                            </label>
                          </fieldset>
                          <label>
                            {c.merchantID}
                            <input
                              required
                              maxLength={120}
                              value={draft.merchantID}
                              onChange={(event) =>
                                update("merchantID", event.target.value)
                              }
                            />
                          </label>
                        </>
                      )}
                      {(!activeAccount || draft.rotate) && !pending?.secret && (
                        <>
                          <label>
                            {c.hashKey}
                            <input
                              ref={hashKey}
                              type="password"
                              required
                              autoComplete="off"
                            />
                          </label>
                          <label>
                            {c.hashIV}
                            <input
                              ref={hashIV}
                              type="password"
                              required
                              autoComplete="off"
                            />
                          </label>
                          <p className="settings-note">{c.secretHint}</p>
                        </>
                      )}
                      <p className="settings-warning settings-safety">
                        <svg
                          width="18"
                          height="18"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="1.7"
                          aria-hidden="true"
                        >
                          <circle cx="12" cy="12" r="9" />
                          <path d="M12 7v1m0 3v6" />
                        </svg>
                        <span>{c.accountUnverified}</span>
                      </p>
                      <div className="settings-actions">
                        <button type="button" onClick={() => goStep(1)}>
                          {c.back}
                        </button>
                        <button className="primary" type="submit">
                          {busy
                            ? c.pending
                            : activeAccount && !draft.rotate
                              ? c.next
                              : c.saveContinue}
                        </button>
                      </div>
                    </fieldset>
                  </form>
                </>
              ))}
            {store && step === 3 && (
              <>
                <h2 className="settings-section-title">{c.marketTitle}</h2>
                <div className="settings-fields">
                  <label>
                    {c.market}
                    <select
                      disabled={!ready || busy || !!pending}
                      value={draft.marketID}
                      onChange={(event) =>
                        update("marketID", event.target.value)
                      }
                    >
                      <option value="">{c.createMarket}</option>
                      {markets.map((item) => (
                        <option key={item.id} value={item.id}>
                          {item.name} · {item.code} · {item.currency}
                          {!item.active ? ` · ${c.marketInactive}` : ""}
                        </option>
                      ))}
                    </select>
                  </label>
                  {marketCursor && (
                    <button
                      type="button"
                      disabled={busy || !!pending}
                      onClick={() => void moreMarkets()}
                    >
                      {c.loadMore}
                    </button>
                  )}
                  {activeMarket && (
                    <button
                      type="button"
                      data-testid="settings-reload-current"
                      disabled={!ready || busy || !!pending}
                      onClick={() => void reloadCurrent()}
                    >
                      {c.reloadCurrent}
                    </button>
                  )}
                </div>
                {!activeMarket && (
                  <form
                    data-testid="settings-market-form"
                    onSubmit={marketSubmit}
                    className="settings-fields"
                  >
                    <label>
                      {c.marketCode}
                      <input
                        required
                        pattern="[a-z][a-z0-9_-]{0,39}"
                        value={draft.marketCode}
                        onChange={(event) =>
                          update("marketCode", event.target.value)
                        }
                      />
                    </label>
                    <label>
                      {c.marketName}
                      <input
                        required
                        maxLength={120}
                        value={draft.marketName}
                        onChange={(event) =>
                          update("marketName", event.target.value)
                        }
                      />
                    </label>
                    <p className="settings-note">
                      {c.currency}: {store.currency}
                    </p>
                    <div className="settings-actions">
                      <button
                        className="primary"
                        disabled={!ready || !!pending || busy}
                        type="submit"
                      >
                        {c.createMarket}
                      </button>
                    </div>
                  </form>
                )}
                {activeMarket && (
                  <>
                    {!activeMarket.active && (
                      <p className="message error">{c.marketInactive}</p>
                    )}
                    {draft.branch === "payuni" ? (
                      <>
                        <h2 className="settings-section-title settings-subtitle">
                          {c.methodTitle}
                        </h2>
                        <p className="settings-warning">{c.disabledReason}</p>
                        <form
                          data-testid="settings-payment-form"
                          onSubmit={methodSubmit}
                          className="settings-fields"
                        >
                          <label>
                            {c.method}
                            <select
                              value={draft.methodCode}
                              disabled={!ready || !!pending || busy}
                              onChange={(event) =>
                                selectMethod(event.target.value as MethodCode)
                              }
                            >
                              {methodCodes.map((code) => (
                                <option key={code} value={code}>
                                  {
                                    names[code][
                                      locale === "en"
                                        ? 2
                                        : locale === "zh-TW"
                                          ? 1
                                          : 0
                                    ]
                                  }
                                </option>
                              ))}
                            </select>
                          </label>
                          <div className="settings-field-grid">
                            <label>
                              {c.nameHans}
                              <input
                                required
                                maxLength={120}
                                value={draft.nameHans}
                                onChange={(event) =>
                                  update("nameHans", event.target.value)
                                }
                              />
                            </label>
                            <label>
                              {c.nameHant}
                              <input
                                required
                                maxLength={120}
                                value={draft.nameHant}
                                onChange={(event) =>
                                  update("nameHant", event.target.value)
                                }
                              />
                            </label>
                            <label>
                              {c.nameEN}
                              <input
                                required
                                maxLength={120}
                                value={draft.nameEN}
                                onChange={(event) =>
                                  update("nameEN", event.target.value)
                                }
                              />
                            </label>
                            <label>
                              {c.sort}
                              <input
                                type="number"
                                min="0"
                                max="1000"
                                required
                                value={draft.sort}
                                onChange={(event) =>
                                  update("sort", event.target.value)
                                }
                              />
                            </label>
                            <label>
                              {c.minAmount}
                              <input
                                type="number"
                                min="1"
                                required
                                value={draft.min}
                                onChange={(event) =>
                                  update("min", event.target.value)
                                }
                              />
                            </label>
                            <label>
                              {c.maxAmount}
                              <input
                                type="number"
                                min="1"
                                required
                                value={draft.max}
                                onChange={(event) =>
                                  update("max", event.target.value)
                                }
                              />
                            </label>
                          </div>
                          <label className="settings-check">
                            <input
                              type="checkbox"
                              checked={draft.visible}
                              onChange={(event) =>
                                update("visible", event.target.checked)
                              }
                            />
                            {c.visible}
                          </label>
                          <p className="settings-note">
                            {c.enabled}: {c.notAvailable}. {c.providerGate}
                          </p>
                          <div className="settings-actions">
                            <button type="button" onClick={() => goStep(2)}>
                              {c.back}
                            </button>
                            <button
                              className="primary"
                              type="submit"
                              disabled={
                                !ready ||
                                !!pending ||
                                busy ||
                                !activeAccount ||
                                store.currency !== "TWD"
                              }
                            >
                              {busy ? c.pending : c.saveMethod}
                            </button>
                          </div>
                        </form>
                      </>
                    ) : (
                      <>
                        <h2 className="settings-section-title settings-subtitle">
                          {c.deliveryTitle}
                        </h2>
                        <p className="settings-note">{c.sequential}</p>
                        <div className="settings-fields">
                          <label>
                            {c.country}
                            <input
                              maxLength={2}
                              pattern="[A-Z]{2}"
                              value={draft.country}
                              onChange={(event) =>
                                update(
                                  "country",
                                  event.target.value.toUpperCase(),
                                )
                              }
                            />
                          </label>
                          <label>
                            {c.service}
                            <select
                              value={activeService?.code ?? ""}
                              onChange={(event) => {
                                const selected = services.find(
                                  (item) => item.code === event.target.value,
                                );
                                if (selected) {
                                  const next = {
                                    ...draft,
                                    serviceCode: selected.code,
                                    policyObservation: null,
                                  };
                                  try {
                                    setPolicy(null);
                                    saveDraft(hydrateService(next, selected));
                                  } catch {
                                    storageError();
                                  }
                                } else update("serviceCode", "");
                              }}
                            >
                              <option value="">{c.createService}</option>
                              {services.map((item) => (
                                <option key={item.code} value={item.code}>
                                  {item.code}
                                </option>
                              ))}
                            </select>
                          </label>
                          {serviceCursor && (
                            <button
                              type="button"
                              disabled={busy || !!pending}
                              onClick={() => void moreServices()}
                            >
                              {c.loadMore}
                            </button>
                          )}
                          <label>
                            {c.serviceCode}
                            <input
                              required
                              pattern="[a-z][a-z0-9_-]{0,39}"
                              maxLength={40}
                              value={draft.serviceCode}
                              onChange={(event) =>
                                update("serviceCode", event.target.value)
                              }
                            />
                          </label>
                          <label>
                            {c.deliveryKind}
                            <select
                              value={draft.serviceKind}
                              onChange={(event) =>
                                update(
                                  "serviceKind",
                                  event.target.value as Draft["serviceKind"],
                                )
                              }
                            >
                              <option value="home">{c.home}</option>
                              <option value="cvs_711">{c.cvs711}</option>
                              <option value="cvs_familymart">
                                {c.cvsFamily}
                              </option>
                              <option value="cvs_hilife">{lc.chains.cvs_hilife}</option>
                              <option value="cvs_okmart">{lc.chains.cvs_okmart}</option>
                            </select>
                          </label>
                          <label>
                            {lc.modeLabel}
                            <select
                              data-testid="settings-service-mode"
                              value={draft.serviceMode}
                              disabled={draft.serviceKind === "home"}
                              onChange={(event) =>
                                update(
                                  "serviceMode",
                                  event.target.value as Draft["serviceMode"],
                                )
                              }
                            >
                              <option value="MANUAL">{lc.modeManual}</option>
                              {(ecpayReady || draft.serviceMode === "API") &&
                                draft.serviceKind !== "home" && (
                                  <option value="API" disabled={!ecpayReady}>
                                    {lc.modeApi}
                                  </option>
                                )}
                            </select>
                            <small>{lc.modeApiHint}</small>
                          </label>
                        </div>
                        <form
                          data-testid="settings-policy-form"
                          onSubmit={policySubmit}
                          className="settings-fields settings-subform"
                        >
                          <div className="settings-field-grid">
                            <label>
                              {c.shipping}
                              <input
                                type="number"
                                min="0"
                                required
                                value={draft.shipping}
                                onChange={(event) =>
                                  update("shipping", event.target.value)
                                }
                              />
                            </label>
                            <label>
                              {c.taxMode}
                              <select
                                value={draft.taxMode}
                                onChange={(event) =>
                                  update(
                                    "taxMode",
                                    event.target.value as Draft["taxMode"],
                                  )
                                }
                              >
                                <option value="none">{c.taxNone}</option>
                                <option value="inclusive">
                                  {c.taxInclusive}
                                </option>
                                <option value="exclusive">
                                  {c.taxExclusive}
                                </option>
                              </select>
                            </label>
                            <label>
                              {c.taxBasis}
                              <select
                                value={draft.taxBasis}
                                onChange={(event) =>
                                  update(
                                    "taxBasis",
                                    event.target.value as Draft["taxBasis"],
                                  )
                                }
                              >
                                <option value="goods">{c.goods}</option>
                                <option value="goods_and_shipping">
                                  {c.goodsShipping}
                                </option>
                              </select>
                            </label>
                            <label>
                              {c.taxRate}
                              <input
                                type="number"
                                min="0"
                                max="10000"
                                required
                                value={draft.taxRate}
                                onChange={(event) =>
                                  update("taxRate", event.target.value)
                                }
                              />
                            </label>
                            <label>
                              {c.ttl}
                              <input
                                type="number"
                                min="60"
                                max="1800"
                                required
                                value={draft.ttl}
                                onChange={(event) =>
                                  update("ttl", event.target.value)
                                }
                              />
                            </label>
                          </div>
                          <label className="settings-check">
                            <input
                              type="checkbox"
                              checked={draft.policyEnabled}
                              onChange={(event) =>
                                update("policyEnabled", event.target.checked)
                              }
                            />
                            {c.policyEnabled}
                          </label>
                          <label>
                            {c.ref}
                            <input
                              required={needsPolicySave}
                              maxLength={240}
                              value={draft.reference}
                              onChange={(event) =>
                                update("reference", event.target.value)
                              }
                            />
                            <small>{c.refHint}</small>
                          </label>
                          {needsPolicySave && (
                            <div className="settings-actions">
                              <button
                                className="primary"
                                disabled={
                                  !ready ||
                                  !!pending ||
                                  busy ||
                                  !activeMarket.active
                                }
                                type="submit"
                              >
                                {busy ? c.pending : c.savePolicy}
                              </button>
                            </div>
                          )}
                        </form>
                        {!needsPolicySave && (
                          <form
                            data-testid="settings-service-form"
                            onSubmit={serviceSubmit}
                            className="settings-fields settings-subform"
                          >
                            <div className="settings-field-grid">
                              <label>
                                {c.nameHans}
                                <input
                                  required
                                  maxLength={120}
                                  value={draft.nameHans}
                                  onChange={(event) =>
                                    update("nameHans", event.target.value)
                                  }
                                />
                              </label>
                              <label>
                                {c.nameHant}
                                <input
                                  required
                                  maxLength={120}
                                  value={draft.nameHant}
                                  onChange={(event) =>
                                    update("nameHant", event.target.value)
                                  }
                                />
                              </label>
                              <label>
                                {c.nameEN}
                                <input
                                  required
                                  maxLength={120}
                                  value={draft.nameEN}
                                  onChange={(event) =>
                                    update("nameEN", event.target.value)
                                  }
                                />
                              </label>
                              <label>
                                {c.sort}
                                <input
                                  type="number"
                                  min="0"
                                  max="1000"
                                  required
                                  value={draft.sort}
                                  onChange={(event) =>
                                    update("sort", event.target.value)
                                  }
                                />
                              </label>
                            </div>
                            <label className="settings-check">
                              <input
                                type="checkbox"
                                checked={draft.serviceEnabled}
                                onChange={(event) =>
                                  update("serviceEnabled", event.target.checked)
                                }
                              />
                              {c.serviceEnabled}
                            </label>
                            <label className="settings-check">
                              <input
                                type="checkbox"
                                checked={draft.serviceVisible}
                                onChange={(event) =>
                                  update("serviceVisible", event.target.checked)
                                }
                              />
                              {c.visible}
                            </label>
                            <p className="settings-note">{c.prereq}</p>
                            <div className="settings-actions">
                              <button type="button" onClick={() => goStep(2)}>
                                {c.back}
                              </button>
                              <button
                                className="primary"
                                type="submit"
                                disabled={
                                  !ready ||
                                  !!pending ||
                                  busy ||
                                  !policy?.enabled ||
                                  !activeMarket.active
                                }
                              >
                                {busy ? c.pending : c.saveService}
                              </button>
                            </div>
                          </form>
                        )}
                      </>
                    )}
                  </>
                )}
              </>
            )}
            {store && step === 4 && (
              <div data-testid="settings-status">
                <h2 className="settings-section-title">{c.statusTitle}</h2>
                <p className="settings-note">
                  {draft.branch === "payuni"
                    ? c.accountUnverified
                    : c.manualHint}
                </p>
                <dl className="settings-status-list">
                  <div>
                    <dt>{c.platform}</dt>
                    <dd>{draft.branch === "payuni" ? c.payuni : c.manual}</dd>
                  </div>
                  <div>
                    <dt>{c.market}</dt>
                    <dd>
                      {activeMarket
                        ? `${activeMarket.name} · ${activeMarket.currency}`
                        : c.notConfigured}
                    </dd>
                  </div>
                  {draft.branch === "payuni" && (
                    <div>
                      <dt>{c.account}</dt>
                      <dd>
                        {activeAccount
                          ? `${activeAccount.account_id} · ${activeAccount.state === "CONFIGURED_UNVERIFIED" ? c.savedUnverified : c.notChecked} · v${activeAccount.credential_version}`
                          : c.notConfigured}
                      </dd>
                    </div>
                  )}
                  <div>
                    <dt>{draft.branch === "payuni" ? c.payment : c.service}</dt>
                    <dd>
                      {draft.branch === "payuni"
                        ? activeMethod
                          ? `${paymentNames[locale === "en" ? 2 : locale === "zh-TW" ? 1 : 0]} · ${activeMethod.enabled ? c.enabled : c.notAvailable} · ${activeMethod.visible ? c.visible : c.notConfigured}`
                          : c.notConfigured
                        : activeService
                          ? `${activeService.code} · ${activeService.enabled ? c.enabled : c.notAvailable} · ${activeService.visible ? c.visible : c.notConfigured}`
                          : c.notConfigured}
                    </dd>
                  </div>
                  {draft.branch === "manual" && (
                    <div>
                      <dt>{c.policyEnabled}</dt>
                      <dd>
                        {policy
                          ? `v${policy.version} · ${policy.enabled ? c.enabled : c.notAvailable}`
                          : c.notConfigured}
                      </dd>
                    </div>
                  )}
                  {draft.branch === "payuni" && (
                    <div>
                      <dt>{c.qualification}</dt>
                      <dd>{c.notApproved}</dd>
                    </div>
                  )}
                  {draft.branch === "payuni" && (
                    <div>
                      <dt>{c.runtime}</dt>
                      <dd>
                        {availability
                          ? `${availability.available ? c.enabled : c.notAvailable}${availability.reasons.length ? ` · ${availability.reasons.map((reason) => availabilityReason(locale, reason)).join(", ")}` : ""}`
                          : c.notChecked}
                      </dd>
                    </div>
                  )}
                </dl>
                <div className="settings-actions">
                  <button type="button" onClick={() => goStep(3)}>
                    {c.back}
                  </button>
                  {draft.branch === "payuni" && activeMethod && (
                    <button
                      type="button"
                      disabled={busy || !!pending}
                      onClick={() => void inspect()}
                    >
                      {c.inspect}
                    </button>
                  )}
                  <button
                    type="button"
                    className="primary"
                    disabled={busy}
                    onClick={() => {
                      void loadBase();
                      if (draft.marketID)
                        void loadMarket(draft.marketID, draft.country);
                    }}
                  >
                    {c.refresh}
                  </button>
                </div>
              </div>
            )}
          </section>
          <aside className="settings-status-column" aria-label={c.statusTitle}>
            <h2>{c.statusTitle}</h2>
            <dl>
              <div>
                <dt>{c.platform}</dt>
                <dd>{draft.branch === "payuni" ? c.payuni : c.manual}</dd>
              </div>
              <div>
                <dt>{c.market}</dt>
                <dd>
                  {activeMarket
                    ? `${activeMarket.name} · ${activeMarket.currency}`
                    : c.notConfigured}
                </dd>
              </div>
              {draft.branch === "payuni" && (
                <div>
                  <dt>{c.account}</dt>
                  <dd>
                    {activeAccount
                      ? `${activeAccount.account_id} · ${activeAccount.state === "CONFIGURED_UNVERIFIED" ? c.savedUnverified : c.notChecked}`
                      : c.notConfigured}
                  </dd>
                </div>
              )}
              <div>
                <dt>{draft.branch === "payuni" ? c.payment : c.service}</dt>
                <dd>
                  {draft.branch === "payuni"
                    ? activeMethod
                      ? activeMethod.enabled
                        ? c.enabled
                        : c.notAvailable
                      : c.notConfigured
                    : activeService
                      ? `${activeService.code} · ${activeService.enabled ? c.enabled : c.notAvailable} · ${activeService.visible ? c.visible : c.notConfigured}`
                      : c.notConfigured}
                </dd>
              </div>
              {draft.branch === "manual" && (
                <div>
                  <dt>{c.policyEnabled}</dt>
                  <dd>
                    {policy
                      ? `v${policy.version} · ${policy.enabled ? c.enabled : c.notAvailable}`
                      : c.notConfigured}
                  </dd>
                </div>
              )}
              {draft.branch === "payuni" && (
                <div>
                  <dt>{c.qualification}</dt>
                  <dd>{c.notApproved}</dd>
                </div>
              )}
              {draft.branch === "payuni" && (
                <div>
                  <dt>{c.runtime}</dt>
                  <dd>
                    {availability
                      ? availability.available
                        ? c.enabled
                        : c.notAvailable
                      : c.notChecked}
                  </dd>
                </div>
              )}
            </dl>
            <div className="settings-status-next">
              <h3>{c.next}</h3>
              <p>
                {step === 1
                  ? c.payuniHint
                  : step === 2
                    ? c.accountNext
                    : step === 3
                      ? draft.branch === "payuni"
                        ? c.disabledReason
                        : c.prereq
                      : c.providerGate}
              </p>
            </div>
          </aside>
        </div>
      </div>
    </WorkspaceFrame>
  );
}
