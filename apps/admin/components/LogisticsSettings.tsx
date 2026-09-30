"use client";

// Settings -> 物流 cards (mounted by SettingsWizard.tsx, merchant-arranged delivery branch): "綠界物流" (connect / rotate,
// enable, read-only status URL) and "超商取貨" (chains, pay-at-pickup toggle and limits).
// BFF routes (lib/logistics-client.ts) -> Go internal/httpapi/cvs.go (taiwan-cvs-logistics-v1 §8, §16.5):
//   GET|PUT /api/stores/{store}/logistics/ecpay, POST .../logistics/ecpay/enabled, GET|PUT .../logistics/cvs-settings
// A GET 403 hides the card (this member has no integration:read); a PUT 403 shows a no-permission notice. ECPay
// keys are typed here, sent once and never displayed again: the inputs are cleared after every send and only a
// SHA-256 digest of the last body is kept (to reuse the Idempotency-Key for a byte-identical retry after an unknown
// outcome), never the keys. Nothing is optimistic: every write re-GETs the card.
import { useEffect, useRef, useState, type FormEvent } from "react";
import type { Locale } from "@live-commerce/i18n";
import { sessionBoundary } from "@/lib/settings-client";
import { OrderReadError } from "@/lib/orders-client";
import {
  postEcpayEnabled,
  putCvsSettings,
  putEcpay,
  readCvsSettings,
  readEcpay,
  type WriteResult,
} from "@/lib/logistics-client";
import {
  CHAINS,
  ECPAY_ENVIRONMENTS,
  ECPAY_MODES,
  cvsSettingsBody,
  ecpayConnectBody,
  ecpayQualified,
  type Chain,
  type CvsSettings,
  type EcpayConnection,
  type EcpayEnvironment,
  type EcpayMode,
} from "@/lib/logistics-model";
import { logisticsCopy, type LogisticsCopy } from "@/lib/logistics-copy";
import "./settings.css";

type Load = "loading" | "ready" | "hidden" | "error";
const digest = async (text: string) =>
  [...new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)))]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
const errorLine = (lc: LogisticsCopy, code: string) =>
  code === "forbidden" ? lc.noPermission : (lc.errors[code] ?? lc.unavailable);

export function LogisticsSettings({ store, locale }: { store: string; locale: Locale }) {
  const lc = logisticsCopy[locale];
  const [boundary, setBoundary] = useState("");
  useEffect(() => {
    let live = true;
    sessionBoundary().then(
      (value) => live && setBoundary(value),
      () => live && setBoundary(""),
    );
    return () => {
      live = false;
    };
  }, [store]);
  return (
    <>
      <EcpayCard store={store} lc={lc} boundary={boundary} />
      <CvsCard store={store} lc={lc} boundary={boundary} />
    </>
  );
}

function EcpayCard({ store, lc, boundary }: { store: string; lc: LogisticsCopy; boundary: string }) {
  const [load, setLoad] = useState<Load>("loading");
  const [conn, setConn] = useState<EcpayConnection | null>(null);
  const [tick, setTick] = useState(0);
  const [editing, setEditing] = useState(false);
  const [environment, setEnvironment] = useState<EcpayEnvironment>("SANDBOX");
  const [mode, setMode] = useState<EcpayMode>("C2C");
  const [merchantID, setMerchantID] = useState("");
  const [hashKey, setHashKey] = useState("");
  const [hashIV, setHashIV] = useState("");
  const [senderName, setSenderName] = useState("");
  const [senderPhone, setSenderPhone] = useState("");
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState("");
  const [notice, setNotice] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const [copied, setCopied] = useState<"" | "ok" | "failed">("");
  const pending = useRef<{ key: string; digest: string } | null>(null);

  useEffect(() => {
    const active = new AbortController();
    readEcpay(store, active.signal).then(
      (value) => {
        setConn(value);
        setLoad("ready");
      },
      (error) => {
        if (active.signal.aborted) return;
        setConn(null);
        setLoad(error instanceof OrderReadError && error.code === "forbidden" ? "hidden" : "error");
      },
    );
    return () => active.abort();
  }, [store, tick]);

  if (load === "hidden") return null;
  const showForm = load === "ready" && (conn === null || editing);
  const wipeSecrets = () => {
    setHashKey("");
    setHashIV("");
  };

  async function connect(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    const body = ecpayConnectBody({
      expectedVersion: conn?.version ?? 0, environment, mode, merchantID, hashKey, hashIV, senderName, senderPhone,
    });
    if (!body) {
      setProblem(lc.ecpayInvalid);
      return;
    }
    const text = JSON.stringify(body);
    const sum = await digest(text);
    if (pending.current?.digest !== sum) pending.current = { key: `ecpay-${crypto.randomUUID()}`, digest: sum };
    setBusy(true);
    setProblem("");
    const result: WriteResult = await putEcpay(store, pending.current.key, text, boundary);
    setBusy(false);
    wipeSecrets(); // the keys are never kept: a retry needs them typed again
    if (result.ok) {
      pending.current = null;
      setUncertain(false);
      setEditing(false);
      setNotice(lc.ecpaySaved);
      setTick((value) => value + 1);
      return;
    }
    if (result.uncertain) {
      setUncertain(true);
      setProblem(lc.uncertain);
      return;
    }
    pending.current = null;
    setUncertain(false);
    setProblem(errorLine(lc, result.code));
  }
  async function toggle(enabled: boolean) {
    if (!conn || busy) return;
    const body = JSON.stringify({ expected_version: conn.version, enabled });
    const sum = await digest(body);
    if (pending.current?.digest !== sum) pending.current = { key: `ecpay-${crypto.randomUUID()}`, digest: sum };
    setBusy(true);
    setProblem("");
    const result = await postEcpayEnabled(store, pending.current.key, body, boundary);
    setBusy(false);
    if (result.ok) {
      pending.current = null;
      setNotice(lc.ecpayEnabledDone);
      setTick((value) => value + 1);
      return;
    }
    if (result.uncertain) {
      setProblem(lc.uncertain);
      return;
    }
    pending.current = null;
    setProblem(errorLine(lc, result.code));
    if (result.code === "version_changed" || result.code === "version_conflict") setTick((value) => value + 1);
  }
  async function copyURL() {
    try {
      await navigator.clipboard.writeText(conn!.status_url);
      setCopied("ok");
    } catch {
      setCopied("failed");
    }
  }
  function beginEdit() {
    pending.current = null;
    setEnvironment(conn?.environment ?? "SANDBOX");
    setMode(conn?.mode ?? "C2C");
    setMerchantID(conn?.merchant_id ?? "");
    wipeSecrets();
    setProblem("");
    setNotice("");
    setEditing(true);
  }

  return (
    <section className="settings-fields" data-testid="ecpay-card" aria-labelledby="ecpay-title">
      <h2 id="ecpay-title" className="settings-section-title settings-subtitle">{lc.ecpayTitle}</h2>
      <p className="settings-note">{lc.ecpayIntro}</p>
      {load === "loading" && <p role="status">{lc.loading}</p>}
      {load === "error" && (
        <div role="status">
          <p>{lc.unavailable}</p>
          <button type="button" onClick={() => setTick((value) => value + 1)}>{lc.retry}</button>
        </div>
      )}
      {load === "ready" && conn === null && !showForm && <p className="settings-note">{lc.ecpayNone}</p>}
      {load === "ready" && conn && (
        <dl className="settings-status-list" data-testid="ecpay-status">
          <div><dt>{lc.ecpayEnv}</dt><dd data-testid="ecpay-env">{lc.ecpayEnvBadge[conn.environment]}</dd></div>
          <div><dt>{lc.ecpayMode}</dt><dd>{lc.ecpayModes[conn.mode]}</dd></div>
          <div><dt>{lc.ecpayMerchantID}</dt><dd>{conn.merchant_id}</dd></div>
          <div>
            <dt>{lc.ecpayQualified}</dt>
            <dd>{conn.qualified_at ? lc.ecpayQualified : lc.ecpayNotQualified}</dd>
          </div>
          <div><dt>{lc.chains.cvs_okmart}</dt><dd>{conn.ok_verified ? lc.ecpayOkVerified : lc.ecpayOkPending}</dd></div>
          <div>
            <dt>{lc.ecpayEnabled}</dt>
            <dd data-testid="ecpay-enabled">{conn.enabled ? lc.ecpayEnabled : lc.ecpayDisabled}</dd>
          </div>
          <div>
            <dt>{lc.ecpayStatusURL}</dt>
            <dd>
              <code data-testid="ecpay-status-url">{conn.status_url}</code>{" "}
              <button type="button" onClick={() => void copyURL()}>{lc.copy}</button>
              {copied && <span role="status"> {copied === "ok" ? lc.copied : lc.copyFailed}</span>}
              <br />
              <small>{lc.ecpayStatusHint}</small>
            </dd>
          </div>
        </dl>
      )}
      {load === "ready" && conn && !editing && (
        <div className="settings-actions">
          <button
            type="button"
            data-testid="ecpay-toggle"
            disabled={busy || (!conn.enabled && conn.qualified_at === null)}
            onClick={() => void toggle(!conn.enabled)}
          >
            {conn.enabled ? lc.ecpayDisable : lc.ecpayEnable}
          </button>
          <button type="button" data-testid="ecpay-rotate" disabled={busy} onClick={beginEdit}>{lc.ecpayRotate}</button>
        </div>
      )}
      {showForm && (
        <form data-testid="ecpay-form" onSubmit={(event) => void connect(event)} className="settings-fields" autoComplete="off">
          {conn && <p className="settings-warning" role="note">{lc.ecpayRotateWarn}</p>}
          <div className="settings-field-grid">
            <label>
              {lc.ecpayEnv}
              <select value={environment} disabled={busy} onChange={(event) => setEnvironment(event.target.value as EcpayEnvironment)}>
                {ECPAY_ENVIRONMENTS.map((value) => <option key={value} value={value}>{lc.ecpayEnvBadge[value]}</option>)}
              </select>
            </label>
            <label>
              {lc.ecpayMode}
              <select value={mode} disabled={busy} onChange={(event) => setMode(event.target.value as EcpayMode)}>
                {ECPAY_MODES.map((value) => <option key={value} value={value}>{lc.ecpayModes[value]}</option>)}
              </select>
            </label>
            <label>
              {lc.ecpayMerchantID}
              <input value={merchantID} maxLength={16} required disabled={busy} autoComplete="off" spellCheck={false}
                onChange={(event) => setMerchantID(event.target.value)} />
            </label>
            <label>
              {lc.ecpaySenderName}
              <input value={senderName} maxLength={10} required disabled={busy} autoComplete="off"
                onChange={(event) => setSenderName(event.target.value)} />
            </label>
            <label>
              {lc.ecpayHashKey}
              <input type="password" value={hashKey} maxLength={64} required disabled={busy} autoComplete="new-password"
                spellCheck={false} onChange={(event) => setHashKey(event.target.value)} />
            </label>
            <label>
              {lc.ecpayHashIV}
              <input type="password" value={hashIV} maxLength={64} required disabled={busy} autoComplete="new-password"
                spellCheck={false} onChange={(event) => setHashIV(event.target.value)} />
            </label>
            <label>
              {lc.ecpaySenderPhone}
              <input type="tel" value={senderPhone} maxLength={10} required disabled={busy} autoComplete="off"
                onChange={(event) => setSenderPhone(event.target.value)} />
            </label>
          </div>
          <p className="settings-note">{lc.ecpaySecretsNote}</p>
          <div className="settings-actions">
            {conn && <button type="button" disabled={busy} onClick={() => { pending.current = null; wipeSecrets(); setEditing(false); setProblem(""); }}>{lc.cancel}</button>}
            <button className="primary" type="submit" data-testid="ecpay-submit" disabled={busy}>
              {busy ? lc.saving : uncertain ? lc.retrySame : conn ? lc.ecpayRotate : lc.ecpayConnect}
            </button>
          </div>
        </form>
      )}
      {problem && <p className="settings-warning" role="alert" data-testid="ecpay-problem">{problem}</p>}
      {notice && <p className="message pending" role="status" data-testid="ecpay-notice">{notice}</p>}
    </section>
  );
}

function CvsCard({ store, lc, boundary }: { store: string; lc: LogisticsCopy; boundary: string }) {
  const [load, setLoad] = useState<Load>("loading");
  const [saved, setSaved] = useState<CvsSettings | null>(null);
  const [tick, setTick] = useState(0);
  const [chains, setChains] = useState<Chain[]>([]);
  const [payAtPickup, setPayAtPickup] = useState(false);
  const [maxTWD, setMaxTWD] = useState("");
  const [maxOpen, setMaxOpen] = useState("20");
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState("");
  const [notice, setNotice] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const pending = useRef<{ key: string; body: string } | null>(null);

  useEffect(() => {
    const active = new AbortController();
    readCvsSettings(store, active.signal).then(
      (value) => {
        setSaved(value);
        setChains(value.enabled_chains);
        setPayAtPickup(value.pay_at_pickup_enabled);
        setMaxTWD(value.pay_at_pickup_max_twd === null ? "" : String(value.pay_at_pickup_max_twd));
        setMaxOpen(String(value.pay_at_pickup_max_open));
        setLoad("ready");
      },
      (error) => {
        if (active.signal.aborted) return;
        setSaved(null);
        setLoad(error instanceof OrderReadError && error.code === "forbidden" ? "hidden" : "error");
      },
    );
    return () => active.abort();
  }, [store, tick]);

  if (load === "hidden") return null;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || !saved) return;
    const body = cvsSettingsBody({ expectedVersion: saved.version, chains, payAtPickup, maxTWD, maxOpen });
    if (!body) {
      setProblem(lc.cvsInvalid);
      return;
    }
    const text = JSON.stringify(body);
    if (pending.current?.body !== text) pending.current = { key: `cvs-set-${crypto.randomUUID()}`, body: text };
    setBusy(true);
    setProblem("");
    const result = await putCvsSettings(store, pending.current.key, text, boundary);
    setBusy(false);
    if (result.ok) {
      pending.current = null;
      setUncertain(false);
      setNotice(lc.cvsSaved);
      setTick((value) => value + 1);
      return;
    }
    if (result.uncertain) {
      setUncertain(true);
      setProblem(lc.uncertain);
      return;
    }
    pending.current = null;
    setUncertain(false);
    setProblem(errorLine(lc, result.code));
    if (result.code === "version_changed" || result.code === "version_conflict") setTick((value) => value + 1);
  }

  return (
    <section className="settings-fields" data-testid="cvs-settings-card" aria-labelledby="cvs-settings-title">
      <h2 id="cvs-settings-title" className="settings-section-title settings-subtitle">{lc.cvsTitle}</h2>
      <p className="settings-note">{lc.cvsIntro}</p>
      {load === "loading" && <p role="status">{lc.loading}</p>}
      {load === "error" && (
        <div role="status">
          <p>{lc.unavailable}</p>
          <button type="button" onClick={() => setTick((value) => value + 1)}>{lc.retry}</button>
        </div>
      )}
      {load === "ready" && saved && (
        <form onSubmit={(event) => void submit(event)} className="settings-fields">
          <fieldset className="settings-radio" disabled={busy}>
            <legend>{lc.cvsChains}</legend>
            {CHAINS.map((chain) => (
              <label key={chain} className="settings-check">
                <input
                  type="checkbox"
                  data-testid={`cvs-chain-${chain}`}
                  checked={chains.includes(chain)}
                  onChange={(event) =>
                    setChains(event.target.checked ? [...chains, chain] : chains.filter((value) => value !== chain))
                  }
                />
                {lc.chains[chain]}
              </label>
            ))}
          </fieldset>
          <label className="settings-check">
            <input
              type="checkbox"
              data-testid="cvs-pay-at-pickup"
              checked={payAtPickup}
              disabled={busy}
              onChange={(event) => setPayAtPickup(event.target.checked)}
            />
            {lc.cvsPayAtPickup}
          </label>
          <div className="settings-field-grid">
            <label>
              {lc.cvsMaxTWD}
              <input data-testid="cvs-max-twd" inputMode="numeric" value={maxTWD} disabled={busy} maxLength={5}
                onChange={(event) => setMaxTWD(event.target.value)} />
            </label>
            <label>
              {lc.cvsMaxOpen}
              <input data-testid="cvs-max-open" inputMode="numeric" value={maxOpen} disabled={busy} maxLength={3}
                onChange={(event) => setMaxOpen(event.target.value)} />
            </label>
          </div>
          <p className="settings-note">{lc.cvsPayNote}</p>
          <div className="settings-actions">
            <button className="primary" type="submit" data-testid="cvs-settings-save" disabled={busy}>
              {busy ? lc.saving : uncertain ? lc.retrySame : lc.save}
            </button>
          </div>
        </form>
      )}
      {problem && <p className="settings-warning" role="alert" data-testid="cvs-settings-problem">{problem}</p>}
      {notice && <p className="message pending" role="status" data-testid="cvs-settings-notice">{notice}</p>}
    </section>
  );
}
