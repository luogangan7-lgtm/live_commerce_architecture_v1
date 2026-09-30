// Stripe buyer UI browser gates: SP18 (= SU06, SANDBOX), SU07, SU08, SU09 (MOCK). Written from
// contracts/stripe-buyer-ui-v1.md (FROZEN) and stripe-psp-v1.md §14 SP18, not from the UI code.
// Chain: Chromium -> TLS edge (https://buyer.example) -> production Next -> private Go -> PG -> worker.
// SP18 alone reaches real Stripe hosts, only through the CONNECT tunnel below; the store tab never
// sees a Stripe key. The SP18 file name in the contract is stripe-browser.spec.ts; this repo drives
// browsers from Go-launched .mjs scripts (tests/storefront/buyer-payment-browser.mjs), so it does too.
//
// Calls: BFF /api/buyer/orders/{id}/payment[/prepare|/handoff|/refresh|/cancel] and Go /v1/buyer/...
// (observed at the edge); Go control server /facts and /act (counts and fixture actions only).
// Output: result.json (+ PNGs) in LC_STRIPE_EVIDENCE. Every console line is redacted: a Stripe URL,
// session id, key or synthetic email must never reach browser.log (SU08).
import assert from "node:assert/strict";
import {createHash} from "node:crypto";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import {spawn,execFileSync} from "node:child_process";
import {once} from "node:events";
import {readFile,writeFile,mkdtemp,rm} from "node:fs/promises";
import {createWriteStream} from "node:fs";
import {tmpdir} from "node:os";
import path from "node:path";
import { expect } from "@playwright/test";
import { launch, ctxOpts, phone, phoneName } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const root=process.cwd(),evidence=process.env.LC_STRIPE_EVIDENCE,CONTROL=process.env.LC_STRIPE_CONTROL;
const cfg=JSON.parse(process.env.LC_STRIPE_CASE??"{}");
assert(evidence&&/^http:\/\/127\.0\.0\.1:\d+$/.test(CONTROL??""),"driver env");
assert(["SP18","SU07","SU09","OBS"].includes(cfg.kind)&&["SANDBOX","MOCK"].includes(cfg.mode),"case");
const SANDBOX=cfg.mode==="SANDBOX";
const origin="https://buyer.example",product=`${origin}/en/products/${process.env.LC_STRIPE_PRODUCT}`;
const ex=expect.configure({timeout:20000});
const pause=ms=>new Promise(r=>setTimeout(r,ms));
const within=(p,ms,fallback)=>Promise.race([Promise.resolve(p).catch(()=>fallback),new Promise(r=>setTimeout(()=>r(fallback),ms))]); // a wedged page must not wedge the driver
const listen=async s=>{s.listen(0,"127.0.0.1");await once(s,"listening");return s.address().port;};
// ---- redaction: nothing that identifies a Stripe session may reach stdout/stderr (SU08) ----
const redact=s=>String(s).replace(/https:\/\/checkout\.stripe\.com\/\S*/gi,"[stripe-url]").replace(/cs_(?:test|live)_\w+/g,"[cs]")
  .replace(/gate\+\d+@example\.com/g,"[email]").replace(/(?:sk|rk)_(?:test|live)_\w+/g,"[key]");
const say=(...a)=>console.log(redact(a.join(" ")));
const unreached=[],cases=[],shots=[],selectors={},refused=new Set(),orders=[],calls=[],consoleLines=[],histCalls=[];
const children=new Set(),sockets=new Set(),contexts=[],logs=[];
let browser,edge,proxy,stripeVisits=0,payuniHits=0,nextPort=0;
const passed=name=>{cases.push(name);say(`PASS ${name}`);};
const t0=Date.now(),step=(...a)=>say(`[+${Math.round((Date.now()-t0)/1000)}s]`,...a);
// ---- copy: contract §7 + the approved payment/order copy (independent of the implementation) ----
const LOC={
  en:{pay:"Pay in a new tab",recover:"Continue original payment",cancel:"Cancel payment",refresh:"Refresh order",paymentState:"Payment status",orderState:"Order status",
    NOT_STARTED:"Not paid",PENDING:"Waiting for the payment result",CAPTURED:"Payment capture recorded",REVIEW_REQUIRED:"Payment needs review — do not pay again",CLOSED_UNPAID:"Payment closed without charge",
    DRAFT:"Not paid",AWAITING_PAYMENT:"Payment pending",CONFIRMED:"Order confirmed",CANCELLED:"Order canceled",
    failed:"Payment status could not be confirmed. Refresh this order to check again; do not place it again.",
    blocked:"The payment tab could not be opened. Allow pop-ups for this store, then try again.",
    unavailable:"No payment method is available for this order. Creating an order does not take payment.",
    submitted:"Payment handoff requested. Check the other tab, then refresh this order for the result. This does not confirm payment.",
    readOnly:"Continue in the payment tab already opened. If it is unavailable, refresh this order or contact the store; do not pay again.",
    cancelConfirm:"Cancel this payment? The order is cancelled once the payment provider confirms. If you already paid, the payment stands.",
    cancelling:"Cancellation requested. Waiting for the payment provider to confirm; refresh this order to check.",
    creating:"The secure payment page is still being prepared. Try Continue original payment again shortly.",
    cutoff:"This payment page can no longer be opened. Refresh this order for the result."},
  "zh-CN":{pay:"前往新标签页付款",recover:"继续原付款请求",cancel:"取消付款",refresh:"刷新订单",paymentState:"付款状态",orderState:"订单状态",
    NOT_STARTED:"尚未付款",PENDING:"等待付款结果",CAPTURED:"已记录付款请款结果",REVIEW_REQUIRED:"付款需要核查，请勿重复付款",CLOSED_UNPAID:"付款已关闭，未扣款",
    DRAFT:"尚未付款",AWAITING_PAYMENT:"等待支付结果",CONFIRMED:"订单已确认",CANCELLED:"订单已取消",
    failed:"暂时无法确认付款状态，请刷新此订单查询，不要重复下单。",
    blocked:"未能打开付款标签页，请允许此商店弹出窗口后重试。",unavailable:"此订单暂时没有可用的付款方式。创建订单不会扣款。",
    submitted:"已请求跳转付款，请查看另一个标签页，再刷新此订单查询结果。这不代表付款成功。",
    readOnly:"请在已打开的付款标签页继续。如无法使用，请刷新此订单或联系商家，不要重复付款。",
    cancelConfirm:"确定取消付款？支付服务商确认后订单会取消。如已付款，以付款为准。",cancelling:"已请求取消，正在等待支付服务商确认，请刷新此订单查询。",
    creating:"安全付款页仍在准备中，请稍后再点击“继续原付款请求”。",cutoff:"此付款页已无法再打开，请刷新此订单查询结果。"},
  "zh-TW":{pay:"前往新分頁付款",recover:"繼續原付款請求",cancel:"取消付款",refresh:"重新整理訂單",paymentState:"付款狀態",orderState:"訂單狀態",
    NOT_STARTED:"尚未付款",PENDING:"等待付款結果",CAPTURED:"已記錄付款請款結果",REVIEW_REQUIRED:"付款需要核查，請勿重複付款",CLOSED_UNPAID:"付款已關閉，未扣款",
    DRAFT:"尚未付款",AWAITING_PAYMENT:"等待付款結果",CONFIRMED:"訂單已確認",CANCELLED:"訂單已取消",
    failed:"暫時無法確認付款狀態，請重新整理此訂單查詢，不要重複下單。",
    blocked:"無法開啟付款分頁，請允許此商店的彈出式視窗後重試。",unavailable:"此訂單暫時沒有可用的付款方式。建立訂單不會扣款。",
    submitted:"已請求跳轉付款，請查看另一個分頁，再重新整理此訂單查詢結果。這不代表付款成功。",
    readOnly:"請在已開啟的付款分頁繼續。如無法使用，請重新整理此訂單或聯絡商家，不要重複付款。",
    cancelConfirm:"確定取消付款？支付服務商確認後訂單會取消。如已付款，以付款為準。",cancelling:"已請求取消，正在等待支付服務商確認，請重新整理此訂單查詢。",
    creating:"安全付款頁仍在準備中，請稍後再點選「繼續原付款請求」。",cutoff:"此付款頁已無法再開啟，請重新整理此訂單查詢結果。"},
};
const pii={recipient_name:"Synthetic Gate Recipient",phone:"+886900000091",region:"Synthetic Region",city:"Synthetic City",postal_code:"99991",line1:"Synthetic Address Ninety One",line2:"Synthetic Unit Ninety Two"};
const email=`gate+${Date.now()%1000000000}@example.com`; // synthetic; SU08 sentinel
// ---- control server (counts only) ----
const CH={"X-Gate-Key":process.env.LC_STRIPE_CONTROL_KEY};
async function control(order){const r=await fetch(`${CONTROL}/facts${order?`?order=${order}`:""}`,{headers:CH});assert.equal(r.status,200,"facts");return r.json();}
async function act(name,order){const r=await fetch(`${CONTROL}/act?name=${name}${order?`&order=${order}`:""}`,{method:"POST",headers:CH});assert.equal(r.status,200,`act ${name}`);}
async function until(fn,ms,what,step=400){const end=Date.now()+ms;for(;;){const v=await fn();if(v)return v;if(Date.now()>end)throw new Error(`timeout: ${what}`);await pause(step);}}
// ---- edge + proxy + Next (same pattern as tests/storefront/buyer-payment-browser.mjs) ----
function relay(port,req,body){
  return new Promise((resolve,reject)=>{
    const headers={...req.headers};delete headers.connection;delete headers["transfer-encoding"];
    if(body.length)headers["content-length"]=String(body.length);else delete headers["content-length"];
    const call=http.request({hostname:"127.0.0.1",port,path:req.url,method:req.method,headers},res=>{
      const chunks=[];res.on("data",x=>chunks.push(x));res.on("error",reject);res.on("end",()=>resolve({status:res.statusCode,headers:res.headers,body:Buffer.concat(chunks)}));
    });call.setTimeout(15000,()=>call.destroy(new Error("owned relay deadline")));call.on("error",reject);call.end(body);
  });
}
async function startNext(){
  const reserve=net.createServer(),port=await listen(reserve);await new Promise(r=>reserve.close(r));
  const log=createWriteStream(path.join(evidence,"next.log"),{flags:"wx",mode:0o600});logs.push(log);await once(log,"open");
  const env={...process.env,NODE_ENV:"production",NEXT_TELEMETRY_DISABLED:"1"};
  for(const key of Object.keys(env))if(key.startsWith("LC_STRIPE_"))delete env[key];
  const child=spawn(process.execPath,[path.join(root,"apps/storefront/node_modules/next/dist/bin/next"),"start","--hostname","127.0.0.1","--port",String(port)],{cwd:path.join(root,"apps/storefront"),env,stdio:["ignore",log,log]});children.add(child);
  for(let i=0;i<120;i++){
    if(child.exitCode!==null)throw new Error("owned Next failed readiness");
    try{if((await relay(port,{url:"/api/buyer/session",method:"GET",headers:{host:"buyer.example"}},Buffer.alloc(0))).status===200)return port;}catch{}
    await pause(50);
  }throw new Error("owned Next readiness timeout");
}
const STRIPE_HOSTS=/^(?:[a-z0-9-]+\.)*(?:stripe\.com|stripe\.network|stripecdn\.com):443$/;
function tunnel(socket,head,host,port){
  const upstream=net.connect(port,host,()=>{socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");if(head.length)upstream.write(head);socket.pipe(upstream).pipe(socket);});
  for(const s of [socket,upstream]){sockets.add(s);s.on("close",()=>sockets.delete(s));s.on("error",()=>{socket.destroy();upstream.destroy();});}
}
async function startEdge(){
  const key=await readFile(path.join(certDir,"key.pem")),cert=await readFile(path.join(certDir,"cert.pem"));
  edge=https.createServer({key,cert},async(req,res)=>{
    try{
      const chunks=[];for await(const x of req)chunks.push(x);const body=Buffer.concat(chunks);
      const p=new URL(req.url,origin).pathname;
      if(p.startsWith("/api/buyer/orders/")&&p.includes("/payment"))calls.push({path:p,method:req.method,key:req.headers["idempotency-key"]??null,body:body.length?body.toString():null,at:Date.now()});
      const out=await relay(nextPort,req,body);
      const headers={...out.headers};delete headers.connection;delete headers["transfer-encoding"];
      res.writeHead(out.status,headers);res.end(out.body);
    }catch(e){step("edge relay error",String(e?.code??e?.message).slice(0,80));if(!res.headersSent)res.writeHead(502);res.end();}
  });
  const edgePort=await listen(edge);proxy=http.createServer((_,r)=>{r.writeHead(403);r.end();});
  proxy.on("connect",(req,socket,head)=>{
    if(req.url==="buyer.example:443"){tunnel(socket,head,"127.0.0.1",edgePort);return;}
    if(SANDBOX&&STRIPE_HOSTS.test(req.url)){const [h,p]=req.url.split(":");tunnel(socket,head,h,Number(p));return;}
    refused.add(req.url.replace(/:443$/,"")); // hostname only; recorded, refused; the run fails only if the flow cannot complete
    socket.destroy();
  });
  return listen(proxy);
}
const certDir=await mkdtemp(path.join(tmpdir(),"lc-stripe-edge-"));
// ---- browser contexts ----
const INIT=`(()=>{
  window.__hist=[];for(const m of ["pushState","replaceState"]){const o=history[m];history[m]=function(...a){try{window.__hist.push(String(a[2]??""));}catch{}return o.apply(this,a);};}
  window.__live=[];const sel='[role="status"],[role="alert"]',inSection=e=>e&&e.closest&&e.closest('[data-testid="order-payment"]');
  const note=(role,text)=>{text=(text||"").trim();if(text)window.__live.push({role,text,t:performance.now()});};
  new MutationObserver(ms=>{for(const m of ms){
    const t=m.target.nodeType===1?m.target:m.target.parentElement,r=t&&t.closest&&t.closest(sel);
    if(r&&inSection(r))note(r.getAttribute("role"),r.textContent);
    for(const n of m.addedNodes){if(n.nodeType!==1)continue;const rs=n.matches&&n.matches(sel)?[n]:[...n.querySelectorAll(sel)];for(const x of rs)if(inSection(x))note(x.getAttribute("role"),x.textContent);}
  }}).observe(document,{subtree:true,childList:true,characterData:true});
})();`;
function attach(page){
  page.on("console",m=>{if(page.url().startsWith(origin))consoleLines.push(m.text());});
  page.on("pageerror",e=>{if(page.url().startsWith(origin))consoleLines.push(String(e.message));});
}
async function context(mobile=false){
  const c=await browser.newContext(ctxOpts(mobile?{...phone,viewport:{width:390,height:844},screen:{width:390,height:844},ignoreHTTPSErrors:true}:{ignoreHTTPSErrors:true,viewport:{width:1440,height:900}}));contexts.push(c);
  await c.addInitScript(INIT);
  await c.route(/https:\/\/(?:sandbox-api|api)\.payuni\.com\.tw\//,route=>{payuniHits++;return route.abort();});
  if(!SANDBOX)await c.route(/^https:\/\/checkout\.stripe\.com\//,route=>{stripeVisits++;return route.fulfill({status:200,contentType:"text/html; charset=utf-8",body:"<!doctype html><title>Synthetic Stripe hosted page</title><main>Synthetic hosted page (MOCK)</main>"});});
  c.on("page",p=>{attach(p);attachStep(p);});
  return c;
}
// ---- store-tab helpers ----
const requestIs=(response,suffix,method)=>new URL(response.url()).pathname===`/api/buyer/${suffix}`&&response.request().method()===method;
async function newOrder(page,{pay=true}={}){
  // After a reload the restored order mounts asynchronously: wait for whichever of the two entry points
  // exists rather than counting once (count() is 0 while the page is still hydrating).
  await ex(page.getByTestId("continue-shopping").or(page.locator("#quantity")).first()).toBeVisible({timeout:20000});
  if(await page.getByTestId("continue-shopping").count())await page.getByTestId("continue-shopping").click();
  await ex(page.locator("#quantity")).toBeEnabled();
  await page.locator("#quantity").fill("2");
  await page.getByRole("button",{name:"Choose delivery",exact:true}).click();
  const quotation=page.waitForResponse(r=>requestIs(r,"quotes","POST"));
  await page.getByRole("button",{name:"Get current total",exact:true}).click();assert.equal((await quotation).status(),200);
  await ex(page.getByTestId("address-section")).toBeVisible();
  for(const [key,value] of Object.entries(pii))await page.locator(`input[name="${key}"]`).fill(value);
  await page.getByTestId("confirm-address").click();await ex(page.getByTestId("create-order")).toBeEnabled();
  await page.getByTestId("create-order").click();await ex(page.getByTestId("order-section")).toBeVisible();
  const id=(await page.getByTestId("order-id").innerText()).trim();assert.match(id,/^[0-9a-f-]{36}$/);orders.push(id);
  await ex(page.getByTestId("order-payment")).toBeVisible();await ex(page.getByTestId("payment-status")).toHaveAttribute("data-state","NOT_STARTED");
  const payment=await page.evaluate(async orderID=>{
    const session=await(await fetch("/api/buyer/session",{cache:"no-store"})).json();
    const response=await fetch(`/api/buyer/orders/${orderID}/payment`,{headers:{"X-Buyer-Context":session.context},cache:"no-store"});
    return {status:response.status,body:await response.json()};
  },id);
  assert.equal(payment.status,200);assert.equal(payment.body.currency,"TWD");assert.equal(payment.body.total_minor,2500);
  assert.equal(payment.body.cancel_requested,undefined,"a fresh order has no Stripe attempt");
  if(pay){assert.deepEqual(payment.body.methods.map(x=>[x.code,x.version]).map(x=>x[0]),["stripe_checkout"]);await ex(payBtn(page,"en")).toBeVisible();}
  else assert.equal(payment.body.methods.length,0);
  return id;
}
async function setLocale(page,loc){await page.locator("header select").selectOption(loc);await ex(page.locator("html")).toHaveAttribute("lang",loc);}
const payBtn=(p,l)=>p.getByRole("button",{name:LOC[l].pay,exact:true});
const recoverBtn=(p,l)=>p.getByRole("button",{name:LOC[l].recover,exact:true});
const cancelBtn=(p,l)=>p.getByRole("button",{name:LOC[l].cancel,exact:true});
const refreshBtn=p=>p.getByTestId("refresh-order");
const state=p=>p.getByTestId("payment-status").getAttribute("data-state",{timeout:2000}).catch(()=>null);
function paymentCalls(id,step,method="POST"){return calls.filter(x=>x.path===`/api/buyer/orders/${id}/payment${step?`/${step}`:""}`&&x.method===method);}
async function refreshUntil(page,wanted,ms,label){
  const end=Date.now()+ms;let last=0;
  for(;;){
    const s=await state(page);if(wanted.includes(s))return s;
    if(Date.now()>end)throw new Error(`timeout waiting for payment state ${wanted.join("|")} (${label}); last=${s}`);
    if(Date.now()-last>=12000&&await refreshBtn(page).isEnabled().catch(()=>false)){last=Date.now();await refreshBtn(page).click();}
    await pause(700);
  }
}
async function expectStatusText(page,l,pay,commercial){
  await ex(page.getByTestId("payment-status")).toHaveText(`${LOC[l].paymentState}: ${LOC[l][pay]}`);
  if(commercial)await ex(page.getByTestId("payment-commercial-status")).toHaveText(`${LOC[l].orderState}: ${LOC[l][commercial]}`);
}
async function shot(locator,name,tier){
  const buf=await locator.screenshot({path:path.join(evidence,`${name}.png`)});
  shots.push({name,sha256:createHash("sha256").update(buf).digest("hex"),tier});
}
async function cancelViaUI(page,l,{keyboard=false}={}){
  let dialog=null;page.once("dialog",d=>{dialog={type:d.type(),message:d.message()};return d.accept();});
  if(keyboard){await keyboardTo(page,LOC[l].cancel);await page.keyboard.press("Enter");}else await cancelBtn(page,l).click();
  await until(()=>dialog,10000,"native confirm dialog");
  assert.equal(dialog.type,"confirm");assert.equal(dialog.message,LOC[l].cancelConfirm);
}
async function keyboardTo(page,name){
  await page.evaluate(()=>{document.activeElement?.blur?.();window.scrollTo(0,0);});
  for(let i=0;i<150;i++){
    await page.keyboard.press("Tab");
    if(await page.evaluate(()=>{const a=document.activeElement;return a&&a.tagName==="BUTTON"?a.textContent.trim():"";})===name)return;
  }
  throw new Error("keyboard focus never reached the named button");
}
async function closeAll(pages){for(const p of pages)await p.close().catch(()=>{});}
// ---- SU08: scan every store-origin surface ----
const LEAKS=[/checkout\.stripe\.com\//i,/cs_(?:test|live)_/i,/\b(?:sk|rk)_(?:test|live)_/i,/gate\+\d+@example\.com/i];
const hits=text=>LEAKS.filter(re=>re.test(text)).map(String);
async function pageDump(p){
  return p.evaluate(async()=>{
    const out={local:{},session:{},idb:[],cookie:document.cookie,url:location.href,hist:window.__hist||[]};
    for(let i=0;i<localStorage.length;i++){const k=localStorage.key(i);out.local[k]=localStorage.getItem(k);}
    for(let i=0;i<sessionStorage.length;i++){const k=sessionStorage.key(i);out.session[k]=sessionStorage.getItem(k);}
    const dbs=indexedDB.databases?await indexedDB.databases():[];
    for(const d of dbs)await new Promise(res=>{const r=indexedDB.open(d.name);r.onerror=()=>res();r.onsuccess=()=>{const db=r.result,names=[...db.objectStoreNames];if(!names.length){db.close();res();return;}
      const tx=db.transaction(names,"readonly");let n=names.length;for(const s of names){const q=tx.objectStore(s).getAll();const fin=()=>{if(--n===0){db.close();res();}};q.onsuccess=()=>{out.idb.push({db:d.name,store:s,rows:q.result});fin();};q.onerror=fin;}};});
    return out;
  }).catch(()=>null);
}
async function scanSurfaces(pages,ctxs){
  const s={localStorage:"",sessionStorage:"",indexedDB:"",cookies:"",url_history:"",console:consoleLines.join("\n"),"next.log":""};
  for(const p of pages){
    if(p.isClosed()||!p.url().startsWith(origin))continue;
    const d=await pageDump(p);if(!d)continue;
    s.localStorage+=JSON.stringify(d.local);s.sessionStorage+=JSON.stringify(d.session);s.indexedDB+=JSON.stringify(d.idb);
    s.cookies+=d.cookie;s.url_history+=d.url+JSON.stringify(d.hist);
  }
  for(const c of ctxs)s.cookies+=JSON.stringify(await c.cookies().catch(()=>[]));
  s.url_history+=JSON.stringify(histCalls);
  s["next.log"]=await readFile(path.join(evidence,"next.log"),"utf8").catch(()=>"");
  const found={};for(const [k,v] of Object.entries(s)){const h=hits(v);if(h.length)found[k]=h;}
  return {clean:Object.keys(found).length===0,found,surfaces:Object.keys(s)};
}
// The scan must be able to fail (PROCESS §2.4): an injected URL in localStorage is detected.
async function scanSelfTest(page,ctxs){
  await page.evaluate(()=>localStorage.setItem("__scan_selftest","https://checkout.stripe.com/c/pay/cs_test_selftest"));
  const bad=await scanSurfaces([page],ctxs);
  await page.evaluate(()=>localStorage.removeItem("__scan_selftest"));
  assert.equal(bad.clean,false,"SU08 scan cannot fail");assert(bad.found.localStorage,"SU08 scan missed the injected URL");
  passed("SU08 scan self-test: injected localStorage URL is detected");
}
async function finalScan(pages,ctxs){
  if(cfg.mutate==="su08"&&pages[0])await pages[0].evaluate(()=>localStorage.setItem("mutant","https://checkout.stripe.com/c/pay/cs_test_mutant")); // red-run mutation, harness-only
  const scan=await scanSurfaces(pages,ctxs);
  assert.equal(scan.clean,true,`SU08: leak sentinels found in ${Object.keys(scan.found).join(",")}`);
  assert.equal(payuniHits,0,"PAYUNi host contacted");
  passed("SU08 no Stripe URL, session id, key or synthetic email in storage, cookies, history, console, Next log");
  return scan;
}
// ---- real Stripe hosted page (SANDBOX). Selectors are observed, not contracted; every hit is recorded. ----
async function detectCaptcha(child){
  // Stripe.js keeps an INVISIBLE hCaptcha frame on most pages; only a VISIBLE captcha frame or text is a challenge.
  const visible=child.locator('iframe[src*="hcaptcha" i]:visible, iframe[src*="recaptcha" i]:visible, iframe[title*="captcha" i]:visible');
  if(await within(visible.count(),2500,0)>0)return true;
  return (await within(child.getByText(/verify (?:you are|that you're) (?:a )?human|not a robot|select all images/i).count(),2500,0))>0;
}
async function pick(child,label,cands,ms,optional=false){
  const end=Date.now()+ms;
  for(;;){
    if(await detectCaptcha(child))throw new Error("BLOCKED: captcha");
    for(const scope of [child,...child.frames().filter(f=>f!==child.mainFrame())])for(const c of cands){
      const loc=scope.locator(c).first();
      if(await loc.isVisible().catch(()=>false)){selectors[label]=c;return loc;}
    }
    if(Date.now()>end){if(optional)return null;throw new Error(`hosted page element not found: ${label}`);}
    await pause(300);
  }
}
const CARD={ok:"4242424242424242",decline:"4000000000000002",threeDS:"4000002760003184"};
async function fillHostedCard(child,number){
  step("hosted page: fill card");
  await child.waitForLoadState("domcontentloaded",{timeout:60000});
  const em=await pick(child,"email",["#email","input[name=email]","input[type=email]"],8000,true);if(em)await em.fill(email);
  const num=await pick(child,"card_number",["#cardNumber","input[name=cardNumber]","input[autocomplete=cc-number]","input[name=cardnumber]"],60000);
  await num.fill(number);
  await (await pick(child,"card_expiry",["#cardExpiry","input[name=cardExpiry]","input[autocomplete=cc-exp]"],15000)).fill("12/34");
  await (await pick(child,"card_cvc",["#cardCvc","input[name=cardCvc]","input[autocomplete=cc-csc]"],15000)).fill("123");
  const name=await pick(child,"billing_name",["#billingName","input[name=billingName]","input[autocomplete=cc-name]"],5000,true);if(name)await name.fill("Synthetic Gate Buyer");
  const zip=await pick(child,"postal_code",["#billingPostalCode","input[name=billingPostalCode]","input[autocomplete=postal-code]"],2500,true);if(zip)await zip.fill("10001");
  const save=await pick(child,"save_info_checkbox",["#enableStripePass","input[name=enableStripePass]"],1500,true);
  if(save&&await save.isChecked().catch(()=>false))await save.uncheck().catch(()=>{}); // never save the synthetic buyer
  const submit=await pick(child,"submit",['[data-testid="hosted-payment-submit-button"]',"button.SubmitButton","button[type=submit]"],15000);
  step("hosted page: submit");await submit.click({timeout:20000,noWaitAfter:true});step("hosted page: submitted");
}
async function diag(child){ // failure snapshot: origin, frame hosts, visible text head (all redacted by say())
  const hosts=[...new Set(child.frames().map(f=>{try{return new URL(f.url()).host;}catch{return "?";}}))];
  const text=(await within(child.locator("body").innerText({timeout:2000}),4000,"")).replace(/\s+/g," ").slice(0,500);
  const html=(await within(child.content(),4000,"")).replace(/\s+/g," ").slice(0,300);say("DIAG html head:",html);
  say("DIAG readyState:",await within(child.evaluate(()=>document.readyState+" len="+document.documentElement.outerHTML.length),3000,"evaluate timed out"));
  await within(child.screenshot({path:path.join(evidence,"diag-child.png"),timeout:5000}),7000,null);
  say("DIAG page host:",new URL(child.url()).host,"frames:",hosts.join(","),"text:",text);
}
async function returnedNeutral(child){
  let lastLog=0;
  try{await until(async()=>{
    const u=new URL(child.url());if(Date.now()-lastLog>15000){lastLog=Date.now();step("waiting for return; page host:",u.host,"path:",u.pathname==="/payment/return"?u.pathname:"(other)");}
    if(u.origin===origin&&u.pathname==="/payment/return")return true;
    if(await detectCaptcha(child))throw new Error("BLOCKED: captcha");return false;},120000,"return to /payment/return",500);}catch(e){await diag(child);throw e;}
  assert.equal(new URL(child.url()).search,"","neutral return has no query");
  // Observed 2026-09-29 (Playwright 1243 Chromium): after Stripe's cross-site navigation to the return page the
  // page renders (screenshot works) but frame evaluation hangs. The neutral page is stateless, so one reload
  // restores the execution context; the workaround is recorded in result.json.
  if(await within(child.evaluate(()=>1),3000,0)!==1){step("return page evaluate wedged; reloading the neutral page");selectors.return_page_workaround="reload after cross-site navigation";await child.reload({waitUntil:"domcontentloaded",timeout:20000});}
  await child.locator('[data-testid="payment-return"]').waitFor({timeout:15000}).catch(async e=>{await diag(child);throw e;});
  const body=await child.locator('[data-testid="payment-return"]').innerText();
  assert(/does not confirm payment/i.test(body),"neutral return copy");assert(!/paid|captured|succeeded/i.test(body.replace(/does not confirm payment/i,"")),"return page must not claim payment");
  assert.equal(await child.evaluate(()=>window.opener),null);
  passed("neutral return /payment/return (no query, no payment claim, opener=null)");
}
async function completeThreeDS(child){
  let clicks=0,lastLog=0;
  await until(async()=>{
    if(await detectCaptcha(child))throw new Error("BLOCKED: captcha");
    if(new URL(child.url()).origin===origin)return true; // already returned
    const seen=[];
    for(const f of child.frames()){if(f===child.mainFrame())continue;
      const buttons=f.getByRole("button");const n=await within(buttons.count(),2000,0);
      let host="?";try{host=new URL(f.url()).host;}catch{}
      for(let i=0;i<n&&i<6;i++){const b=buttons.nth(i);const name=(await within(b.innerText({timeout:1000}),1500,"")).trim().slice(0,40);
        if(!name||!await within(b.isVisible(),1500,false))continue;seen.push(`${host}:${name}`);
        if(/^(?:Complete|Authorize)/i.test(name)&&clicks<3&&Date.now()-lastLog>4000){clicks++;lastLog=Date.now();selectors["3ds_complete_button"]=`frame(${host}) role=button name=/^(Complete|Authorize)/i`;step("3DS: clicking",name,"in",host);await within(b.click({timeout:5000}),7000,null);}
      }}
    if(Date.now()-lastLog>10000&&seen.length){lastLog=Date.now();step("3DS frames buttons:",seen.join(" | "));}
    return false;
  },120000,"3DS completion",700);
}
// ---- scenario drivers ----
async function sp18(){
  const c=await context(cfg.mobile),page=await c.newPage();
  await page.goto(product);await scanSelfTest(page,[c]);
  const l=cfg.locale,vp=cfg.mobile?"mobile":"desktop";
  const id=await newOrder(page);
  const base=await control(id);
  await setLocale(page,l);await expectStatusText(page,l,"NOT_STARTED","DRAFT");
  await shot(page.getByTestId("order-payment"),`sp18-${cfg.scenario}-${vp}-${l}-1-ready`,"SANDBOX");
  let child;
  try{
    const pop=page.waitForEvent("popup");await payBtn(page,l).click();child=await pop;
    await child.waitForURL(u=>u.origin==="https://checkout.stripe.com",{timeout:90000,waitUntil:"commit"});
    assert.equal(await child.evaluate(()=>window.opener),null);
    assert.equal(paymentCalls(id,"prepare").length,1);assert.equal(paymentCalls(id,"handoff").length,1);
    await ex(recoverBtn(page,l)).toBeVisible();await ex(cancelBtn(page,l)).toBeVisible();await ex(page.getByText(LOC[l].submitted,{exact:true})).toBeVisible();
    await ex(payBtn(page,l)).toHaveCount(0);
    assert(!page.url().includes("stripe"),"store tab URL never carries Stripe");
    await shot(page.getByTestId("order-payment"),`sp18-${cfg.scenario}-${vp}-${l}-2-continue`,"SANDBOX");
    passed(`SP18 ${cfg.scenario} ${vp} ${l}: Pay opened the hosted page in a child tab; store tab shows Continue + Cancel`);
    if(cfg.card==="ok"||cfg.card==="4242"){
      await fillHostedCard(child,CARD.ok);await returnedNeutral(child);await child.close();await page.bringToFront();
      await refreshUntil(page,["CAPTURED"],90000,"4242");
      await expectStatusText(page,l,"CAPTURED","CONFIRMED");
      await shot(page.getByTestId("order-payment"),`sp18-${cfg.scenario}-${vp}-${l}-3-captured`,"SANDBOX");
      const f=await control(id);assert.equal(f.order.captured,1);assert.equal(f.order.reservation,"COMMITTED");assert.equal(f.allocated,base.allocated+2);assert.equal(f.reserved,base.reserved-2);
      passed(`SP18 ${cfg.scenario} 4242: CAPTURED + CONFIRMED, reservation COMMITTED, stock allocated`);
    }else if(cfg.card==="3ds"){
      await fillHostedCard(child,CARD.threeDS);await completeThreeDS(child);await returnedNeutral(child);await child.close();await page.bringToFront();
      await refreshUntil(page,["CAPTURED"],90000,"3ds");
      await expectStatusText(page,l,"CAPTURED","CONFIRMED");
      await shot(page.getByTestId("order-payment"),`sp18-${cfg.scenario}-${vp}-${l}-3-captured`,"SANDBOX");
      const f=await control(id);assert.equal(f.order.captured,1);assert.equal(f.order.reservation,"COMMITTED");
      passed(`SP18 ${cfg.scenario} 3DS: authenticated, CAPTURED + CONFIRMED`);
    }else{ // decline -> stays open -> buyer cancel
      await fillHostedCard(child,CARD.decline);
      const msg=await pick(child,"decline_message",["#cardNumber-errors",".FieldError",'[role="alert"]','[data-testid*="error" i]'],45000);
      assert(/declin|拒绝|拒絕|不被接受|不被接受/i.test(await msg.innerText()),"Stripe shows the decline");
      assert.equal(new URL(child.url()).origin,"https://checkout.stripe.com","still on the hosted page");
      await ex(recoverBtn(page,l)).toBeVisible();await ex(cancelBtn(page,l)).toBeVisible();
      passed("SP18 B decline: shown on Stripe; store tab still Continue + Cancel");
      await cancelViaUI(page,l);
      await ex(page.getByText(LOC[l].cancelling,{exact:true})).toBeVisible({timeout:15000});
      await shot(page.getByTestId("order-payment"),`sp18-${cfg.scenario}-${vp}-${l}-3-cancelling`,"SANDBOX");
      await ex(cancelBtn(page,l)).toHaveCount(0);await ex(recoverBtn(page,l)).toHaveCount(0);
      passed("SP18 B cancel: native confirm text exact, `cancelling` shown, actions gone");
      await refreshUntil(page,["CLOSED_UNPAID"],90000,"decline cancel");
      await expectStatusText(page,l,"CLOSED_UNPAID","CANCELLED");
      await shot(page.getByTestId("order-payment"),`sp18-${cfg.scenario}-${vp}-${l}-4-closed`,"SANDBOX");
      const f=await control(id);assert.equal(f.order.closed_unpaid,1);assert.equal(f.order.reservation,"RELEASED");assert.equal(f.reserved,base.reserved-2);
      assert.equal(paymentCalls(id,"cancel").length,1);
      passed("SP18 B: CLOSED_UNPAID + CANCELLED, stock restored exactly");
    }
  }catch(error){ // primary drain: the worker's cancel path, through the UI
    if(!page.isClosed()&&await cancelBtn(page,l).isVisible().catch(()=>false))await cancelViaUI(page,l).catch(()=>{});
    throw error;
  }
  await closeAll([child].filter(Boolean));
  return finalScan([page],[c]);
}
async function su07(){
  const c=await context(false),page=await c.newPage();
  await page.goto(product);await scanSelfTest(page,[c]);
  const l="en";
  // 1. popup blocked: zero BFF calls, marker absent, alert shown, no facts changed
  let id=await newOrder(page);const before=await control(id),callsBefore=calls.length;
  await page.evaluate(()=>{window.__nativeOpen=window.open;window.open=()=>null;});
  await payBtn(page,l).click();
  await ex(page.getByRole("alert").filter({hasText:LOC.en.blocked})).toBeVisible();
  assert.equal(calls.slice(callsBefore).filter(x=>x.method==="POST").length,0,"blocked popup: zero BFF POSTs");
  assert.deepEqual(await control(id),before);
  assert.equal(await page.evaluate(id=>Object.keys(localStorage).some(k=>k.endsWith(`:${id}`)&&k.startsWith("commerce-order-payment-v1:")),id),false);
  await page.evaluate(()=>{window.open=window.__nativeOpen;});
  passed("SU07 popup blocked => `blocked`, zero BFF calls, no marker, no facts");
  // 2. child closed => Continue reuses the same session
  let pop=page.waitForEvent("popup");await payBtn(page,l).click();let child=await pop;
  await until(()=>stripeVisits>=1,60000,"first hosted navigation");
  await ex(recoverBtn(page,l)).toBeVisible();await child.close();
  assert.equal(paymentCalls(id,"prepare").length,1);
  const visits=stripeVisits;pop=page.waitForEvent("popup");await recoverBtn(page,l).click();child=await pop;
  await until(()=>stripeVisits>visits,60000,"Continue hosted navigation");await child.close();
  let f=await control(id);assert.equal(f.order.sessions,1);assert.equal(f.order.attempts,1);
  assert.equal(paymentCalls(id,"prepare").length,1,"Continue does not prepare again");assert.equal(paymentCalls(id,"handoff").length,2);
  passed("SU07 child closed => Continue reuses the same session (1 session row, 1 prepare, 2 handoffs)");
  // 3. double click + second tab => 1 prepare, 1 session
  id=await newOrder(page);
  const other=await c.newPage();await other.goto(product);await ex(other.getByTestId("order-id")).toHaveText(id);await ex(payBtn(other,l)).toBeVisible();
  const popups=[];c.on("page",p=>popups.push(p));
  await Promise.all([payBtn(page,l).dblclick(),payBtn(other,l).click()]);
  await until(async()=>(await control(id)).order.pinned===1&&stripeVisits>=3,60000,"pinned session + hosted navigation");
  await pause(1500);
  f=await control(id);assert.equal(paymentCalls(id,"prepare").length,1);assert.equal(f.order.sessions,1);assert.equal(f.order.attempts,1);
  await closeAll(popups.filter(p=>p!==other&&p!==page));await other.close();
  passed("SU07 double click + second tab => 1 prepare, 1 session");
  // 4. reload while CREATING => no POST
  await act("worker-stop");
  id=await newOrder(page);
  pop=page.waitForEvent("popup");await payBtn(page,l).click();child=await pop;
  await until(()=>paymentCalls(id,"prepare").length===1,30000,"prepare while the worker is stopped");
  await pause(1500);const posts=calls.filter(x=>x.method==="POST").length;
  await page.reload();await ex(page.getByTestId("order-payment")).toBeVisible();await ex(recoverBtn(page,l)).toBeVisible();await ex(cancelBtn(page,l)).toBeVisible();
  await pause(2500);
  assert.equal(calls.filter(x=>x.method==="POST").length,posts,"reload during CREATING sent no POST");
  f=await control(id);assert.equal(f.order.sessions,1);assert.equal(f.order.pinned,0,"still CREATING at the provider");
  await act("worker-start");await until(async()=>(await control(id)).order.pinned===1,60000,"worker pins the session");
  const h0=paymentCalls(id,"handoff").length;pop=page.waitForEvent("popup");await recoverBtn(page,l).click();const child2=await pop;
  await until(()=>paymentCalls(id,"handoff").length===h0+1,30000,"one explicit handoff POST");
  await closeAll([child,child2].filter(p=>!p.isClosed()));
  passed("SU07 reload while CREATING => no POST; the next explicit Continue is exactly one handoff POST");
  // 5. neutral return GET/POST changes nothing
  const snap=await control(id);
  const get=await relay(nextPort,{url:"/payment/return?x=untrusted",method:"GET",headers:{host:"buyer.example"}},Buffer.alloc(0));
  const post=await relay(nextPort,{url:"/payment/return",method:"POST",headers:{host:"buyer.example","content-type":"application/x-www-form-urlencoded"}},Buffer.from("payment=success"));
  assert.equal(get.status,200);assert.equal(post.status,200);assert.equal(get.body.toString(),post.body.toString());
  assert.deepEqual(await control(id),snap);
  passed("SU07 GET/POST /payment/return leaves every fact unchanged");
  return finalScan([page],[c]);
}
async function su09(){
  const c=await context(cfg.mobile),page=await c.newPage(),vp=cfg.mobile?"mobile":"desktop";
  await page.goto(product);await scanSelfTest(page,[c]);
  const three=async(row)=>{for(const loc of ["zh-CN","zh-TW","en"]){await setLocale(page,loc);await shot(page.getByTestId("order-payment"),`su09-${vp}-${row}-${loc}`,"MOCK");}await setLocale(page,"en");};
  const liveLen=()=>page.evaluate(()=>window.__live.length);
  const noDupes=async(from)=>{const live=(await page.evaluate(i=>window.__live.slice(i),from));for(let i=1;i<live.length;i++)assert(!(live[i].text===live[i-1].text&&live[i].role===live[i-1].role&&live[i].t-live[i-1].t<1500),`announcement repeated: ${live[i].role} ${JSON.stringify(live.map(x=>[x.role,x.text.slice(0,30)]))}`);return live.length;};
  // roles and names, three locales
  let id=await newOrder(page);
  for(const loc of ["zh-CN","zh-TW","en"]){await setLocale(page,loc);await ex(payBtn(page,loc)).toBeVisible();}
  await ex(page.getByRole("button",{name:LOC.en.refresh,exact:true})).toBeVisible().catch(()=>{});
  await setLocale(page,"en");
  await three("01-fresh-pay");
  // O1: keyboard-only Pay -> Continue -> Cancel
  let mark=await liveLen();
  await keyboardTo(page,LOC.en.pay);let pop=page.waitForEvent("popup");await page.keyboard.press("Enter");let child=await pop;
  await until(()=>stripeVisits>=1,60000,"hosted navigation (keyboard Pay)");
  await ex(recoverBtn(page,"en")).toBeVisible();await ex(cancelBtn(page,"en")).toBeVisible();await ex(page.getByText(LOC.en.submitted,{exact:true})).toBeVisible();
  await noDupes(mark);await three("02-continue-ready-submitted");
  const stable=await liveLen();await pause(12000); // covers the 5 s and 10 s background polls
  assert.equal(await liveLen(),stable,"same-state polls render nothing");
  await child.close();await keyboardTo(page,LOC.en.recover);pop=page.waitForEvent("popup");await page.keyboard.press("Enter");child=await pop;
  await until(()=>stripeVisits>=2,60000,"hosted navigation (keyboard Continue)");await child.close();
  mark=await liveLen();await cancelViaUI(page,"en",{keyboard:true});
  // §8: focus moves to the status line at the moment Cancel/Continue disappear (`cancelling`). Assert it
  // before refreshUntil, whose own Refresh click legitimately moves focus onto the Refresh button.
  await ex(page.getByTestId("payment-status")).toBeFocused({timeout:15000});
  await refreshUntil(page,["CLOSED_UNPAID"],90000,"keyboard cancel");
  await expectStatusText(page,"en","CLOSED_UNPAID","CANCELLED");await noDupes(mark);await three("03-closed-unpaid-cancelled");
  let f=await control(id);assert.equal(f.order.closed_unpaid,1);assert.equal(f.order.reservation,"RELEASED");
  passed(`SU09 ${vp}: keyboard-only Pay/Continue/Cancel, roles+names in 3 locales, one announcement per change, focus to status`);
  // O2: captured + confirmed
  id=await newOrder(page);pop=page.waitForEvent("popup");await payBtn(page,"en").click();child=await pop;
  await until(()=>stripeVisits>=3,60000,"hosted navigation (O2)");await act("pay",id);mark=await liveLen();
  await refreshUntil(page,["CAPTURED"],90000,"mock capture");await expectStatusText(page,"en","CAPTURED","CONFIRMED");await noDupes(mark);
  await ex(page.getByTestId("order-payment")).toHaveAttribute("aria-busy","false");await three("04-captured-confirmed");await child.close();
  // O3: CREATING budget, CREATING view, cancelling
  await act("worker-stop");id=await newOrder(page);pop=page.waitForEvent("popup");await payBtn(page,"en").click();child=await pop;
  await ex(page.getByText(LOC.en.creating,{exact:true})).toBeVisible({timeout:60000});await until(()=>child.isClosed(),15000,"blank child closed after the CREATING budget");
  await three("05-creating-budget");await page.reload();await ex(recoverBtn(page,"en")).toBeVisible();await ex(cancelBtn(page,"en")).toBeVisible();await three("06-creating-view");
  await cancelViaUI(page,"en",{keyboard:true});await ex(page.getByText(LOC.en.cancelling,{exact:true})).toBeVisible();
  assert.equal((await control(id)).order.cancel_requested,true);await three("07-cancelling");
  await act("worker-start");await refreshUntil(page,["CLOSED_UNPAID"],90000,"unsent closure after worker start");
  // O6: cutoff (UNAVAILABLE and client clock past handoff_expires_at)
  id=await newOrder(page);pop=page.waitForEvent("popup");await payBtn(page,"en").click();child=await pop;
  await until(()=>stripeVisits>=4,60000,"hosted navigation (O6)");await child.close();await act("age-cutoff",id);await page.reload();
  await ex(page.getByText(LOC.en.cutoff,{exact:true})).toBeVisible();await ex(cancelBtn(page,"en")).toBeVisible();await ex(recoverBtn(page,"en")).toHaveCount(0);await three("08-cutoff");
  // O7: REVIEW_REQUIRED (SP12 presentment drift)
  id=await newOrder(page);pop=page.waitForEvent("popup");await payBtn(page,"en").click();child=await pop;
  await until(()=>stripeVisits>=5,60000,"hosted navigation (O7)");await act("review",id);await child.close();
  await refreshUntil(page,["REVIEW_REQUIRED"],90000,"review");await expectStatusText(page,"en","REVIEW_REQUIRED");
  await ex(recoverBtn(page,"en")).toHaveCount(0);await ex(cancelBtn(page,"en")).toHaveCount(0);await three("09-review-required");
  // O5: the handoff POST answers UNAVAILABLE (qualification revoked by the operator) => `failed`, never `uncertain`.
  // Go finding (OBS_mock_unavailable, 2026-09-29): a revoked qualification / disabled binding / disabled method changes
  // neither the pinned view's handoff_state (stays READY) nor persists UNAVAILABLE; the view turns UNAVAILABLE only at the
  // time cutoff. So the contract's `readOnly` row (UNAVAILABLE and now < handoff_expires_at) has no producer: recorded.
  id=await newOrder(page);
  pop=page.waitForEvent("popup");await payBtn(page,"en").click();child=await pop;
  await until(()=>stripeVisits>=6,60000,"hosted navigation (O5)");await child.close();
  await act("qualification-revoke");
  pop=page.waitForEvent("popup");await recoverBtn(page,"en").click();const child5=await pop;
  await ex(page.getByText(LOC.en.failed,{exact:true})).toBeVisible();await until(()=>child5.isClosed(),15000,"blank child closed on UNAVAILABLE");
  await ex(page.getByText(/already issued will not be issued again/)).toHaveCount(0); // PAYUNi-only `uncertain` copy never shows for Stripe
  await three("10-handoff-unavailable-failed");
  await page.reload();
  if(await page.getByText(LOC.en.readOnly,{exact:true}).isVisible().catch(()=>false)){await three("10b-unavailable-readonly");}
  else unreached.push({row:"UNAVAILABLE before cutoff (readOnly)",reason:"Go keeps handoff_state READY after a revoked qualification; UNAVAILABLE appears only at the time cutoff (cutoff row covered)"});
  // methods != 1 => `unavailable` (the revoked qualification also removes the method from new orders' views)
  id=await newOrder(page,{pay:false});await ex(page.getByText(LOC.en.unavailable,{exact:true})).toBeVisible();await ex(payBtn(page,"en")).toHaveCount(0);await three("11-unavailable-no-method");
  passed(`SU09 ${vp}: every §5 row screenshotted in 3 locales (MOCK: REVIEW_REQUIRED, UNAVAILABLE, cutoff, CREATING budget, CLOSED_UNPAID)`);
  return finalScan([page],[c]);
}
const attachStep=p=>{
  p.on("framenavigated",f=>{if(f===p.mainFrame()){try{step("nav ->",new URL(f.url()).host||"about:blank");}catch{}}});
  p.on("response",r=>{try{const u=new URL(r.url());if(u.origin===origin&&r.request().isNavigationRequest())step("document response",r.status(),r.request().method(),u.pathname==="/payment/return"?u.pathname:"(other)",r.headers()["content-type"]??"-","len",r.headers()["content-length"]??"-");}catch{}});
  p.on("requestfailed",r=>{try{const u=new URL(r.url());if(u.origin===origin)step("request failed",r.method(),u.pathname==="/payment/return"?u.pathname:"(other)",r.failure()?.errorText);}catch{}});
};
async function obs(){ // developer-only: the real hosted page without the B2 buyer UI
  const c=await context(false);
  const {url}=await (await fetch(`${CONTROL}/observe`,{headers:CH})).json();
  const child=await c.newPage();step("obs: open hosted page");await child.goto(url,{waitUntil:"domcontentloaded",timeout:90000});step("obs: hosted page loaded");
  if(cfg.card==="4242"){await fillHostedCard(child,CARD.ok);await returnedNeutral(child);}
  else if(cfg.card==="3ds"){await fillHostedCard(child,CARD.threeDS);await completeThreeDS(child);await returnedNeutral(child);}
  else{await fillHostedCard(child,CARD.decline);const m=await pick(child,"decline_message",["#cardNumber-errors",".FieldError",'[role="alert"]','[data-testid*="error" i]'],45000);say("observed decline text length",(await m.innerText()).length);}
  return {clean:true,found:{},surfaces:["observe"]};
}
let scan={clean:false,found:{},surfaces:[]},pass=false,failure="";
async function snapshotAll(){
  for(const c of contexts)for(const p of c.pages()){
    if(p.isClosed())continue;
    try{const hosts=[...new Set(p.frames().map(f=>{try{return new URL(f.url()).host;}catch{return "?";}}))];
      const text=(await within(p.locator("body").innerText({timeout:2000}),4000,"")).replace(/\s+/g," ").slice(0,300);
      say("SNAPSHOT page host:",new URL(p.url()).host||"about:blank","frames:",hosts.join(","),"text:",text);}catch{}
  }
}
const watchdog=setTimeout(async()=>{ // fail loudly with a snapshot instead of being killed silently
  say(`WATCHDOG: no completion after ${cfg.budgetMs}ms`);await snapshotAll();
  await writeFile(path.join(evidence,"result.json"),JSON.stringify({kind:cfg.kind,mode:cfg.mode,pass:false,cases,orders,failure:"watchdog",refused_hosts:[...refused].sort(),selectors,screenshots:shots}),{mode:0o600}).catch(()=>{});
  process.exit(4);
},cfg.budgetMs??240000);
try{
  execFileSync("openssl",["req","-x509","-newkey","rsa:2048","-nodes","-keyout",path.join(certDir,"key.pem"),"-out",path.join(certDir,"cert.pem"),"-days","1","-subj","/CN=buyer.example"],{stdio:"ignore"});
  nextPort=await startNext();const proxyPort=await startEdge();
  browser=await launch({headless:true,proxy:{server:`http://127.0.0.1:${proxyPort}`}});
  scan=await ({SP18:sp18,SU07:su07,SU09:su09,OBS:obs}[cfg.kind])();pass=true;
}catch(error){
  failure=redact(error?.stack??error);say(`FAIL ${failure.split("\n").slice(0,12).join("\n")}`);
  process.exitCode=String(error?.message).startsWith("BLOCKED: captcha")?3:1;
}finally{
  clearTimeout(watchdog);
  await writeFile(path.join(evidence,"result.json"),JSON.stringify({kind:cfg.kind,mode:cfg.mode,case:process.env.LC_STRIPE_CASE?`${cfg.kind}-${cfg.scenario??""}-${cfg.mobile?"mobile":"desktop"}-${cfg.locale}`:"",pass,orders,cases,scan,
    screenshots:shots,unreached_rows:unreached,selectors,refused_hosts:[...refused].sort(),viewport:cfg.mobile?`390x844 ${phoneName}`:"1440x900",locale:cfg.locale,card:cfg.card??null,failure:pass?"":failure.slice(0,600)}),{flag:"w",mode:0o600});
  for(const ctx of contexts)await ctx.close().catch(()=>{});
  await browser?.close().catch(()=>{});
  for(const s of sockets)s.destroy();
  for(const server of [proxy,edge])if(server)await new Promise(r=>server.close(r));
  for(const child of children)child.kill("SIGKILL");
  for(const log of logs)log.end();
  await rm(certDir,{recursive:true,force:true});
}
