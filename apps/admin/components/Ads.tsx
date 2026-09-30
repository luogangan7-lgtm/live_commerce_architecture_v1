"use client";

// Merchant ads page: Meta connection, draft list + detail, draft form, report, CAPI setting (contracts/meta-ads-v1.md §2, §5, §7).
// BFF routes, all -> Go internal/httpapi/ads.go under /v1/admin/stores/{store_id}/ads (lib/ads-client.ts is the only caller):
//   GET  /api/stores/{store}/ads/settings | drafts | drafts/{id} | meta/states/{id} | report?from&to   -> GET  ads/…
//   POST /api/stores/{store}/ads/meta/bindings | drafts | drafts/{id}/(approve|publish|pause|end)     -> POST ads/…
//   PUT  /api/stores/{store}/ads/drafts/{id} (If-Match) | capi                                        -> PUT  ads/…
//   POST /api/ads/meta/connect (app/api/ads/meta/connect/route.ts)                                    -> POST ads/meta/connect
//   the return from Meta lands on /api/ads/meta/callback, which 303s back here with ?connect=<state_id>.
// Rules kept here: no token is ever visible (Go holds it sealed); nothing is optimistic (every write is followed by a
// re-GET); one Idempotency-Key per dialog open, reused only for a byte-identical retry after an unknown outcome;
// a paused draft is never resumed (X7: copy into a new draft); spend is never described as a hard stop (§12).
import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import type { Locale } from "@live-commerce/i18n";
import type { Store } from "@/lib/model";
import { sessionBoundary } from "@/lib/settings-client";
import {
  newKey, postBindings, postConnect, postDraft, postDraftAction, putCapi, putDraft, readConnectState, readDraft, readDrafts,
  readReport, readSettings, AdsReadError, type WriteResult,
} from "@/lib/ads-client";
import {
  accountReady, adsManagerHref, buildDraftInput, canApprove, canCopy, canEdit, canEnd, canPause, canPublish, capiBody, copyForm,
  emptyForm, formFromDraft, formatMinor, hasRemote, localDate, maxReportDays, validCapi, validReportWindow, dayMs,
  type AdsCode, type ConnectError, type ConnectState, type Draft, type DraftForm as FormState, type PickItem, type Report, type Settings,
  type Template,
} from "@/lib/ads-model";
import { adsCopy, errorText, type AdsCopy } from "@/lib/ads-copy";
import { WorkspaceFrame } from "./WorkspaceFrame";
import { Icon } from "./Icon";
import "./ads.css";

export type AdsInitialError = "signed-out" | "forbidden" | "unavailable";
type Status = "loading" | "ready" | AdsInitialError;
type Banner = { kind: "ok" | "bad"; text: string } | null;
type Mode = { kind: "none" } | { kind: "new" | "copy"; form: FormState } | { kind: "edit"; form: FormState; draft: Draft };

function when(locale: Locale, iso: string | null, empty: string) {
  if (!iso) return empty;
  const t = Date.parse(iso);
  return Number.isFinite(t) ? new Date(t).toLocaleString(locale, { dateStyle: "medium", timeStyle: "short" }) : empty;
}
const short = (id: string) => `${id.slice(0, 4)}…${id.slice(-4)}`;

export function Ads({
  locale, stores, store, connect, connectError, draft: initialDraft, initialError,
}: {
  locale: Locale; stores: Store[]; store: Store | null; connect: string; connectError: ConnectError | "";
  draft: string; initialError: AdsInitialError | null;
}) {
  const c = adsCopy[locale];
  const router = useRouter();
  const [status, setStatus] = useState<Status>(initialError ?? "loading");
  const [settings, setSettings] = useState<Settings | null>(null);
  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [selected, setSelected] = useState(initialDraft);
  const [detail, setDetail] = useState<Draft | null>(null);
  const [tick, setTick] = useState(0);
  const [banner, setBanner] = useState<Banner>(null);
  const [busy, setBusy] = useState("");
  const [uncertain, setUncertain] = useState("");
  const [mode, setMode] = useState<Mode>({ kind: "none" });
  const [problems, setProblems] = useState<AdsCode[]>([]);
  const [confirmEnd, setConfirmEnd] = useState(false);
  const boundary = useRef("");
  const blocked = useRef(false);
  const keys = useRef(new Map<string, { key: string; body: string }>());
  const last = useRef<(() => Promise<void>) | null>(null);
  const reload = useCallback(() => setTick((n) => n + 1), []);

  // Load settings + drafts (+ selected draft). Session cookie hash is the change fence for later writes.
  useEffect(() => {
    if (!store || initialError) return;
    const active = new AbortController();
    (async () => {
      try {
        boundary.current = await sessionBoundary();
        const [s, list] = await Promise.all([readSettings(store.id, active.signal), readDrafts(store.id, active.signal)]);
        if (active.signal.aborted) return;
        setSettings(s);
        setDrafts(list);
        setStatus("ready");
      } catch (error) {
        if (active.signal.aborted) return;
        const code = error instanceof AdsReadError ? error.code : "unavailable";
        if (code === "signed-out") blocked.current = true;
        setStatus(code === "signed-out" ? "signed-out" : code === "forbidden" ? "forbidden" : "unavailable");
      }
    })();
    return () => active.abort();
  }, [store, initialError, tick]);

  useEffect(() => {
    if (!store || !selected || status !== "ready") {
      setDetail(null);
      return;
    }
    const active = new AbortController();
    readDraft(store.id, selected, active.signal).then(
      (d) => !active.signal.aborted && setDetail(d),
      () => !active.signal.aborted && setDetail(null),
    );
    return () => active.abort();
  }, [store, selected, status, tick]);

  // Sign-out in another tab: drop everything (same events as the orders page).
  useEffect(() => {
    const out = () => {
      blocked.current = true;
      setStatus("signed-out");
      setSettings(null);
      setDrafts([]);
      setDetail(null);
    };
    let channel: BroadcastChannel | null = null;
    const onMessage = (event: MessageEvent) => event.data?.type === "logout" && out();
    const onStorage = (event: StorageEvent) => event.key === "commerce-session-logout" && out();
    try {
      channel = new BroadcastChannel("commerce-session");
      channel.addEventListener("message", onMessage);
    } catch {
      /* storage event still works */
    }
    window.addEventListener("storage", onStorage);
    window.addEventListener("commerce-session-logout", out);
    return () => {
      channel?.removeEventListener("message", onMessage);
      channel?.close();
      window.removeEventListener("storage", onStorage);
      window.removeEventListener("commerce-session-logout", out);
    };
  }, []);

  const url = useCallback((extra: Record<string, string> = {}) => {
    const p = new URLSearchParams();
    if (store) p.set("store", store.id);
    for (const [k, v] of Object.entries(extra)) if (v) p.set(k, v);
    return `/${locale}/ads${p.size ? `?${p}` : ""}`;
  }, [locale, store]);

  // One key per submission; the same key only comes back for the identical slot + bytes (unknown-outcome retry).
  function keyFor(slot: string, body: string) {
    const held = keys.current.get(slot);
    if (held && held.body === body) return held.key;
    const key = newKey("ads");
    keys.current.set(slot, { key, body });
    return key;
  }
  /** Run one write. Definitive failure drops the key (a new attempt is a new command); an unknown outcome keeps it. */
  async function run<T>(slot: string, body: string, exec: (key: string) => Promise<WriteResult<T>>, okText: string, after?: (value: T) => void) {
    if (busy || blocked.current) return;
    const go = async () => {
      setBusy(slot);
      setBanner(null);
      const result = await exec(keyFor(slot, body));
      setBusy("");
      if (result.ok) {
        keys.current.delete(slot);
        setUncertain("");
        last.current = null;
        setBanner(okText ? { kind: "ok", text: okText } : null);
        after?.(result.value);
        reload();
        return;
      }
      if (result.uncertain) {
        setUncertain(slot);
        setBanner({ kind: "bad", text: c.uncertain });
        return;
      }
      keys.current.delete(slot);
      setUncertain("");
      last.current = null;
      if (result.code === "unauthorized") {
        blocked.current = true;
        setStatus("signed-out");
      }
      setBanner({ kind: "bad", text: errorText(c, result.code) });
      if (result.code === "revision_changed" || result.code === "attempt_changed" || result.code === "draft_approved") reload();
    };
    last.current = go;
    await go();
  }
  const retrySame = () => void last.current?.();

  async function startConnect() {
    if (!store || busy) return;
    await run("connect", "connect", (key) => postConnect(store.id, key, boundary.current), "", (dialog) => {
      // postConnect already proved the origin is exactly https://www.facebook.com.
      if (dialog) window.location.assign(dialog);
    });
  }

  const allowanceOff = !!settings && settings.max_active_budget_minor === 0;
  const selectedDraft = detail && detail.id === selected ? detail : null;

  function openNew() {
    if (!settings || !store) return;
    keys.current.delete("draft-new");
    setProblems([]);
    setBanner(null);
    setUncertain("");
    setMode({ kind: "new", form: emptyForm(settings.allowance_currency || store.currency) });
  }
  function openEdit(d: Draft) {
    keys.current.delete(`draft-edit:${d.id}`);
    setProblems([]);
    setBanner(null);
    setUncertain("");
    setMode({ kind: "edit", form: formFromDraft(d), draft: d });
  }
  function openCopy(d: Draft) {
    keys.current.delete("draft-new");
    setProblems([]);
    setBanner(null);
    setUncertain("");
    setMode({ kind: "copy", form: copyForm(d) });
  }
  function closeForm() {
    if (mode.kind === "edit") keys.current.delete(`draft-edit:${mode.draft.id}`);
    else keys.current.delete("draft-new");
    setMode({ kind: "none" });
    setUncertain("");
    last.current = null;
    reload();
  }
  async function submitDraft(form: FormState) {
    if (!store || !settings) return;
    const built = buildDraftInput(form, Date.now(), settings.allowance_currency);
    if (!built.ok) {
      setProblems(built.codes);
      return;
    }
    setProblems([]);
    const body = JSON.stringify(built.input);
    if (mode.kind === "edit") {
      const target = mode.draft;
      await run(`draft-edit:${target.id}`, body, (key) => putDraft(store.id, target.id, key, body, boundary.current, target.revision), c.saved, () => setMode({ kind: "none" }));
    } else {
      await run("draft-new", body, (key) => postDraft(store.id, key, body, boundary.current), c.saved, (created) => {
        setMode({ kind: "none" });
        if (created) setSelected(created.id);
      });
    }
  }
  function act(d: Draft, action: "approve" | "publish" | "pause" | "end") {
    if (!store) return;
    const body = JSON.stringify(action === "approve" ? { revision: d.revision } : action === "publish" ? { publish_attempt: d.publish_attempt } : {});
    const text = action === "approve" ? c.approved : action === "publish" ? c.published : action === "pause" ? c.paused : c.ended;
    setConfirmEnd(false);
    void run(`${action}:${d.id}`, body, (key) => postDraftAction(store.id, d.id, action, key, body, boundary.current), text);
  }

  const bar = (
    <div className="ads-controls">
      {stores.length > 1 && (
        <label>
          {c.store}
          <select data-testid="store-selector" value={store?.id ?? ""} onChange={(e) => router.push(`/${locale}/ads?store=${e.target.value}`)}>
            {stores.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
        </label>
      )}
      <button type="button" data-testid="ads-refresh" disabled={status === "loading"} onClick={() => { blocked.current = false; setStatus("loading"); reload(); }}>
        <Icon name="refresh" size={18} />{c.refresh}
      </button>
    </div>
  );

  return (
    <WorkspaceFrame locale={locale} storeName={store?.name ?? c.noStore} active="ads">
      <div className="ads-page" data-testid="merchant-ads">
        <header className="ads-heading">
          <h1>{c.title}</h1>
          <p>{c.subtitle}</p>
        </header>
        {bar}
        {status === "loading" && <p className="ads-message" role="status">{c.loading}</p>}
        {(status === "signed-out" || status === "forbidden" || status === "unavailable") && (
          <div className="ads-message" role="status" data-testid="ads-error">
            <p>{!store && status === "unavailable" ? c.noStore : status === "signed-out" ? c.signedOut : status === "forbidden" ? c.forbidden : c.unavailable}</p>
            <button type="button" onClick={() => (initialError || !store ? router.refresh() : (blocked.current = false, setStatus("loading"), reload()))}>{c.retry}</button>
          </div>
        )}
        {status === "ready" && settings && store && (
          <>
            {settings.environment === "SANDBOX" && <p className="ads-banner ads-banner-sandbox" role="status" data-testid="ads-sandbox">{c.sandboxBanner}</p>}
            {allowanceOff
              ? <p className="ads-banner ads-banner-off" role="status" data-testid="ads-allowance-off">{c.allowanceOff}</p>
              : <p className="ads-note" data-testid="ads-allowance">{c.allowanceLine(formatMinor(locale, settings.allowance_currency, settings.max_active_budget_minor))}</p>}
            <p className="ads-note" data-testid="ads-budget-note">{c.budgetNote}</p>
            {banner && (
              <p className={banner.kind === "ok" ? "ads-notice" : "ads-bad"} role={banner.kind === "ok" ? "status" : "alert"} data-testid="ads-banner">
                {banner.text}
                {uncertain && banner.kind === "bad" && <button type="button" className="ads-inline" data-testid="ads-retry-same" onClick={retrySame}>{c.retrySame}</button>}
              </p>
            )}
            <ConnectionSection c={c} locale={locale} settings={settings} store={store} connect={connect} connectError={connectError}
              busy={busy} uncertain={uncertain} startConnect={startConnect} run={run} boundary={boundary}
              done={() => { setBanner({ kind: "ok", text: c.connected }); router.replace(url()); reload(); }}
              startAgain={() => { router.replace(url()); void startConnect(); }} />
            <section className="ads-section" aria-labelledby="ads-drafts-h">
              <div className="ads-section-head">
                <h2 id="ads-drafts-h">{c.draftsTitle}</h2>
                <button type="button" className="primary" data-testid="ads-new-draft" onClick={openNew} disabled={mode.kind !== "none"}>
                  <Icon name="plus" size={16} />{c.newDraft}
                </button>
              </div>
              {mode.kind !== "none" && (
                <DraftFormPanel key={mode.kind + (mode.kind === "edit" ? mode.draft.id : "")} c={c} settings={settings} mode={mode}
                  problems={problems} busy={busy.startsWith("draft-")} uncertain={uncertain.startsWith("draft-")}
                  onSubmit={submitDraft} onCancel={closeForm} onRetry={retrySame} />
              )}
              {drafts.length === 0 ? <p className="ads-empty" data-testid="ads-drafts-empty">{c.draftsEmpty}</p> : (
                <div className="ads-table-scroll">
                  <table className="ads-table" data-testid="ads-drafts">
                    <thead><tr><th>{c.colTemplate}</th><th>{c.colBudget}</th><th>{c.colSchedule}</th><th>{c.colStatus}</th><th>{c.colCreated}</th></tr></thead>
                    <tbody>
                      {drafts.map((d) => (
                        <tr key={d.id} className={d.id === selected ? "ads-selected" : ""} data-testid={`ads-draft-${d.id}`}>
                          <td data-label={c.colTemplate}>
                            <button type="button" aria-expanded={d.id === selected} data-testid={`ads-open-${d.id}`}
                              aria-label={`${c.select}: ${c.templates[d.template]} ${short(d.id)}`}
                              onClick={() => { setConfirmEnd(false); setSelected(d.id === selected ? "" : d.id); window.history.replaceState(null, "", url({ draft: d.id === selected ? "" : d.id })); }}>
                              <Icon name="chevron" size={16} style={{ transform: d.id === selected ? "rotate(90deg)" : undefined }} />
                              <span>{c.templates[d.template]}</span>
                            </button>
                          </td>
                          <td data-label={c.colBudget}>{formatMinor(locale, d.currency, d.lifetime_budget_minor)}</td>
                          <td data-label={c.colSchedule}>{when(locale, d.starts_at, "—")} → {when(locale, d.ends_at, "—")}</td>
                          <td data-label={c.colStatus}><Badge c={c} status={d.status} /></td>
                          <td data-label={c.colCreated}>{when(locale, d.created_at, "—")}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
              {selectedDraft && (
                <DraftDetail c={c} locale={locale} d={selectedDraft} busy={busy} allowanceOff={allowanceOff} confirmEnd={confirmEnd}
                  setConfirmEnd={setConfirmEnd} onEdit={() => openEdit(selectedDraft)} onCopy={() => openCopy(selectedDraft)} act={act} formOpen={mode.kind !== "none"} />
              )}
            </section>
            <ReportSection c={c} locale={locale} store={store} />
            <CapiSection c={c} settings={settings} busy={busy === "capi"} uncertain={uncertain === "capi"} onRetry={retrySame}
              save={(enabled, dataset, test) => {
                const body = JSON.stringify(capiBody(enabled, dataset, test));
                return run("capi", body, (key) => putCapi(store.id, key, body, boundary.current), c.capiSaved);
              }} />
          </>
        )}
      </div>
    </WorkspaceFrame>
  );
}

function Badge({ c, status }: { c: AdsCopy; status: Draft["status"] }) {
  return <span className={`ads-badge ads-badge-${status.toLowerCase()}`} data-state={status}>{c.statuses[status]}</span>;
}

// ---------- connection ----------
function ConnectionSection({
  c, locale, settings, store, connect, connectError, busy, uncertain, startConnect, run, boundary, done, startAgain,
}: {
  c: AdsCopy; locale: Locale; settings: Settings; store: Store; connect: string; connectError: ConnectError | "";
  busy: string; uncertain: string; startConnect: () => Promise<void>; boundary: { current: string }; done: () => void; startAgain: () => void;
  run: <T>(slot: string, body: string, exec: (key: string) => Promise<WriteResult<T>>, okText: string, after?: (value: T) => void) => Promise<void>;
}) {
  const accounts = settings.connections.filter((x) => x.provider === "meta_ads");
  const datasets = settings.connections.filter((x) => x.provider === "meta_dataset");
  return (
    <section className="ads-section" aria-labelledby="ads-conn-h" data-testid="ads-connection">
      <div className="ads-section-head">
        <h2 id="ads-conn-h">{c.connTitle}</h2>
        <button type="button" className="primary" data-testid="ads-connect" disabled={busy !== "" || !!connect} onClick={() => void startConnect()}>
          {busy === "connect" ? c.connecting : c.connectButton}
        </button>
      </div>
      <p className="ads-note">{c.connectHint}</p>
      {connectError && (
        <div className="ads-bad" role="alert" data-testid="ads-connect-error">
          {c.connectErrors[connectError]} <button type="button" className="ads-inline" onClick={startAgain}>{c.startAgain}</button>
        </div>
      )}
      {connect && <PickStep c={c} store={store} stateID={connect} busy={busy} uncertain={uncertain} run={run} boundary={boundary} done={done} startAgain={startAgain} />}
      {accounts.length === 0 && datasets.length === 0 ? <p className="ads-empty" data-testid="ads-conn-empty">{c.connEmpty}</p> : (
        <ul className="ads-list" data-testid="ads-connections">
          {[...accounts, ...datasets].map((x) => (
            <li key={x.binding_id}>
              <strong>{c.providers[x.provider] ?? x.provider}</strong> <span className="ads-mono">{x.asset_id}</span>
              <small>{c.connBusiness}: <span className="ads-mono">{x.client_business_id}</span> · {c.connAt}: {when(locale, x.connected_at, "—")}</small>
              <span className={x.enabled ? "ads-tone-ok" : "ads-tone-warn"}>{x.enabled ? c.connEnabled : c.connDisabled}</span>
            </li>
          ))}
        </ul>
      )}
      <h3>{c.identitiesTitle}</h3>
      {settings.identities.length === 0 ? <p className="ads-empty">{c.identitiesEmpty}</p> : (
        <ul className="ads-list" data-testid="ads-identities">
          {settings.identities.map((x) => (
            <li key={x.binding_id}><strong>{c.providers[x.provider] ?? x.provider}</strong> <span className="ads-mono">{x.asset_id}</span></li>
          ))}
        </ul>
      )}
    </section>
  );
}

function PickStep({
  c, store, stateID, busy, uncertain, run, boundary, done, startAgain,
}: {
  c: AdsCopy; store: Store; stateID: string; busy: string; uncertain: string; boundary: { current: string }; done: () => void; startAgain: () => void;
  run: <T>(slot: string, body: string, exec: (key: string) => Promise<WriteResult<T>>, okText: string, after?: (value: T) => void) => Promise<void>;
}) {
  const [state, setState] = useState<{ kind: "loading" } | { kind: "expired" } | { kind: "error" } | { kind: "ready"; value: ConnectState }>({ kind: "loading" });
  const [account, setAccount] = useState("");
  const [dataset, setDataset] = useState("");
  useEffect(() => {
    const active = new AbortController();
    readConnectState(store.id, stateID, active.signal).then(
      (value) => !active.signal.aborted && setState({ kind: "ready", value }),
      (error) => {
        if (active.signal.aborted) return;
        const code = error instanceof AdsReadError ? error.code : "unavailable";
        setState({ kind: code === "state_expired" || code === "not-found" || code === "state_mismatch" ? "expired" : "error" });
      },
    );
    return () => active.abort();
  }, [store.id, stateID]);
  if (state.kind === "loading") return <p className="ads-message" role="status">{c.loading}</p>;
  if (state.kind !== "ready")
    return (
      <div className="ads-bad" role="alert" data-testid="ads-pick-expired">
        {state.kind === "expired" ? c.pickExpired : c.errors.retry_later} <button type="button" className="ads-inline" onClick={startAgain}>{c.startAgain}</button>
      </div>
    );
  const accounts = state.value.picks.filter((p: PickItem) => p.kind === "ad_account");
  const datasets = state.value.picks.filter((p: PickItem) => p.kind === "dataset");
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!account) return;
    const body = JSON.stringify(dataset ? { state_id: stateID, ad_account_id: account, dataset_id: dataset } : { state_id: stateID, ad_account_id: account });
    void run("bind", body, (key) => postBindings(store.id, key, body, boundary.current), "", done);
  };
  return (
    <form className="ads-form" onSubmit={submit} data-testid="ads-pick">
      <fieldset disabled={busy === "bind" || uncertain === "bind"}>
        <legend>{c.pickTitle}</legend>
        {accounts.length === 0 && <p className="ads-empty">{c.pickNoAccounts}</p>}
        <div role="radiogroup" aria-label={c.pickAccounts} className="ads-radios">
          {accounts.map((p) => (
            <label key={p.id} className="ads-radio">
              <input type="radio" name="ad_account" value={p.id} checked={account === p.id} onChange={() => setAccount(p.id)} data-testid={`ads-pick-${p.id}`} />
              <span><strong>{p.name || p.id}</strong> <span className="ads-mono">{p.id}</span>
                <small>{p.currency}{p.timezone ? ` · ${p.timezone}` : ""} · <span className={accountReady(p.account_status) ? "ads-tone-ok" : "ads-tone-warn"}>{c.accountStatus(p.account_status)}</span></small></span>
            </label>
          ))}
        </div>
        {datasets.length > 0 && (
          <label>{c.pickDatasets}
            <select value={dataset} onChange={(e) => setDataset(e.target.value)} data-testid="ads-pick-dataset">
              <option value="">{c.pickNoDataset}</option>
              {datasets.map((p) => <option key={p.id} value={p.id}>{p.name || p.id} ({p.id})</option>)}
            </select>
          </label>
        )}
        <div className="ads-actions">
          <button type="submit" className="primary" disabled={!account} data-testid="ads-pick-submit">{busy === "bind" ? c.sending : c.pickSubmit}</button>
          <button type="button" onClick={startAgain}>{c.startAgain}</button>
        </div>
      </fieldset>
    </form>
  );
}

// ---------- draft form ----------
function DraftFormPanel({
  c, settings, mode, problems, busy, uncertain, onSubmit, onCancel, onRetry,
}: {
  c: AdsCopy; settings: Settings; mode: Exclude<Mode, { kind: "none" }>; problems: AdsCode[]; busy: boolean; uncertain: boolean;
  onSubmit: (form: FormState) => void; onCancel: () => void; onRetry: () => void;
}) {
  const [form, setForm] = useState<FormState>(mode.form);
  const set = <K extends keyof FormState>(key: K, value: FormState[K]) => setForm((f) => ({ ...f, [key]: value }));
  const accounts = settings.connections.filter((x) => x.provider === "meta_ads" && x.enabled);
  const identities = settings.identities.filter((x) => (form.template === "PRODUCT_TRAFFIC" ? x.provider === "facebook" : x.provider === "facebook" || x.provider === "instagram"));
  useEffect(() => {
    // Preselect the only choice; drop a choice the template no longer allows.
    setForm((f) => {
      const ad = accounts.some((a) => a.binding_id === f.ad_binding_id) ? f.ad_binding_id : accounts.length === 1 ? accounts[0].binding_id : "";
      const id = identities.some((a) => a.binding_id === f.identity_binding_id) ? f.identity_binding_id : identities.length === 1 ? identities[0].binding_id : "";
      return ad === f.ad_binding_id && id === f.identity_binding_id ? f : { ...f, ad_binding_id: ad, identity_binding_id: id };
    });
  }, [form.template]); // eslint-disable-line react-hooks/exhaustive-deps
  const title = mode.kind === "edit" ? c.formEdit : mode.kind === "copy" ? c.formCopy : c.formNew;
  const boost = form.template === "BOOST_POST";
  return (
    <form className="ads-form ads-draft-form" data-testid="ads-form" onSubmit={(e) => { e.preventDefault(); onSubmit(form); }} noValidate>
      <fieldset disabled={busy || uncertain}>
        <legend>{title}</legend>
        {mode.kind === "copy" && <p className="ads-note">{c.copyHint}</p>}
        <div role="radiogroup" aria-label={c.fTemplate} className="ads-radios ads-radios-row">
          {(["BOOST_POST", "PRODUCT_TRAFFIC"] as Template[]).map((t) => (
            <label key={t} className="ads-radio">
              <input type="radio" name="template" value={t} checked={form.template === t} data-testid={`ads-template-${t}`} onChange={() => set("template", t)} />
              <span>{c.templates[t]}</span>
            </label>
          ))}
        </div>
        <label>{c.fAdAccount}
          <select value={form.ad_binding_id} onChange={(e) => set("ad_binding_id", e.target.value)} data-testid="ads-f-account">
            <option value="">—</option>
            {accounts.map((a) => <option key={a.binding_id} value={a.binding_id}>{a.asset_id}</option>)}
          </select>
          {accounts.length === 0 && <small className="ads-bad">{c.fNoAccount}</small>}
        </label>
        <label>{boost ? c.fIdentity : c.fIdentityFacebookOnly}
          <select value={form.identity_binding_id} onChange={(e) => set("identity_binding_id", e.target.value)} data-testid="ads-f-identity">
            <option value="">—</option>
            {identities.map((a) => <option key={a.binding_id} value={a.binding_id}>{c.providers[a.provider] ?? a.provider} · {a.asset_id}</option>)}
          </select>
          {identities.length === 0 && <small className="ads-bad">{c.fNoIdentity}</small>}
        </label>
        <label>{c.fSource}
          <input value={form.source_ref} onChange={(e) => set("source_ref", e.target.value)} maxLength={128} autoComplete="off" inputMode="text" data-testid="ads-f-source" />
          <small>{boost ? c.fSourceBoost : c.fSourceProduct}</small>
        </label>
        <label>{c.fBudget(form.currency)}
          <input value={form.budget} onChange={(e) => set("budget", e.target.value)} inputMode="decimal" autoComplete="off" maxLength={14} data-testid="ads-f-budget" />
          <small>{c.fBudgetHint(form.currency)}</small>
        </label>
        <div className="ads-grid2">
          <label>{c.fStarts}
            <input type="datetime-local" value={form.starts_local} onChange={(e) => set("starts_local", e.target.value)} data-testid="ads-f-starts" />
          </label>
          <label>{c.fEnds}
            <input type="datetime-local" value={form.ends_local} onChange={(e) => set("ends_local", e.target.value)} data-testid="ads-f-ends" />
          </label>
        </div>
        <small className="ads-hint">{c.fDatesHint}</small>
        <label>{c.fCountries}
          <input value={form.countries} onChange={(e) => set("countries", e.target.value)} maxLength={80} autoComplete="off" data-testid="ads-f-countries" />
          <small>{c.fCountriesHint}</small>
        </label>
        <div className="ads-grid2">
          <label>{c.fAgeMin}
            <input value={form.age_min} onChange={(e) => set("age_min", e.target.value)} inputMode="numeric" maxLength={2} data-testid="ads-f-agemin" />
          </label>
          <label>{c.fAgeMax}
            <input value={form.age_max} onChange={(e) => set("age_max", e.target.value)} inputMode="numeric" maxLength={2} data-testid="ads-f-agemax" />
          </label>
        </div>
        <small className="ads-hint">{c.fPlacement}</small>
        {problems.length > 0 && (
          <div className="ads-bad" role="alert" data-testid="ads-form-problems">
            <p>{c.fixFields}</p>
            <ul>{problems.map((p) => <li key={p}>{errorText(c, p)}</li>)}</ul>
          </div>
        )}
        <div className="ads-actions">
          <button type="submit" className="primary" data-testid="ads-f-save">{busy ? c.saving : c.save}</button>
          <button type="button" onClick={onCancel} data-testid="ads-f-cancel">{c.cancel}</button>
        </div>
      </fieldset>
      {uncertain && <p className="ads-actions"><button type="button" onClick={onRetry}>{c.retrySame}</button></p>}
    </form>
  );
}

// ---------- draft detail ----------
function DraftDetail({
  c, locale, d, busy, allowanceOff, confirmEnd, setConfirmEnd, onEdit, onCopy, act, formOpen,
}: {
  c: AdsCopy; locale: Locale; d: Draft; busy: string; allowanceOff: boolean; confirmEnd: boolean; formOpen: boolean;
  setConfirmEnd: (v: boolean) => void; onEdit: () => void; onCopy: () => void; act: (d: Draft, a: "approve" | "publish" | "pause" | "end") => void;
}) {
  const link = d.ads_manager_url ? adsManagerHref(d.ads_manager_url) : null;
  const working = busy !== "";
  const remote: [string, string | null][] = [
    [c.remoteCampaign, d.remote.campaign_id], [c.remoteAdset, d.remote.adset_id], [c.remoteCreative, d.remote.creative_id], [c.remoteAd, d.remote.ad_id],
  ];
  return (
    <section className="ads-detail" aria-label={`${c.detailTitle} ${d.id}`} data-testid="ads-detail" data-status={d.status}>
      <div className="ads-detail-head">
        <h3>{c.templates[d.template]} <Badge c={c} status={d.status} /></h3>
        {link && <a href={link} target="_blank" rel="noopener noreferrer" data-testid="ads-manager-link">{c.adsManager}</a>}
      </div>
      {d.status === "UNKNOWN" && <p className="ads-bad" role="status" data-testid="ads-unknown-help">{c.checkAdsManager}</p>}
      <dl className="ads-facts">
        <div><dt>{c.budget}</dt><dd>{formatMinor(locale, d.currency, d.lifetime_budget_minor)}</dd></div>
        <div><dt>{c.starts}</dt><dd>{when(locale, d.starts_at, "—")}</dd></div>
        <div><dt>{c.ends}</dt><dd>{when(locale, d.ends_at, "—")}</dd></div>
        <div><dt>{c.countries}</dt><dd>{d.countries.join(", ")}</dd></div>
        <div><dt>{c.ages}</dt><dd>{d.age_min}–{d.age_max}</dd></div>
        <div><dt>{c.source}</dt><dd className="ads-mono">{d.source_ref}</dd></div>
        <div><dt>{c.revision}</dt><dd>{d.revision}</dd></div>
        <div><dt>{c.attempt}</dt><dd>{d.publish_attempt}</dd></div>
      </dl>
      <div className="ads-actions" data-testid="ads-actions">
        {canEdit(d) && <button type="button" onClick={onEdit} disabled={working || formOpen} data-testid="ads-edit">{c.edit}</button>}
        {canApprove(d) && <button type="button" className="primary" onClick={() => act(d, "approve")} disabled={working || allowanceOff} data-testid="ads-approve">{busy === `approve:${d.id}` ? c.sending : c.approve}</button>}
        {canPublish(d) && <button type="button" className="primary" onClick={() => act(d, "publish")} disabled={working || allowanceOff} data-testid="ads-publish">{busy === `publish:${d.id}` ? c.sending : d.status === "FAILED" ? c.republish : c.publish}</button>}
        {canPause(d) && <button type="button" onClick={() => act(d, "pause")} disabled={working} data-testid="ads-pause">{busy === `pause:${d.id}` ? c.sending : c.pause}</button>}
        {canEnd(d) && !confirmEnd && <button type="button" onClick={() => setConfirmEnd(true)} disabled={working} data-testid="ads-end">{c.end}</button>}
        {canCopy(d) && <button type="button" onClick={onCopy} disabled={working || formOpen} data-testid="ads-copy">{c.copy}</button>}
      </div>
      {allowanceOff && canApprove(d) && <p className="ads-hint">{c.approveOffHint}</p>}
      {canCopy(d) && <p className="ads-hint">{c.copyHint}</p>}
      {confirmEnd && (
        <div className="ads-confirm" role="alertdialog" aria-labelledby="ads-end-h" data-testid="ads-end-confirm">
          <h4 id="ads-end-h">{c.endConfirmTitle}</h4>
          <p>{c.endConfirmBody}</p>
          <div className="ads-actions">
            <button type="button" className="ads-danger" onClick={() => act(d, "end")} disabled={working} data-testid="ads-end-yes">{c.endConfirm}</button>
            <button type="button" onClick={() => setConfirmEnd(false)}>{c.cancel}</button>
          </div>
        </div>
      )}
      <h4>{c.remote}</h4>
      {hasRemote(d) ? (
        <dl className="ads-facts" data-testid="ads-remote">
          {remote.filter(([, v]) => v).map(([label, v]) => <div key={label}><dt>{label}</dt><dd className="ads-mono">{v}</dd></div>)}
        </dl>
      ) : <p className="ads-empty">{c.remoteNone}</p>}
      <h4>{c.opsTitle}</h4>
      {d.ops.length === 0 ? <p className="ads-empty">{c.opsEmpty}</p> : (
        <div className="ads-table-scroll">
          <table className="ads-table ads-ops" data-testid="ads-ops">
            <thead><tr><th>{c.opKind}</th><th>{c.opAttempt}</th><th>{c.opState}</th><th>{c.opUpdated}</th></tr></thead>
            <tbody>
              {d.ops.map((o) => (
                <tr key={`${o.kind}:${o.attempt}:${o.seq}`}>
                  <td data-label={c.opKind}><span className="ads-mono">{o.kind}</span>{o.seq > 1 ? ` #${o.seq}` : ""}</td>
                  <td data-label={c.opAttempt}>{o.attempt}</td>
                  <td data-label={c.opState}>{c.opStates[o.state]}{o.code ? <small className="ads-mono"> {o.code}</small> : null}</td>
                  <td data-label={c.opUpdated}>{when(locale, o.updated_at, "—")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

// ---------- report ----------
function ReportSection({ c, locale, store }: { c: AdsCopy; locale: Locale; store: Store }) {
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [state, setState] = useState<{ kind: "idle" } | { kind: "loading" } | { kind: "invalid" } | { kind: "error"; text: string } | { kind: "ready"; report: Report }>({ kind: "idle" });
  const load = useCallback(async (a: string, b: string, signal: AbortSignal) => {
    if (!validReportWindow(a, b)) {
      setState({ kind: "invalid" });
      return;
    }
    setState({ kind: "loading" });
    try {
      const report = await readReport(store.id, a, b, signal);
      if (!signal.aborted) setState({ kind: "ready", report });
    } catch (error) {
      if (signal.aborted) return;
      const code = error instanceof AdsReadError ? error.code : "unavailable";
      setState({ kind: "error", text: code === "signed-out" ? c.signedOut : code === "forbidden" ? c.forbidden : code === "unavailable" || code === "not-found" ? c.unavailable : errorText(c, code) });
    }
  }, [store.id, c]);
  useEffect(() => {
    // Default window: the last 7 days ending today (browser-local dates, set after mount to keep SSR identical).
    const now = Date.now();
    const a = localDate(now - 6 * dayMs);
    const b = localDate(now);
    setFrom(a);
    setTo(b);
    const active = new AbortController();
    void load(a, b, active.signal);
    return () => active.abort();
  }, [load]);
  const money = (currency: string, minor: number) => formatMinor(locale, currency, minor);
  const num = (n: number) => new Intl.NumberFormat(locale).format(n);
  const fetched = (iso: string | null) => `${c.reportFetched}: ${when(locale, iso, c.reportNotFetched)}`;
  const r = state.kind === "ready" ? state.report : null;
  return (
    <section className="ads-section" aria-labelledby="ads-report-h" data-testid="ads-report">
      <div className="ads-section-head"><h2 id="ads-report-h">{c.reportTitle}</h2></div>
      <form className="ads-controls" onSubmit={(e) => { e.preventDefault(); void load(from, to, new AbortController().signal); }}>
        <label>{c.reportFrom}<input type="date" value={from} onChange={(e) => setFrom(e.target.value)} data-testid="ads-report-from" /></label>
        <label>{c.reportTo}<input type="date" value={to} onChange={(e) => setTo(e.target.value)} data-testid="ads-report-to" /></label>
        <button type="submit" disabled={state.kind === "loading"} data-testid="ads-report-load">{c.reportLoad}</button>
      </form>
      <p className="ads-note">{c.reportRefreshNote}</p>
      {state.kind === "loading" && <p className="ads-message" role="status">{c.reportLoading}</p>}
      {state.kind === "invalid" && <p className="ads-bad" role="alert">{c.reportInvalid} ({maxReportDays})</p>}
      {state.kind === "error" && <p className="ads-bad" role="alert">{state.text}</p>}
      {r && (
        <>
          <p className="ads-note" data-testid="ads-report-separate">{c.reportSeparate}</p>
          <div className="ads-blocks">
            <article className="ads-block" data-testid="ads-block-orders" aria-labelledby="ads-b1">
              <h3 id="ads-b1">{c.blockOrders}</h3>
              <p className="ads-sub">{c.blockOrdersSub}</p>
              {r.orders === null ? <p className="ads-empty" data-testid="ads-orders-na">{c.notAvailable}</p> : (
                <dl className="ads-facts">
                  <div><dt>{c.captured}</dt><dd>{money(r.orders.currency, r.orders.captured_minor)}</dd></div>
                  <div><dt>{c.refunded}</dt><dd>{money(r.orders.currency, r.orders.refunded_minor)}</dd></div>
                  <div><dt>{c.net}</dt><dd><strong>{money(r.orders.currency, r.orders.net_minor)}</strong></dd></div>
                </dl>
              )}
              {r.orders?.note === "card_payments_only" && <p className="ads-hint">{c.cardOnly}</p>}
              <p className="ads-meta">{c.reportWindow}: {r.window.from} – {r.window.to} · {c.reportTimezone}: {r.timezone}{r.orders ? ` · ${fetched(r.orders.fetched_at)}` : ""}</p>
            </article>
            <article className="ads-block" data-testid="ads-block-delivery" aria-labelledby="ads-b2">
              <h3 id="ads-b2">{c.blockDelivery}</h3>
              <p className="ads-sub">{c.blockDeliverySub}</p>
              <dl className="ads-facts">
                <div><dt>{c.spend}</dt><dd>{money(r.meta_delivery.currency, r.meta_delivery.spend_minor)}</dd></div>
                <div><dt>{c.impressions}</dt><dd>{num(r.meta_delivery.impressions)}</dd></div>
                <div><dt>{c.clicks}</dt><dd>{num(r.meta_delivery.clicks)}</dd></div>
                {r.meta_delivery.final_through && <div><dt>{c.finalThrough}</dt><dd>{r.meta_delivery.final_through}</dd></div>}
              </dl>
              <p className="ads-meta">{c.reportWindow}: {r.window.from} – {r.window.to} · {c.reportTimezone}: {r.meta_delivery.account_timezone} · {fetched(r.meta_delivery.fetched_at)}</p>
            </article>
            <article className="ads-block" data-testid="ads-block-reported" aria-labelledby="ads-b3">
              <h3 id="ads-b3">{c.blockReported}</h3>
              <p className="ads-sub">{c.blockReportedSub}</p>
              <dl className="ads-facts">
                <div><dt>{c.purchases}</dt><dd>{num(r.meta_reported.purchases)}</dd></div>
                <div><dt>{c.purchaseValue}</dt><dd>{money(r.meta_reported.currency, r.meta_reported.purchase_value_minor)}</dd></div>
              </dl>
              <p className="ads-meta">{c.reportWindow}: {r.window.from} – {r.window.to} · {c.reportTimezone}: {r.meta_delivery.account_timezone} · {fetched(r.meta_reported.fetched_at)}</p>
            </article>
          </div>
        </>
      )}
    </section>
  );
}

// ---------- CAPI ----------
function CapiSection({
  c, settings, busy, uncertain, save, onRetry,
}: {
  c: AdsCopy; settings: Settings; busy: boolean; uncertain: boolean; onRetry: () => void;
  save: (enabled: boolean, dataset: string, test: string) => Promise<void>;
}) {
  const datasets = useMemo(() => settings.connections.filter((x) => x.provider === "meta_dataset"), [settings]);
  const [enabled, setEnabled] = useState(settings.capi.enabled);
  const [dataset, setDataset] = useState(settings.capi.dataset_binding_id ?? "");
  const [test, setTest] = useState(settings.capi.test_event_code ?? "");
  const [invalid, setInvalid] = useState(false);
  useEffect(() => {
    setEnabled(settings.capi.enabled);
    setDataset(settings.capi.dataset_binding_id ?? "");
    setTest(settings.capi.test_event_code ?? "");
    // Primitive deps: a background reload returns a new settings object and must not wipe unsaved edits.
  }, [settings.capi.enabled, settings.capi.dataset_binding_id, settings.capi.test_event_code]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <section className="ads-section" aria-labelledby="ads-capi-h" data-testid="ads-capi">
      <div className="ads-section-head"><h2 id="ads-capi-h">{c.capiTitle}</h2></div>
      <p className="ads-note">{c.capiIntro}</p>
      <p className="ads-note">{c.capiConsent}</p>
      <form className="ads-form" onSubmit={(e) => { e.preventDefault(); if (!validCapi(enabled, dataset, test)) return setInvalid(true); setInvalid(false); void save(enabled, dataset, test); }}>
        <fieldset disabled={busy || uncertain}>
          <label className="ads-check"><input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} data-testid="ads-capi-enabled" />{c.capiEnable}</label>
          <label>{c.capiDataset}
            <select value={dataset} onChange={(e) => setDataset(e.target.value)} data-testid="ads-capi-dataset">
              <option value="">{c.capiNoDataset}</option>
              {datasets.map((x) => <option key={x.binding_id} value={x.binding_id}>{x.asset_id}</option>)}
            </select>
            {datasets.length === 0 && <small>{c.capiNoneConnected}</small>}
          </label>
          <label>{c.capiTest}
            <input value={test} onChange={(e) => setTest(e.target.value)} maxLength={64} autoComplete="off" data-testid="ads-capi-test" />
            <small>{c.capiTestHint}</small>
          </label>
          {invalid && <p className="ads-bad" role="alert">{c.capiInvalid}</p>}
          <div className="ads-actions"><button type="submit" className="primary" data-testid="ads-capi-save">{busy ? c.sending : c.capiSave}</button></div>
        </fieldset>
        {uncertain && <p className="ads-actions"><button type="button" onClick={onRetry}>{c.retrySame}</button></p>}
      </form>
    </section>
  );
}
