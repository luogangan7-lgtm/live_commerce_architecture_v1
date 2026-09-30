"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import { useRouter } from "next/navigation";
import type { Locale } from "@live-commerce/i18n";
import type { Store } from "@/lib/model";
import { csrfCookie, sessionBoundary } from "@/lib/settings-client";
import {
  createStudioDraft, editStudioDraft, readStudioDetail, readStudioPage,
  startStudioRehearsal, stopStudioRehearsal, StudioError,
  type DraftInput, type StudioErrorCode,
} from "@/lib/studio-client";
import type { AspectRatio, Draft, StudioDetail, StudioPage } from "@/lib/studio-model";
import { studioCopy } from "@/lib/studio-copy";
import { WorkspaceFrame } from "./WorkspaceFrame";

type Status = "initial" | "loading" | "ready" | "hidden" | StudioErrorCode;
type PageView = { scope: string; status: Status; data: StudioPage | null };
type DetailView = { key: string; status: Status; data: StudioDetail | null };
type Form = { id: string; title: string; scheduled: string; aspect: AspectRatio };
type Action = "create" | "edit" | "start" | "stop";
type EditInput = DraftInput & { expected_version: number };
type Pending = { signature: string; key: string; action: Action; body: DraftInput | EditInput | { authorization_id: string; expected_session_version: number } | { attempt_id: string }; sessionID: string; storeID: string; boundary: string };
type Concealed = { scope: string; scene: string; route: string; selectedID: string; boundary: string; cookie: string; form: Form; newMode: boolean; dirty: boolean; actionError: StudioErrorCode | null; formError: string };
type Recovery = Concealed & { request: Pending | null };

const blank: Form = { id: "new", title: "", scheduled: "", aspect: "9:16" };
// ponytail: module memory survives SPA route unmounts; a document exit needs beforeunload consent.
let historyRecovery: Recovery | null = null;
const recoveryKey = (scope: string, scene: string) => JSON.stringify([scope, scene]);
const sameRecovery = (saved: Recovery | null, scope: string, scene: string) =>
  !!saved && recoveryKey(saved.scope, saved.scene) === recoveryKey(scope, scene);
let recoveryWatchInstalled = false;
function watchRecoverySession() {
  if (recoveryWatchInstalled) return;
  recoveryWatchInstalled = true;
  const purge = () => { historyRecovery = null; };
  const check = () => { if (historyRecovery && csrfCookie() !== historyRecovery.cookie) purge(); };
  window.addEventListener("commerce-session-logout", purge);
  window.addEventListener("storage", (event) => {
    if (event.key === "commerce-session-logout") purge();
  });
  window.addEventListener("focus", check);
  window.addEventListener("beforeunload", (event) => {
    if (!historyRecovery) return;
    event.preventDefault();
    event.returnValue = "";
  });
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") check();
  });
  try {
    const channel = new BroadcastChannel("commerce-session");
    channel.addEventListener("message", (event) => {
      if (event.data?.type === "logout") purge();
    });
  } catch { /* focus and storage still fence recovery */ }
}

function studioURL(locale: Locale, store: string, cursor = "", scene = "") {
  const query = new URLSearchParams();
  if (store) query.set("store", store);
  if (cursor) query.set("cursor", cursor);
  if (scene) query.set("scene", scene);
  return `/${locale}/studio${query.size ? `?${query}` : ""}`;
}
function formOf(draft: Draft): Form {
  return { id: draft.session_id, title: draft.title,
    scheduled: utcMinute(draft.scheduled_at), aspect: draft.aspect_ratio };
}
function utcMinute(value: string | null) {
  return value ? new Date(value).toISOString().slice(0, 16) : "";
}
function time(locale: Locale, value: string) {
  return new Intl.DateTimeFormat(locale, { timeZone: "UTC", year: "numeric", month: "2-digit",
    day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false }).format(new Date(value));
}
function draftInput(form: Form): DraftInput | null {
  if (form.title.trim() !== form.title || Array.from(form.title).length < 1 ||
    Array.from(form.title).length > 200 || /[\p{Cc}]/u.test(form.title)) return null;
  if (form.scheduled) {
    if (!/^\d{4}-\d\d-\d\dT\d\d:\d\d$/.test(form.scheduled)) return null;
    const date = new Date(`${form.scheduled}:00Z`);
    if (!Number.isFinite(date.getTime()) || date.toISOString().slice(0, 16) !== form.scheduled ||
      date.getUTCFullYear() < 2000 || date.getUTCFullYear() > 2199) return null;
    return { title: form.title, scheduled_at: date.toISOString(), aspect_ratio: form.aspect };
  }
  return { title: form.title, scheduled_at: null, aspect_ratio: form.aspect };
}
function errorCode(error: unknown): StudioErrorCode {
  return error instanceof StudioError ? error.code : "unavailable";
}

export function Studio({ locale, stores, store, scene, cursor, initialError }: {
  locale: Locale;
  stores: Store[];
  store: Store | null;
  scene: string;
  cursor: string;
  initialError: StudioErrorCode | null;
}) {
  const c = studioCopy[locale];
  const router = useRouter();
  const storeID = store?.id ?? "";
  const scope = `${locale}|${storeID}|${cursor}`;
  const route = studioURL(locale, storeID, cursor, scene);
  const initialRecovery = !initialError && sameRecovery(historyRecovery, scope, scene) ? historyRecovery : null;
  const [page, setPage] = useState<PageView>({ scope: "", status: "initial", data: null });
  const [detail, setDetail] = useState<DetailView>({ key: "", status: "initial", data: null });
  const [form, setForm] = useState<Form>(blank);
  const [newMode, setNewMode] = useState(false);
  const [actionError, setActionError] = useState<StudioErrorCode | null>(null);
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [pageReload, setPageReload] = useState(0);
  const [detailReload, setDetailReload] = useState(0);
  const [pinnedScene, setPinnedScene] = useState({ scope: "", id: "" });
  // Last media_enabled the API reported (GET detail). Hidden until the API says media is on (R1 ruling G2).
  const [mediaOn, setMediaOn] = useState(false);
  const pageEpoch = useRef(0);
  const detailEpoch = useRef(0);
  const actionEpoch = useRef(0);
  const revealEpoch = useRef(0);
  const pageController = useRef<AbortController | null>(null);
  const detailController = useRef<AbortController | null>(null);
  const hidden = useRef(!!initialRecovery);
  const blocked = useRef(false);
  const cookie = useRef("");
  const boundary = useRef(initialRecovery?.boundary ?? "");
  const dirty = useRef(initialRecovery?.dirty ?? false);
  const pending = useRef<Pending | null>(initialRecovery?.request ?? null);
  const concealed = useRef<Concealed | null>(initialRecovery ?? null);
  const explicitDeparture = useRef(false);
  const previous = useRef<string[]>([]);
  const currentPage = page.scope === scope && (!cookie.current || csrfCookie() === cookie.current)
    ? page : { scope, status: "initial" as Status, data: null };
  const selectedID = scene || (concealed.current?.scope === scope ? concealed.current.selectedID : "") ||
    (pinnedScene.scope === scope ? pinnedScene.id : "") || currentPage.data?.items[0]?.session_id || "";
  const detailKey = `${storeID}|${selectedID}`;
  const currentDetail = detail.key === detailKey && (!cookie.current || csrfCookie() === cookie.current)
    ? detail : { key: detailKey, status: "initial" as Status, data: null };
  const shown = newMode ? null : currentDetail.data;
  useEffect(() => { if (currentDetail.data) setMediaOn(currentDetail.data.media_enabled); }, [currentDetail.data]);
  const formDirty = newMode ? (form.title !== "" || form.scheduled !== "" || form.aspect !== blank.aspect) :
    !!shown && (form.id !== shown.draft.session_id || form.title !== shown.draft.title ||
      form.scheduled !== utcMinute(shown.draft.scheduled_at) || form.aspect !== shown.draft.aspect_ratio);
  if (newMode || shown) dirty.current = formDirty;
  const volatile = useRef({ scope, scene, route, selectedID, form, newMode, actionError, formError });
  volatile.current = { scope, scene, route, selectedID, form, newMode, actionError, formError };
  const recoveryElsewhere = !!historyRecovery && !sameRecovery(historyRecovery, scope, scene);
  const recoveryGuard = hidden.current || sameRecovery(historyRecovery, scope, scene);
  const recoveryRoute = recoveryElsewhere ? historyRecovery?.route : null;
  useEffect(watchRecoverySession, []);
  useEffect(() => { explicitDeparture.current = false; }, [scope, scene]);

  const clear = useCallback((status: Status, block = false) => {
    revealEpoch.current++;
    pageEpoch.current++;
    detailEpoch.current++;
    actionEpoch.current++;
    pageController.current?.abort();
    detailController.current?.abort();
    cookie.current = "";
    boundary.current = "";
    pending.current = null;
    concealed.current = null;
    dirty.current = false;
    if (block) { blocked.current = true; historyRecovery = null; }
    flushSync(() => {
      setPage({ scope, status, data: null });
      setDetail({ key: detailKey, status, data: null });
      setForm(blank);
      setNewMode(false);
      setActionError(null);
      setFormError("");
      setPinnedScene({ scope: "", id: "" });
      setBusy(false);
    });
  }, [scope, detailKey]);

  function retainHistory() {
    if (explicitDeparture.current) return;
    const state = volatile.current;
    const saved: Concealed = concealed.current ?? {
      scope: state.scope, scene: state.scene, route: state.route, selectedID: state.selectedID,
      boundary: boundary.current, cookie: cookie.current || csrfCookie(),
      form: state.form, newMode: state.newMode, dirty: dirty.current,
      actionError: pending.current ? "uncertain" : state.actionError, formError: state.formError,
    };
    if ((saved.dirty || saved.newMode || pending.current) && saved.cookie &&
      (!historyRecovery || sameRecovery(historyRecovery, saved.scope, saved.scene)))
      historyRecovery = { ...saved, request: pending.current };
  }

  const reveal = useCallback(() => {
    if (!hidden.current) return;
    const saved = concealed.current;
    const epoch = ++revealEpoch.current;
    if (!saved || saved.scope !== volatile.current.scope || !saved.cookie) {
      hidden.current = false;
      clear("signed-out", true);
      return;
    }
    void sessionBoundary(saved.cookie).then((current) => {
      if (epoch !== revealEpoch.current || !hidden.current) return;
      if ((saved.boundary && current !== saved.boundary) || csrfCookie() !== saved.cookie) {
        hidden.current = false;
        clear("signed-out", true);
        return;
      }
      hidden.current = false;
      boundary.current = current;
      setPageReload((v) => v + 1);
      setDetailReload((v) => v + 1);
    }).catch(() => {
      if (epoch !== revealEpoch.current || !hidden.current) return;
      hidden.current = false;
      clear("signed-out", true);
    });
  }, [clear]);

  useEffect(() => {
    if (initialError === "signed-out") { historyRecovery = null; return; }
    const currentCookie = csrfCookie();
    if (historyRecovery && historyRecovery.cookie !== currentCookie) historyRecovery = null;
    const saved = sameRecovery(historyRecovery, scope, scene) ? historyRecovery : null;
    if (!saved || concealed.current === saved || initialError || blocked.current) return;
    const url = new URL(window.location.href);
    if (url.pathname !== `/${locale}/studio` ||
      (url.searchParams.get("store") && url.searchParams.get("store") !== storeID) ||
      (url.searchParams.get("cursor") ?? "") !== cursor ||
      (url.searchParams.get("scene") ?? "") !== scene) return;
    hidden.current = true;
    pageEpoch.current++;
    detailEpoch.current++;
    actionEpoch.current++;
    pageController.current?.abort();
    detailController.current?.abort();
    concealed.current = saved;
    pending.current = saved.request;
    boundary.current = saved.boundary;
    dirty.current = saved.dirty;
    flushSync(() => {
      setPage({ scope, status: "hidden", data: null });
      setDetail({ key: `${storeID}|${saved.selectedID}`, status: "hidden", data: null });
      setForm(blank);
      setNewMode(false);
      setActionError(null);
      setFormError("");
      setBusy(false);
    });
    if (document.visibilityState === "visible") reveal();
  }, [scope, scene, locale, storeID, cursor, initialError, pageReload, reveal]);

  const loadPage = useCallback(async () => {
    if (hidden.current || blocked.current) return;
    if (initialError || !storeID) {
      setPage({ scope, status: initialError ?? "not-found", data: null });
      return;
    }
    const epoch = ++pageEpoch.current;
    pageController.current?.abort();
    const controller = new AbortController();
    pageController.current = controller;
    setPage({ scope, status: "loading", data: null });
    try {
      const before = await sessionBoundary().catch(() => { throw new StudioError("signed-out"); });
      const data = await readStudioPage(storeID, cursor, controller.signal);
      if (pageEpoch.current !== epoch || controller.signal.aborted || hidden.current) return;
      if ((await sessionBoundary().catch(() => "")) !== before) throw new StudioError("signed-out");
      cookie.current = csrfCookie();
      boundary.current = before;
      setPage({ scope, status: "ready", data });
      const saved = concealed.current;
      if (saved?.scope === scope && saved.boundary === before && saved.newMode) {
        dirty.current = saved.dirty;
        setForm(saved.form);
        setNewMode(true);
        setActionError(saved.actionError);
        setFormError(saved.formError);
        concealed.current = null;
        if (sameRecovery(historyRecovery, saved.scope, saved.scene)) historyRecovery = null;
      }
    } catch (error) {
      if (pageEpoch.current !== epoch || controller.signal.aborted) return;
      const code = errorCode(error);
      if (code === "signed-out") { clear("signed-out", true); return; }
      setPage({ scope, status: code, data: null });
    }
  }, [scope, storeID, cursor, initialError, pageReload, clear]);

  useEffect(() => {
    void loadPage();
    return () => { pageEpoch.current++; pageController.current?.abort(); };
  }, [loadPage]);

  const loadDetail = useCallback(async () => {
    if (hidden.current || blocked.current || newMode || concealed.current?.newMode) return;
    if (!selectedID || !storeID) {
      setDetail({ key: detailKey, status: "initial", data: null });
      return;
    }
    const epoch = ++detailEpoch.current;
    detailController.current?.abort();
    const controller = new AbortController();
    detailController.current = controller;
    setDetail((current) => current.key === detailKey
      ? { ...current, status: "loading" }
      : { key: detailKey, status: "loading", data: null });
    try {
      const before = await sessionBoundary().catch(() => { throw new StudioError("signed-out"); });
      const data = await readStudioDetail(storeID, selectedID, controller.signal);
      if (detailEpoch.current !== epoch || controller.signal.aborted || hidden.current) return;
      if ((await sessionBoundary().catch(() => "")) !== before) throw new StudioError("signed-out");
      cookie.current = csrfCookie();
      boundary.current = before;
      const unresolved = pending.current;
      const resolved = unresolved?.storeID === storeID && unresolved.sessionID === selectedID &&
        ((unresolved.action === "start" && data.attempt) ||
          (unresolved.action === "stop" && data.attempt &&
            (data.attempt.stop_requested || data.attempt.resource_state === "TERMINAL")));
      if (resolved) {
        pending.current = null;
        setActionError(null);
      }
      const saved = concealed.current;
      if (saved?.scope === scope && saved.boundary === before && !saved.newMode && saved.selectedID === selectedID) {
        dirty.current = saved.dirty;
        setForm(saved.form);
        setPinnedScene({ scope, id: selectedID });
        setActionError(resolved ? null : saved.actionError);
        setFormError(saved.formError);
        concealed.current = null;
        if (sameRecovery(historyRecovery, saved.scope, saved.scene)) historyRecovery = null;
      } else if (!dirty.current || form.id !== selectedID) setForm(formOf(data.draft));
      setDetail({ key: detailKey, status: "ready", data });
    } catch (error) {
      if (detailEpoch.current !== epoch || controller.signal.aborted) return;
      const code = errorCode(error);
      if (code === "signed-out") { clear("signed-out", true); return; }
      setDetail({ key: detailKey, status: code, data: null });
    }
  }, [detailKey, selectedID, storeID, newMode, detailReload, scope, clear]);

  useEffect(() => {
    void loadDetail();
    return () => { detailEpoch.current++; detailController.current?.abort(); };
  }, [loadDetail]);

  useEffect(() => {
    const attempt = currentDetail.data?.attempt;
    if (!attempt || attempt.resource_state === "TERMINAL" || attempt.escalated || newMode) return;
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") setDetailReload((value) => value + 1);
    }, 5000);
    return () => window.clearInterval(timer);
  }, [currentDetail.data?.attempt?.attempt_id, currentDetail.data?.attempt?.resource_state,
    currentDetail.data?.attempt?.escalated, newMode]);

  useEffect(() => {
    const deadline = currentDetail.data?.prepared?.start_before;
    if (!deadline) return;
    const remaining = Date.parse(deadline) - Date.now();
    setNow(Date.now());
    if (remaining <= 0) return;
    const timer = window.setTimeout(() => setNow(Date.now()), Math.min(remaining + 20, 2_147_483_647));
    return () => window.clearTimeout(timer);
  }, [currentDetail.data?.prepared?.start_before]);

  useEffect(() => {
    const conceal = () => {
      if (hidden.current) return;
      const state = volatile.current;
      concealed.current = {
        scope: state.scope, scene: state.scene, route: state.route,
        selectedID: state.selectedID, boundary: boundary.current,
        cookie: cookie.current || csrfCookie(), form: state.form, newMode: state.newMode,
        dirty: dirty.current, actionError: pending.current ? "uncertain" : state.actionError,
        formError: state.formError,
      };
      hidden.current = true;
      revealEpoch.current++;
      pageEpoch.current++;
      detailEpoch.current++;
      actionEpoch.current++;
      pageController.current?.abort();
      detailController.current?.abort();
      cookie.current = "";
      // Only rendered data is removed here. The volatile draft and request key
      // survive a temporary tab switch or bfcache round trip.
      flushSync(() => {
        setPage({ scope: state.scope, status: "hidden", data: null });
        setDetail({ key: detailKey, status: "hidden", data: null });
        setForm(blank);
        setNewMode(false);
        setActionError(null);
        setFormError("");
        setBusy(false);
      });
    };
    const visibility = () => document.visibilityState === "hidden" ? conceal() : reveal();
    const onMessage = (event: MessageEvent) => { if (event.data?.type === "logout") clear("signed-out", true); };
    const onStorage = (event: StorageEvent) => { if (event.key === "commerce-session-logout") clear("signed-out", true); };
    const onLocalLogout = () => clear("signed-out", true);
    const onHistory = () => {
      explicitDeparture.current = false;
      retainHistory();
      clear("loading");
      setPageReload((v) => v + 1);
      setDetailReload((v) => v + 1);
    };
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      if (!dirty.current && !pending.current && !concealed.current?.dirty &&
        volatile.current.actionError !== "uncertain") return;
      event.preventDefault();
      event.returnValue = "";
    };
    const onFocus = () => {
      if (hidden.current) { reveal(); return; }
      if (!boundary.current) return;
      void sessionBoundary().then((value) => { if (boundary.current && value !== boundary.current) clear("signed-out", true); })
        .catch(() => clear("signed-out", true));
    };
    let channel: BroadcastChannel | null = null;
    try { channel = new BroadcastChannel("commerce-session"); channel.addEventListener("message", onMessage); }
    catch { /* storage event and focus fence remain */ }
    document.addEventListener("visibilitychange", visibility);
    window.addEventListener("pagehide", conceal);
    window.addEventListener("pageshow", reveal);
    window.addEventListener("storage", onStorage);
    window.addEventListener("commerce-session-logout", onLocalLogout);
    window.addEventListener("popstate", onHistory);
    window.addEventListener("beforeunload", onBeforeUnload);
    window.addEventListener("focus", onFocus);
    if (document.visibilityState === "hidden") conceal();
    else if (hidden.current) reveal();
    return () => {
      document.removeEventListener("visibilitychange", visibility);
      window.removeEventListener("pagehide", conceal);
      window.removeEventListener("pageshow", reveal);
      window.removeEventListener("storage", onStorage);
      window.removeEventListener("commerce-session-logout", onLocalLogout);
      window.removeEventListener("popstate", onHistory);
      window.removeEventListener("beforeunload", onBeforeUnload);
      window.removeEventListener("focus", onFocus);
      channel?.removeEventListener("message", onMessage);
      channel?.close();
    };
  }, [clear, reveal]);

  useEffect(() => () => { retainHistory(); }, []);

  function mayLeave(depart = false) {
    if (recoveryGuard) return false;
    if (pending.current || actionError === "uncertain") return false;
    if (dirty.current && !window.confirm(c.dirty)) return false;
    if (sameRecovery(historyRecovery, scope, scene)) historyRecovery = null;
    if (depart) explicitDeparture.current = true;
    return true;
  }
  function navigate(nextStore: string, nextCursor: string, nextScene: string) {
    if (!mayLeave(true)) return false;
    dirty.current = false;
    setNewMode(false);
    if (nextStore !== storeID || nextCursor !== cursor) clear("loading");
    else {
      detailEpoch.current++;
      detailController.current?.abort();
      setDetail({ key: `${nextStore}|${nextScene}`, status: "loading", data: null });
    }
    router.push(studioURL(locale, nextStore, nextCursor, nextScene));
    return true;
  }
  function newScene() {
    if (recoveryElsewhere || recoveryGuard || !mayLeave()) return;
    explicitDeparture.current = false;
    dirty.current = false;
    setNewMode(true);
    setForm(blank);
    setFormError("");
    if (actionError !== "uncertain") setActionError(null);
  }
  function refresh() {
    if (hidden.current) return;
    if (dirty.current && !window.confirm(c.dirty)) return;
    if (!pending.current && sameRecovery(historyRecovery, scope, scene)) {
      historyRecovery = null;
      concealed.current = null;
      setNewMode(false);
      setForm(blank);
    }
    dirty.current = false;
    blocked.current = false;
    if (actionError !== "uncertain") setActionError(null);
    setFormError("");
    if (initialError || !storeID) { router.refresh(); return; }
    setPageReload((value) => value + 1);
    setDetailReload((value) => value + 1);
  }
  async function perform(action: Action, body: Pending["body"], sessionID: string, reuse?: Pending) {
    if (!storeID || busy || recoveryElsewhere || recoveryGuard) return;
    explicitDeparture.current = false;
    const signature = JSON.stringify({ storeID, action, sessionID, body });
    if (pending.current && pending.current.signature !== signature && actionError === "uncertain") return;
    const request = reuse ?? (pending.current?.signature === signature ? pending.current :
      { signature, key: crypto.randomUUID(), action, body, sessionID, storeID, boundary: boundary.current });
    pending.current = request;
    const epoch = ++actionEpoch.current;
    setBusy(true);
    setActionError(null);
    try {
      const current = await sessionBoundary().catch(() => { throw new StudioError("signed-out"); });
      if (!request.boundary || request.storeID !== storeID || current !== request.boundary ||
        boundary.current !== request.boundary) throw new StudioError("signed-out");
      if (epoch !== actionEpoch.current || hidden.current) return;
      if (action === "create") {
        const draft = await createStudioDraft(storeID, body as DraftInput, request.key, current);
        if (epoch !== actionEpoch.current) return;
        pending.current = null;
        dirty.current = false;
        setNewMode(false);
        setPageReload((value) => value + 1);
        navigate(storeID, "", draft.session_id);
      } else if (action === "edit") {
        const input = body as EditInput;
        await editStudioDraft(storeID, sessionID,
          { title: input.title, scheduled_at: input.scheduled_at, aspect_ratio: input.aspect_ratio },
          input.expected_version, request.key, current);
        if (epoch !== actionEpoch.current) return;
        pending.current = null;
        dirty.current = false;
        setDetailReload((value) => value + 1);
        setPageReload((value) => value + 1);
      } else if (action === "start") {
        const input = body as { authorization_id: string; expected_session_version: number };
        await startStudioRehearsal(storeID, sessionID, input.authorization_id, input.expected_session_version, request.key, current);
        if (epoch !== actionEpoch.current) return;
        pending.current = null;
        setDetailReload((value) => value + 1);
        setPageReload((value) => value + 1);
      } else {
        await stopStudioRehearsal(storeID, sessionID, (body as { attempt_id: string }).attempt_id, request.key, current);
        if (epoch !== actionEpoch.current) return;
        pending.current = null;
        setDetailReload((value) => value + 1);
      }
    } catch (error) {
      if (epoch !== actionEpoch.current) return;
      const code = errorCode(error);
      if (code !== "uncertain") pending.current = null;
      if (code === "signed-out") clear(code, true);
      else {
        setActionError(code);
        if (code === "conflict" || code === "forbidden") {
          setDetailReload((value) => value + 1);
          setPageReload((value) => value + 1);
        }
      }
    } finally { if (epoch === actionEpoch.current) setBusy(false); }
  }
  function save() {
    if (recoveryElsewhere || recoveryGuard) return;
    const input = draftInput(form);
    if (!input) {
      setFormError(form.title.trim() !== form.title || Array.from(form.title).length < 1 ||
        Array.from(form.title).length > 200 || /[\p{Cc}]/u.test(form.title) ? c.titleInvalid : c.scheduleInvalid);
      return;
    }
    setFormError("");
    if (newMode) void perform("create", input, "");
    else if (shown?.can_manage && shown.draft.state === "DRAFT")
      void perform("edit", { ...input,
        scheduled_at: form.scheduled === utcMinute(shown.draft.scheduled_at)
          ? shown.draft.scheduled_at : input.scheduled_at,
        expected_version: shown.draft.version }, shown.draft.session_id);
  }
  const statusText = (status: Status) => status === "signed-out" ? c.signedOut : status === "forbidden" ? c.forbidden :
    status === "not-found" ? c.notFound : status === "loading" ? c.loading : c.unavailable;
  const actionMessage = (code: StudioErrorCode) => code === "conflict" ? c.conflict :
    code === "invalid" ? c.invalid : code === "uncertain" ? c.uncertain : statusText(code);
  const attempt = shown?.attempt;
  const prepared = shown?.prepared;
  const preparedCurrent = !!prepared && Date.parse(prepared.start_before) > now;
  const actionBlocked = recoveryElsewhere || recoveryGuard || actionError === "uncertain" || actionError === "conflict";
  const canStart = !!shown?.can_manage && shown.draft.state === "DRAFT" && !!prepared && preparedCurrent && !attempt && !formDirty && !actionBlocked;
  const canStop = !!shown?.can_manage && !!attempt && !attempt.stop_requested && !attempt.escalated && attempt.resource_state !== "TERMINAL" && !actionBlocked;
  const canEdit = !recoveryElsewhere && !recoveryGuard && (newMode || (!!shown?.can_manage && shown.draft.state === "DRAFT"));
  // Planning-only Studio (media_enabled=false) has no rehearsal column; save errors stay visible below the editor.
  const actionAlert = actionError && <div role="alert" className="studio-action-error">
    <p>{actionMessage(actionError)}</p>
    {actionError === "uncertain" && pending.current && <button type="button" disabled={busy}
      onClick={() => { const value = pending.current; if (value) void perform(value.action, value.body, value.sessionID, value); }}>
      {c.retrySame}</button>}
  </div>;

  return <WorkspaceFrame locale={locale} storeName={store?.name ?? c.noStore} active="live" onBeforeNavigate={() => mayLeave(true)}>
    <div className="studio-page" data-testid="merchant-studio">
      <header className="studio-heading">
        <div><h1>{c.title}</h1><p>{c.subtitle}</p></div>
        <div className="studio-heading-actions">
          {stores.length > 1 && <label>{c.store}<select value={storeID} onChange={(event) => {
            previous.current = []; navigate(event.target.value, "", "");
          }}>{stores.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>}
          {shown && <button type="button" className="studio-refresh" data-testid="studio-open-claims"
            onClick={() => { if (mayLeave(true)) router.push(`/${locale}/studio/claims?store=${encodeURIComponent(storeID)}&scene=${encodeURIComponent(shown.draft.session_id)}`); }}>
            {c.claims}</button>}
          <button type="button" className="studio-refresh" onClick={refresh}>{c.refresh}</button>
        </div>
      </header>
      {recoveryElsewhere && historyRecovery && <p className="studio-note" role="status">
        {locale === "zh-CN" ? "另一场次有待处理的草稿或请求。请先返回该场次。" :
          locale === "zh-TW" ? "另一場次有待處理的草稿或請求。請先返回該場次。" :
            "Another session has an unfinished draft or request. Return to it before editing here."}
        {" "}<button type="button" onClick={() => { if (recoveryRoute) router.push(recoveryRoute); }}>
          {locale === "zh-CN" ? "返回待处理场次" : locale === "zh-TW" ? "返回待處理場次" : "Return to session"}
        </button>
      </p>}
      <div className={`studio-surface${mediaOn ? "" : " studio-planning-only"}`}>
        <section className="studio-scenes" aria-label={c.scenes}>
          <button type="button" className="primary studio-new" disabled={recoveryGuard || recoveryElsewhere || !storeID || busy || actionError === "uncertain" || currentPage.status === "forbidden" || shown?.can_manage === false}
            onClick={newScene}>＋ {c.newScene}</button>
          <h2 className="sr-only">{c.scenes}</h2>
          {currentPage.status === "ready" ? currentPage.data?.items.length ? <>
            <div className="studio-scene-list">
              {currentPage.data.items.map((item) => <button key={item.session_id} type="button"
                className={`studio-scene ${!newMode && selectedID === item.session_id ? "selected" : ""}`}
                aria-current={!newMode && selectedID === item.session_id ? "true" : undefined}
                onClick={() => { navigate(storeID, cursor, item.session_id); }}>
                <strong>{item.title}</strong><span>{c.state[item.state as keyof typeof c.state] ?? item.state}</span>
                <small>{item.scheduled_at ? time(locale, item.scheduled_at) + " UTC" : c.schedule}</small>
              </button>)}
            </div>
            <div className="studio-pager">
              <button type="button" disabled={!previous.current.length} onClick={() => {
                const target = previous.current[previous.current.length - 1] ?? "";
                if (navigate(storeID, target, "")) previous.current.pop();
              }}>{c.previous}</button>
              <button type="button" disabled={!currentPage.data.next_cursor} onClick={() => {
                if (navigate(storeID, currentPage.data!.next_cursor, "")) previous.current.push(cursor);
              }}>{c.next}</button>
            </div>
          </> : <p className="studio-list-message">{c.empty}</p> :
            <div className="studio-list-message" role={currentPage.status === "loading" ? "status" : "alert"}>
              {currentPage.status === "loading" ? c.loading : statusText(currentPage.status)}
            </div>}
        </section>
        <section className="studio-editor" aria-label={c.editor}>
          <h2>{c.editor}</h2><p className="studio-muted">{c.editorHint}</p>
          {newMode || shown ? <>
            <div className="studio-fields">
              <label>{c.name}<input value={form.title} maxLength={400} disabled={!canEdit || busy}
                onChange={(event) => { explicitDeparture.current = false; setForm({ ...form, title: event.target.value }); setFormError(""); }} /></label>
              {locale === "en" ? <div className="studio-schedule-field">
                <label htmlFor="studio-schedule-entry">{c.schedule}</label>
                <div className="studio-schedule-control">
                  <input id="studio-schedule-entry" type="text" placeholder="YYYY-MM-DDTHH:mm" maxLength={16}
                    value={form.scheduled} disabled={!canEdit || busy}
                    onChange={(event) => { explicitDeparture.current = false; setForm({ ...form, scheduled: event.target.value }); setFormError(""); }} />
                  <span className="studio-schedule-picker">
                    <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="5" width="18" height="16" rx="2" /><path d="M7 3v4M17 3v4M3 10h18" /></svg>
                    <input type="datetime-local" aria-label={c.pickSchedule} title={c.pickSchedule}
                      value={form.scheduled} disabled={!canEdit || busy}
                      min="2000-01-01T00:00" max="2199-12-31T23:59"
                      onClick={(event) => event.currentTarget.showPicker?.()}
                      onChange={(event) => { explicitDeparture.current = false; setForm({ ...form, scheduled: event.target.value }); setFormError(""); }} />
                  </span>
                </div>
              </div> : <label>{c.schedule}<input type="datetime-local" value={form.scheduled} disabled={!canEdit || busy}
                min="2000-01-01T00:00" max="2199-12-31T23:59"
                onChange={(event) => { explicitDeparture.current = false; setForm({ ...form, scheduled: event.target.value }); setFormError(""); }} /></label>}
              <label>{c.aspect}<select value={form.aspect} disabled={!canEdit || busy}
                onChange={(event) => { explicitDeparture.current = false; setForm({ ...form, aspect: event.target.value as AspectRatio }); }}>
                <option value="9:16">{c.tall}</option><option value="16:9">{c.wide}</option>
              </select></label>
            </div>
            <div className="studio-version">
              <h3>{c.version}</h3>
              {shown ? <div><strong>{c.state[shown.draft.state as keyof typeof c.state] ?? shown.draft.state} / {c.version} {shown.draft.version}</strong>
                <span>{c.savedAt}: {time(locale, shown.draft.updated_at)}</span></div> : <p>{c.newDraft}</p>}
            </div>
            {!canEdit && shown && !recoveryElsewhere && !recoveryGuard && <p className="studio-note">{shown.can_manage ? c.notDraftEditable : c.readOnly}</p>}
            {formDirty && <p className="studio-dirty">{c.unsaved}</p>}
            {formError && <p role="alert" className="studio-error">{formError}</p>}
            <button type="button" className="primary studio-save" disabled={!canEdit || busy || (!newMode && !formDirty) || actionError === "uncertain" || actionError === "conflict"}
              onClick={save}>{busy ? c.saving : newMode ? c.create : c.save}</button>
          </> : currentDetail.status === "loading" && currentPage.status === "ready" ?
            <p className="studio-panel-message" role="status">{c.detailLoading}</p> :
            <p className="studio-panel-message" role="status">{selectedID ? statusText(currentDetail.status) : c.select}</p>}
          {!mediaOn && actionAlert}
        </section>
        {mediaOn && <aside className="studio-status" aria-label={c.rehearsal}>
          <div className="studio-status-title"><h2>{c.rehearsal}</h2><span>MOCK</span></div>
          <p className="studio-notice">{c.localOnly}</p>
          {shown ? <>
            <section className="studio-fact">
              <h3>{c.prepared}</h3>
              {prepared ? <>
                <p className="studio-muted">{preparedCurrent ? `${c.preparedUntil}: ${time(locale, prepared.start_before)}` : c.expiredPrepared}</p>
              </> : !attempt ? <p className="studio-muted">{c.noPrepared}</p> : null}
              {(prepared || attempt) && <><p className="studio-destination-label">{c.destinations}</p>
                <ul>{(prepared?.destinations ?? attempt!.destinations).map((item) => <li key={item.ordinal}>
                  <div className="studio-provider-name">
                    {item.provider === "facebook" && <svg className="studio-facebook-mark" viewBox="0 0 24 24" aria-hidden="true">
                      <path fill="currentColor" d="M22 12a10 10 0 1 0-11.56 9.87v-6.98H7.9V12h2.54V9.8c0-2.51 1.49-3.89 3.78-3.89 1.09 0 2.24.2 2.24.2v2.46h-1.26c-1.25 0-1.63.77-1.63 1.56V12h2.77l-.44 2.89h-2.33v6.98A10 10 0 0 0 22 12Z" />
                    </svg>}
                    <strong>{c.provider[item.provider]}</strong>
                  </div><span>{c.unverified}</span>
                </li>)}</ul></>}
            </section>
            <section className="studio-fact studio-observation">
              <h3>{c.observed}</h3>
              <div className="studio-current-state">{attempt ? <dl>
                <div><dt>{c.operation}</dt><dd>{c.operationState[attempt.operation_state as keyof typeof c.operationState] ?? attempt.operation_state}</dd></div>
                <div><dt>{c.resource}</dt><dd>{c.resourceState[attempt.resource_state]}</dd></div>
                <div><dt>{c.transport}</dt><dd>{attempt.transport_status || c.noTransport}</dd></div>
                <div><dt>{c.statusAt}</dt><dd>{time(locale, attempt.updated_at)}</dd></div>
                {attempt.cleanup_required && <div><dt>{c.cleanup}</dt><dd>{c.yes}</dd></div>}
                {attempt.stop_requested && <div><dt>{c.stopRequested}</dt><dd>{c.yes}</dd></div>}
                {attempt.escalated && <div className="studio-escalated"><dt>{c.escalated}</dt><dd>{c.yes}</dd></div>}
              </dl> : <div className="studio-current-empty">
                <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></svg>
                <div><strong>{c.noAttemptTitle}</strong><p>{c.noAttempt}</p></div>
              </div>}</div>
            </section>
            <button type="button" className="primary studio-rehearsal-action" disabled={busy || actionError === "uncertain" || !(attempt ? canStop : canStart)}
              onClick={() => {
                if (attempt && canStop) void perform("stop", { attempt_id: attempt.attempt_id }, shown.draft.session_id);
                else if (prepared && canStart) void perform("start", { authorization_id: prepared.authorization_id,
                  expected_session_version: prepared.session_version }, shown.draft.session_id);
              }}>{busy ? c.working : attempt ? c.stop : c.start}</button>
            {!shown.can_manage && <p className="studio-muted">{c.readOnly}</p>}
          </> : <p className="studio-panel-message">{newMode ? c.noPrepared : c.noAttempt}</p>}
          {actionAlert}
        </aside>}
      </div>
    </div>
  </WorkspaceFrame>;
}
