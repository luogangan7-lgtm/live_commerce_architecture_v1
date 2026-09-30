// Actual buyer form -> production Next -> Go -> isolated PostgreSQL.
// Only synthetic TLS/domain routing and causal network/storage faults live here.
import assert from "node:assert/strict";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import {spawn, execFileSync} from "node:child_process";
import {once} from "node:events";
import {readFile, writeFile, mkdtemp, rm, mkdir, copyFile} from "node:fs/promises";
import {createWriteStream} from "node:fs";
import {tmpdir} from "node:os";
import path from "node:path";
import { expect } from "@playwright/test";
import { launch, ctxOpts } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const root=process.cwd(), evidence=process.env.LC_ORDER_EVIDENCE;
assert(evidence && /^http:\/\/127\.0\.0\.1:\d+$/.test(process.env.LC_ORDER_CONTROL));
const origin="https://buyer.example", productPath=`/en/products/${process.env.LC_ORDER_PRODUCT}`;
const children=new Set(), sockets=new Set(), logs=[], contexts=[], orders=[], observations=[], storageWrites=[], consoleText=[], requestURLs=[];
const pii={recipient_name:"Synthetic Gate Recipient",phone:"+886900000091",region:"Synthetic Region",city:"Synthetic City",postal_code:"99991",line1:"Synthetic Address Ninety One",line2:"Synthetic Unit Ninety Two"};
const secrets=[], pageErrors=[];
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const pause=ms=>new Promise(r=>setTimeout(r,ms));
const listen=async s=>{s.listen(0,"127.0.0.1");await once(s,"listening");return s.address().port;};
const pass=name=>{observations.push(name);console.log(`PASS ${name}`);};
const certDir=await mkdtemp(path.join(tmpdir(),"lc-order-edge-"));
const review=path.join(root,"output/playwright/review/buyer-order");
const historyReview=path.join(root,"output/playwright/review/buyer-history");
let browser,edge,proxy,hook,sessionResets=0;
const calls=[];
async function control(resource,method="GET") {
  const response=await fetch(`${process.env.LC_ORDER_CONTROL}/${resource}`,{method,headers:{"X-Gate-Key":process.env.LC_ORDER_CONTROL_KEY}});
  assert.equal(response.status,200,`fixture control ${resource.split("/")[0]}`);return response.json();
}
function relay(port,req,body) {
  return new Promise((resolve,reject)=>{
    const headers={...req.headers};delete headers.connection;delete headers["transfer-encoding"];
    if(body.length)headers["content-length"]=String(body.length);else delete headers["content-length"];
    const call=http.request({hostname:"127.0.0.1",port,path:req.url,method:req.method,headers},res=>{
      const chunks=[];res.on("data",x=>chunks.push(x));res.on("error",reject);res.on("end",()=>resolve({status:res.statusCode,headers:res.headers,body:Buffer.concat(chunks)}));
    });call.setTimeout(15000,()=>call.destroy(new Error("fixture relay deadline")));call.on("error",reject);call.end(body);
  });
}
async function startNext() {
  const reserve=net.createServer(),port=await listen(reserve);await new Promise(r=>reserve.close(r));
  const log=createWriteStream(path.join(evidence,"next.log"),{flags:"wx",mode:0o600});logs.push(log);await once(log,"open");
  const env={...process.env,NODE_ENV:"production",NEXT_TELEMETRY_DISABLED:"1"};
  for(const name of Object.keys(env))if(name.startsWith("LC_ORDER_"))delete env[name];
  const child=spawn(process.execPath,[path.join(root,"apps/storefront/node_modules/next/dist/bin/next"),"start","--hostname","127.0.0.1","--port",String(port)],{cwd:path.join(root,"apps/storefront"),env,stdio:["ignore",log,log]});children.add(child);
  for(let i=0;i<100;i++){
    if(child.exitCode!==null)throw new Error("owned Next failed readiness");
    try{if((await relay(port,{url:"/api/buyer/session",method:"GET",headers:{host:"buyer.example"}},Buffer.alloc(0))).status===200)return port;}catch{}
    await pause(50);
  }throw new Error("owned Next readiness timeout");
}
function arm(suffix,fields={}) {
  return hook={path:`/api/buyer/${suffix}`,entered:deferred(),release:deferred(),result:deferred(),...fields};
}
async function newContext(mobile=false) {
  const c=await browser.newContext(ctxOpts({ignoreHTTPSErrors:true,viewport:mobile?{width:390,height:844}:{width:1440,height:900}}));contexts.push(c);
  await c.exposeBinding("__gateStorageWrite",(_,value)=>storageWrites.push(value));
  await c.addInitScript(()=>{
    const native=Storage.prototype.setItem;
    const remove=Storage.prototype.removeItem;
    Storage.prototype.removeItem=function(key){
      if(window.__keepQuoteLocator && this===sessionStorage && String(key).startsWith("commerce-purchase-quote-v1:"))return;
      return remove.call(this,key);
    };
    Storage.prototype.setItem=function(key,value){
      void window.__gateStorageWrite({kind:this===localStorage?"local":"session",key:String(key),value:String(value)});
      if(window.__failOrderLocator && String(key).startsWith("commerce-purchase-order-v1:"))throw new DOMException("Synthetic gate storage failure","QuotaExceededError");
      return native.call(this,key,value);
    };
  });
  c.on("page",p=>{
    p.on("pageerror",e=>pageErrors.push(e.name));
    p.on("console",m=>consoleText.push(m.text()));
    p.on("request",r=>requestURLs.push(r.url()));
  });return c;
}
const requestIs=(response,suffix,method)=>new URL(response.url()).pathname===`/api/buyer/${suffix}`&&response.request().method()===method;
async function quotePage(c,clock=false,p) {
  if(!p){p=await c.newPage();if(clock)await p.clock.install();await p.goto(origin+productPath);}
  await p.getByRole("button",{name:"Choose delivery",exact:true}).click();
  const pending=p.waitForResponse(r=>requestIs(r,"quotes","POST"));
  await p.getByRole("button",{name:"Get current total",exact:true}).click();
  const response=await pending;assert.equal(response.status(),200);
  const quote=await response.json();
  await expect(p.getByTestId("address-section")).toBeVisible();
  return {p,quote};
}
async function fill(p,values=pii) {for(const [key,value] of Object.entries(values))await p.locator(`input[name="${key}"]`).fill(value);}
async function confirm(p) {
  await p.getByTestId("confirm-address").click();
  await expect(p.getByTestId("create-order")).toBeEnabled();
  await expect(p.getByRole("button",{name:"View quotation",exact:true})).not.toHaveClass(/\bprimary\b/);
}
async function created(p,quote,ownerOrders=1) {
  await expect(p.getByTestId("order-section")).toBeVisible();
  await expect(p.getByTestId("order-state")).toHaveAttribute("data-state","DRAFT");
  await expect(p.getByTestId("order-state")).toHaveText(/Not paid/i);
  const id=(await p.getByTestId("order-id").innerText()).trim();assert.match(id,/^[a-f0-9-]{36}$/);
  const f=(await control("facts")).find(x=>x.id===id);assert(f);
  for(const key of ["orders","holds","jobs","receipts","reserve_lines"])assert.equal(f[key],ownerOrders,`per-buyer ${key}`);
  assert.equal(f.hold_state,"HELD");
  assert.equal(f.total,quote.amount.total_minor);assert.equal(f.currency,quote.currency);assert.equal(f.country,quote.country);
  const breakdown=p.getByTestId("order-breakdown").locator("div");
  await expect(breakdown).toHaveCount(3);
  for(const [index,key] of ["shipping_minor","tax_minor","discount_minor"].entries()) {
    await expect(breakdown.nth(index).locator("dd")).toHaveText(new Intl.NumberFormat("en",{style:"currency",currency:quote.currency}).format(quote.amount[key]/100));
  }
  await expect(p.getByTestId("create-order")).toHaveCount(0);
  // This legacy fixture does not enable buyer payment. New payment UI must not
  // manufacture an available method merely because an order was created.
  await expect(p.getByTestId("pay-order")).toHaveCount(0);
  if(!orders.includes(id))orders.push(id);return id;
}
async function stored(p,prefix="commerce-purchase-pending-v1:") {
  return p.evaluate(prefix=>{const key=Object.keys(localStorage).find(k=>k.startsWith(prefix));return key?JSON.parse(localStorage.getItem(key)):null;},prefix);
}
async function api(p,method,suffix,body,key) {
  return p.evaluate(async({method,suffix,body,key})=>{
    const session=await(await fetch("/api/buyer/session",{cache:"no-store"})).json();
    const response=await fetch(`/api/buyer/${suffix}`,{method,headers:{"Content-Type":"application/json","X-Buyer-Context":session.context,...(key?{"Idempotency-Key":key}:{})},...(body?{body:JSON.stringify(body)}:{})});
    return {status:response.status,body:await response.json()};
  },{method,suffix,body,key});
}
async function rememberCookie(c) {
  const cookie=(await c.cookies(origin))[0];assert(cookie?.httpOnly&&cookie.secure&&cookie.sameSite==="Lax");
  secrets.push(cookie.value);const payload=JSON.parse(Buffer.from(cookie.value.split(".")[0],"base64url").toString());if(payload.token)secrets.push(payload.token);
}
async function capture(p,name,fullPage=true,directory=review) {
  await mkdir(directory,{recursive:true});
  await p.screenshot({path:path.join(evidence,name),fullPage});await copyFile(path.join(evidence,name),path.join(directory,name));
}
try {
  execFileSync("openssl",["req","-x509","-newkey","rsa:2048","-nodes","-keyout",path.join(certDir,"key.pem"),"-out",path.join(certDir,"cert.pem"),"-days","1","-subj","/CN=buyer.example"],{stdio:"ignore"});
  const port=await startNext();
  edge=https.createServer({key:await readFile(path.join(certDir,"key.pem")),cert:await readFile(path.join(certDir,"cert.pem"))},async(req,res)=>{
    try {
      const chunks=[];for await(const x of req)chunks.push(x);const body=Buffer.concat(chunks);
      if(req.url.startsWith("/api/buyer/session/")&&req.url.includes("prepare")&&body.toString().includes('"reset"'))sessionResets++;
      const call={path:req.url,method:req.method,key:req.headers["idempotency-key"],body:body.length?body.toString():null};
      if(req.url.startsWith("/api/buyer/"))calls.push(call);
      const active=hook&&hook.path===req.url&&(!hook.method||hook.method===req.method)?hook:null;
      if(active){if(!active.repeat)hook=null;active.entered.resolve();if(active.before)await active.release.promise;}
      const out=await relay(port,req,body);call.status=out.status;
      if(active){active.out=out;active.result.resolve(out.status);}
      if(active?.drop){res.destroy();return;}
      if(active?.after)await active.release.promise;
      const headers={...out.headers};delete headers.connection;delete headers["transfer-encoding"];
      res.writeHead(out.status,headers);res.end(out.body);
    }catch{if(!res.headersSent)res.writeHead(502);res.end();}
  });
  const edgePort=await listen(edge);proxy=http.createServer((_,r)=>{r.writeHead(403);r.end();});
  proxy.on("connect",(req,socket,head)=>{
    if(req.url!=="buyer.example:443"){socket.destroy();return;}
    const upstream=net.connect(edgePort,"127.0.0.1",()=>{socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");if(head.length)upstream.write(head);socket.pipe(upstream).pipe(socket);});
    for(const s of [socket,upstream]){sockets.add(s);s.on("close",()=>sockets.delete(s));s.on("error",()=>{socket.destroy();upstream.destroy();});}
  });
  browser=await launch({headless:true,proxy:{server:`http://127.0.0.1:${await listen(proxy)}`}});

  // BO01/BO03: native form, all locales, in-memory PII and causal lost PUT.
  const c1=await newContext(),{p:a,quote:q1}=await quotePage(c1);await rememberCookie(c1);
  for(const name of Object.keys(pii))await expect(a.locator(`input[name="${name}"]`)).toHaveCount(1);
  await expect(a.locator('input[name="phone"]')).toHaveAttribute("type","tel");
  await fill(a);await a.evaluate(()=>window.__sameDocument="kept");
  await capture(a,"desktop-address.png");
  await a.getByTestId("address-section").scrollIntoViewIfNeeded();await capture(a,"desktop-address-viewport.png",false);
  for(const locale of ["zh-CN","zh-TW","en"]){
    await a.locator("header select").selectOption(locale);
    await expect(a).toHaveURL(`${origin}/${locale}/products/${process.env.LC_ORDER_PRODUCT}`);
    await expect(a.locator("html")).toHaveAttribute("lang",locale);
    assert.equal(await a.evaluate(()=>window.__sameDocument),"kept");
    for(const [name,value] of Object.entries(pii))await expect(a.locator(`input[name="${name}"]`)).toHaveValue(value);
  }
  await expect(a.getByTestId("create-order")).toBeDisabled();
  pass("BO01 native address and three history locales retain unsaved in-memory form");
  const destinationStart=calls.length,dropDest=arm("destination",{method:"PUT",drop:true,repeat:true});
  await a.getByTestId("confirm-address").click();assert.equal(await dropDest.result.promise,200);
  await expect(a.getByTestId("recover-purchase")).toBeVisible();hook=null;
  const destinationPending=await stored(a);assert.equal(destinationPending.kind,"destination");
  assert.deepEqual(Object.keys(destinationPending.body).sort(),["cart_version","country","expected_version","kind"]);
  await a.getByTestId("recover-purchase").click();await confirm(a);
  const attempts=calls.slice(destinationStart).filter(x=>x.path==="/api/buyer/destination"&&x.method==="PUT");
  assert(attempts.length>=2);assert.equal(new Set(attempts.map(x=>x.key)).size,1);assert.equal(new Set(attempts.map(x=>x.body)).size,1);
  pass("BO03 committed destination lost reply retries exact key and body");

  await a.locator('input[name="line2"]').fill("Synthetic Changed Unit");
  const dropReloadDest=arm("destination",{method:"PUT",drop:true,repeat:true});
  await a.getByTestId("confirm-address").click();assert.equal(await dropReloadDest.result.promise,200);
  await expect(a.getByTestId("recover-purchase")).toBeVisible();hook=null;
  const pendingReloadDestination=await stored(a),beforeReloadDestination=calls.filter(x=>x.path==="/api/buyer/destination"&&x.method==="PUT").length;
  await a.reload();await expect(a.getByTestId("address-section")).toBeVisible();
  await expect(a.locator('input[name="recipient_name"]')).toHaveValue(pii.recipient_name);
  await expect(a.getByTestId("create-order")).toBeDisabled();
  assert.equal(calls.filter(x=>x.path==="/api/buyer/destination"&&x.method==="PUT").length,beforeReloadDestination,"reload must not replay PII");
  await confirm(a);const replacedDestination=calls.filter(x=>x.path==="/api/buyer/destination"&&x.method==="PUT").at(-1);
  assert.notEqual(replacedDestination.key,pendingReloadDestination.key);assert(JSON.parse(replacedDestination.body).expected_version>pendingReloadDestination.body.expected_version);
  pass("BO03 lost destination reply reload reads head and explicitly replaces metadata intent");
  await a.locator('input[name="line2"]').fill(pii.line2);await expect(a.getByTestId("create-order")).toBeDisabled();await confirm(a);
  const sourceDest=(await api(a,"GET","destination")).body.destination;
  const external={expected_version:sourceDest.version,cart_version:sourceDest.cart_version,kind:"home",country:"TW",recipient_name:pii.recipient_name,phone:pii.phone,home_address:{...sourceDest.home_address,line1:"Synthetic Concurrent Address"}};
  const other=await c1.newPage();await other.goto(origin+productPath);
  assert.equal((await api(other,"PUT","destination",external,crypto.randomUUID())).status,200);
  const beforeConflict=(await control("facts")).length;
  const conflict=a.waitForResponse(r=>requestIs(r,"checkout","POST"));await a.getByTestId("create-order").click();assert.equal((await conflict).status(),409);await expect(a.locator(".purchase-error")).toContainText("changed");assert.equal((await control("facts")).length,beforeConflict);
  await a.reload();await expect(a.locator('input[name="line1"]')).toHaveValue(external.home_address.line1);await expect(a.getByTestId("create-order")).toBeDisabled();await confirm(a);
  assert.equal((await api(other,"PUT","destination",external,crypto.randomUUID())).status,409,"late lower-CAS address must fail");
  pass("BO03 reload reconfirmation, edits and concurrent/late address CAS fail closed");
  await a.getByTestId("create-order").click();const order1=await created(a,q1);
  await capture(a,"desktop-order.png");
  pass("BO04 actual confirmed form creates authoritative DRAFT and exactly one order/hold/job/receipt");

  // Historical receipt is insufficient: expire through the real worker then GET.
  await control(`expire/${order1}`,"POST");
  const getOrder=a.waitForResponse(r=>requestIs(r,`orders/${order1}`,"GET"));
  await a.getByRole("button",{name:"Refresh order",exact:true}).click();assert.equal((await getOrder).status(),200);
  await expect(a.getByTestId("order-state")).toHaveAttribute("data-state","CANCELLED");
  await expect(a.getByTestId("order-id")).toHaveText(order1);await expect(a.getByTestId("create-order")).toHaveCount(0);
  pass("BO05 owned current-state GET renders real expiry rather than historical DRAFT receipt");

  // BO02: the server rejects an expired quote even if the page still has it.
  const c2=await newContext(),{p:stale,quote:q2}=await quotePage(c2);await fill(stale);await confirm(stale);
  await control(`expire-quote/${q2.id}`,"POST");const beforeExpired=(await control("facts")).length;
  const denied=stale.waitForResponse(r=>requestIs(r,"checkout","POST"));await stale.getByTestId("create-order").click();assert.equal((await denied).status(),409);
  await expect(stale.locator(".purchase-error")).toContainText("changed");assert.equal((await control("facts")).length,beforeExpired);
  await expect(stale.getByTestId("order-section")).toHaveCount(0);
  pass("BO02 expired authoritative quote fails closed with no order or hold");

  // BO04: actual UI invalidates the stale second-tab confirmation. Never force
  // a disabled button enabled to manufacture a UI race the product prevents.
  const c3=await newContext(),{p:tab1,quote:q3}=await quotePage(c3);await fill(tab1);await confirm(tab1);
  const popup=tab1.waitForEvent("popup");await tab1.evaluate(url=>window.open(url,"_blank"),origin+productPath);const tab2=await popup;
  await expect(tab2.getByTestId("address-section")).toBeVisible();await confirm(tab2);
  // tab2's confirmation advanced the head; explicitly inspect and reconfirm in
  // tab1 so both tabs start with the same displayed current destination.
  await tab1.reload();await expect(tab1.getByTestId("address-section")).toBeVisible();await confirm(tab1);
  await expect(tab2.getByTestId("create-order")).toBeDisabled();
  const queuedStart=calls.length,held=arm("checkout",{method:"POST",after:true});
  await tab1.getByTestId("create-order").click();assert.equal(await held.result.promise,200);
  await expect(tab1.getByTestId("create-order")).toBeDisabled();
  await expect(tab2.getByTestId("create-order")).toHaveCount(0);
  await expect(tab2.getByTestId("recover-purchase")).toBeVisible();
  await tab2.getByTestId("recover-purchase").click();
  await expect.poll(()=>tab2.evaluate(async()=> (await navigator.locks.query()).pending.filter(x=>x.name==="commerce-purchase-write-v1").length)).toBeGreaterThan(0);
  held.release.resolve();const order3=await created(tab1,q3);assert.equal(await created(tab2,q3),order3);
  assert.equal(calls.slice(queuedStart).filter(x=>x.path==="/api/buyer/checkout"&&x.method==="POST").length,1);
  pass("BO04 actual second-tab recovery queues on Web Lock and resumes same order without second POST");

  // BO05: a committed checkout with no reply survives document death/reload.
  const c4=await newContext(true),{p:lost,quote:q4}=await quotePage(c4);await fill(lost);await confirm(lost);
  await capture(lost,"mobile-address.png");
  const lostStart=calls.length,dropCheckout=arm("checkout",{method:"POST",drop:true,repeat:true});
  await lost.getByTestId("create-order").click();assert.equal(await dropCheckout.result.promise,200);await expect(lost.getByTestId("recover-purchase")).toBeVisible();hook=null;
  const lostPending=await stored(lost);assert.equal(lostPending.kind,"checkout");
  await expect(lost.getByTestId("continue-shopping")).toHaveCount(0);
  assert.deepEqual(Object.keys(lostPending.body).sort(),["allocation_version","cart_version","destination_id","quote_id","service_version"]);
  await lost.reload();await expect(lost.getByTestId("recover-purchase")).toBeVisible();assert.deepEqual(await stored(lost),lostPending);
  await lost.getByTestId("recover-purchase").click();await created(lost,q4);
  const replays=calls.slice(lostStart).filter(x=>x.path==="/api/buyer/checkout"&&x.method==="POST");assert(replays.length>=2);assert.equal(new Set(replays.map(x=>x.key)).size,1);assert.equal(new Set(replays.map(x=>x.body)).size,1);
  assert.equal(await stored(lost),null);await capture(lost,"mobile-order.png");
  assert(await lost.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  pass("BO05 lost checkout reply reload replays exact five-field intent and original key on mobile");

  // Success storage failure keeps the original intent until locator readback.
  const c5=await newContext(),{p:fault,quote:q5}=await quotePage(c5);await fill(fault);await confirm(fault);
  await fault.evaluate(()=>window.__failOrderLocator=true);const storageStart=calls.length;
  await fault.getByTestId("create-order").click();await expect(fault.getByTestId("recover-purchase")).toBeVisible();
  const storagePending=await stored(fault);assert.equal(storagePending.kind,"checkout");assert.equal(await stored(fault,"commerce-purchase-order-v1:"),null);
  await fault.reload();await expect(fault.getByTestId("recover-purchase")).toBeVisible();await fault.getByTestId("recover-purchase").click();await created(fault,q5);
  const storageAttempts=calls.slice(storageStart).filter(x=>x.path==="/api/buyer/checkout"&&x.method==="POST");assert.equal(storageAttempts.length,2);assert.equal(storageAttempts[0].key,storageAttempts[1].key);
  pass("BO05 locator storage failure retains and reloads same checkout without duplicate hold");

  // A later denial after commit is not evidence of no order. Keep owner/journal.
  const c6=await newContext(),{p:revoked,quote:q6}=await quotePage(c6);await fill(revoked);await confirm(revoked);
  const dropRevoked=arm("checkout",{method:"POST",drop:true,repeat:true});await revoked.getByTestId("create-order").click();assert.equal(await dropRevoked.result.promise,200);
  await expect(revoked.getByTestId("recover-purchase")).toBeVisible();hook=null;
  const unresolved=await stored(revoked),cookieBefore=(await c6.cookies(origin))[0].value,resetsBefore=sessionResets;
  await control("unpublish","POST");await revoked.getByTestId("recover-purchase").click();await expect(revoked.getByTestId("recover-purchase")).toBeVisible();
  assert.deepEqual(await stored(revoked),unresolved);assert.equal((await c6.cookies(origin))[0].value,cookieBefore);assert.equal(sessionResets,resetsBefore);
  await expect(revoked.getByRole("button",{name:/new guest|new session|start over/i})).toHaveCount(0);
  await control("publish","POST");await revoked.reload();await expect(revoked.getByTestId("recover-purchase")).toBeVisible();await revoked.getByTestId("recover-purchase").click();await created(revoked,q6);
  pass("BO05 later publication revocation preserves uncertain committed order and never resets owner");

  // R2: late options hydration must never overwrite a newly confirmed address.
  const c8=await newContext(),{p:slow,quote:q8}=await quotePage(c8);await fill(slow);
  const dropA=arm("destination",{method:"PUT",drop:true,repeat:true});await slow.getByTestId("confirm-address").click();assert.equal(await dropA.result.promise,200);
  await expect(slow.getByTestId("recover-purchase")).toBeVisible();hook=null;
  const slowOptions=arm(`checkout-options?market_id=${q8.market_id}&country=TW&limit=100`,{method:"GET",after:true});
  await slow.reload();await slowOptions.entered.promise;
  await expect(slow.locator('input[name="line1"]')).toHaveValue(pii.line1);
  await slow.locator('input[name="line1"]').fill("Synthetic Confirmed New Address");
  await slow.getByTestId("confirm-address").click();await expect.poll(()=>stored(slow)).toBeNull();
  slowOptions.release.resolve();await expect(slow.getByTestId("create-order")).toBeEnabled();
  await expect(slow.locator('input[name="line1"]')).toHaveValue("Synthetic Confirmed New Address");
  await slow.getByTestId("create-order").click();const order8=await created(slow,q8);
  assert.equal((await api(slow,"GET",`orders/${order8}`)).body.snapshot.destination.home_address.line1,"Synthetic Confirmed New Address");
  await expect(slow.getByTestId("order-section")).toContainText("Synthetic Confirmed New Address");
  pass("BO03 delayed real options cannot replace edited confirmed address or order snapshot");

  // R1: expiry cannot remove the only recovery surface; a fresh tab has no
  // session quote locator and still must recover the shared destination intent.
  const c9=await newContext(),{p:expiredDest,quote:q9}=await quotePage(c9,true);await fill(expiredDest);
  const dropExpired=arm("destination",{method:"PUT",drop:true,repeat:true});await expiredDest.getByTestId("confirm-address").click();assert.equal(await dropExpired.result.promise,200);
  await expect(expiredDest.getByTestId("recover-purchase")).toBeVisible();hook=null;
  const expiredMarker=await stored(expiredDest);
  await control(`expire-quote/${q9.id}`,"POST");await expiredDest.clock.fastForward(61000);
  await expect(expiredDest.getByRole("button",{name:"Reload quotation",exact:true})).toBeDisabled();
  await expect(expiredDest.getByTestId("confirm-address")).toBeEnabled();
  const noQuoteTab=await c9.newPage();await noQuoteTab.goto(origin+productPath);
  assert.equal(await noQuoteTab.evaluate(()=>Object.keys(sessionStorage).filter(k=>k.startsWith("commerce-purchase-quote-v1:")).length),0);
  await expect(noQuoteTab.getByTestId("address-section")).toBeVisible();
  await expect(noQuoteTab.locator('input[name="line1"]')).toHaveValue(pii.line1);
  await expect(noQuoteTab.getByTestId("confirm-address")).toBeEnabled();await noQuoteTab.getByTestId("confirm-address").click();
  await expect.poll(()=>stored(noQuoteTab)).toBeNull();
  const replacement=calls.filter(x=>x.path==="/api/buyer/destination"&&x.method==="PUT").at(-1);assert.notEqual(replacement.key,expiredMarker.key);
  assert(JSON.parse(replacement.body).expected_version>expiredMarker.body.expected_version);
  await expect(noQuoteTab.getByTestId("order-section")).toHaveCount(0);
  pass("BO03 expired pending address retains recovery; quote-less new tab explicitly repairs shared intent");

  // BH01/02: delay the new tab's INITIAL owned GET while the original tab
  // commits continuation, loses its reply, reloads and recovers the same key.
  const originalOrder=(await api(a,"GET",`orders/${order1}`)).body;
  const originalFact=(await control("facts")).find(x=>x.id===order1);
  const originalCheckout=calls.find(x=>x.path==="/api/buyer/checkout"&&x.method==="POST"&&x.status===200&&JSON.parse(x.body).quote_id===q1.id);
  assert(originalCheckout);
  const initialOrder={entered:deferred(),release:deferred()};
  const late=await c1.newPage();
  // Hold this tab's actual responses, including storage-event refreshes. If an
  // intermediate refresh could render A, later pointer removal would invalidate
  // the initial load and accidentally hide the original race from this gate.
  const lateOrderRoute=async route=>{
    const response=await route.fetch();assert.equal(response.status(),200);
    initialOrder.entered.resolve();await initialOrder.release.promise;await route.fulfill({response});
  };
  await late.route(`${origin}/api/buyer/orders/${order1}`,lateOrderRoute);
  await late.goto(origin+productPath);await initialOrder.entered.promise;
  await expect(late.getByTestId("order-section")).toHaveCount(0);
  const continuationStart=calls.length,dropCart=arm("cart",{method:"PUT",drop:true,repeat:true});
  await a.getByTestId("continue-shopping").click();assert.equal(await dropCart.result.promise,200);
  await expect(a.getByTestId("recover-purchase")).toBeVisible();hook=null;
  const nextIntent=await stored(a);assert.equal(nextIntent.kind,"next-cart");
  assert.deepEqual(nextIntent.body,{expected_version:originalOrder.cart_version,items:[]});
  await a.reload();await expect(a.getByTestId("recover-purchase")).toBeVisible();assert.deepEqual(await stored(a),nextIntent);
  await a.getByTestId("recover-purchase").click();await expect(a.getByRole("button",{name:"Choose delivery",exact:true})).toBeEnabled();
  assert.equal(await stored(a),null);assert.equal(await stored(a,"commerce-purchase-order-v1:"),null);
  const continuationCalls=calls.slice(continuationStart).filter(x=>x.path==="/api/buyer/cart"&&x.method==="PUT");
  assert(continuationCalls.length>=2);assert.equal(new Set(continuationCalls.map(x=>x.key)).size,1);assert.equal(new Set(continuationCalls.map(x=>x.body)).size,1);
  assert.equal((await control("facts")).find(x=>x.id===order1).cart_receipts,originalFact.cart_receipts+1);
  const continuedCart=(await api(a,"GET","cart")).body;assert.equal(continuedCart.version,originalOrder.cart_version+1);assert.deepEqual(continuedCart.items,[]);
  pass("BH01 lost next-cart reply reload keeps original key/body and exactly one cart receipt");
  await expect(late.getByTestId("order-section")).toHaveCount(0);
  initialOrder.release.resolve();
  await expect(late.getByRole("button",{name:"Choose delivery",exact:true})).toBeEnabled();
  await expect(late.getByTestId("order-section")).toHaveCount(0);
  assert.equal(await stored(late,"commerce-purchase-order-v1:"),null);
  assert.deepEqual((await api(late,"GET","cart")).body,continuedCart);
  await late.unroute(`${origin}/api/buyer/orders/${order1}`,lateOrderRoute);
  pass("BH02 delayed initial owned GET cannot restore the old order after another tab continues");

  const {quote:qB}=await quotePage(c1,false,a);await fill(a);await confirm(a);
  await a.getByTestId("create-order").click();const orderB=await created(a,qB,2);assert.notEqual(orderB,order1);
  assert.deepEqual((await api(a,"GET",`orders/${order1}`)).body,originalOrder);
  assert.equal((await control("facts")).find(x=>x.id===order1).snapshot_hash,originalFact.snapshot_hash);
  const replayA=await api(a,"POST","checkout",JSON.parse(originalCheckout.body),originalCheckout.key);
  assert.equal(replayA.status,200);assert.equal(replayA.body.order_id,order1);
  assert.equal((await stored(a,"commerce-purchase-order-v1:")).order_id,orderB);
  for(const f of (await control("facts")).filter(x=>[order1,orderB].includes(x.id)))for(const key of ["orders","holds","jobs","receipts","reserve_lines"])assert.equal(f[key],2);
  pass("BH03 same buyer creates distinct B with two exact order facts and unchanged A snapshot/key replay");

  const firstHistory=await api(a,"GET","orders?limit=1");assert.equal(firstHistory.status,200);
  assert.deepEqual(firstHistory.body.items.map(x=>x.order_id),[orderB]);assert(firstHistory.body.next_cursor);
  const secondHistory=await api(a,"GET",`orders?limit=1&cursor=${encodeURIComponent(firstHistory.body.next_cursor)}`);
  assert.equal(secondHistory.status,200);assert.deepEqual(secondHistory.body.items.map(x=>x.order_id),[order1]);assert.equal(secondHistory.body.next_cursor,"");
  const summaryKeys=["order_id","created_at","cart_id","cart_version","commercial_state","fulfillment_state","currency","total_minor"].sort();
  for(const summary of [...firstHistory.body.items,...secondHistory.body.items])assert.deepEqual(Object.keys(summary).sort(),summaryKeys);
  const pointerB=await stored(a,"commerce-purchase-order-v1:"),cartB=(await api(a,"GET","cart")).body;
  const historyLoading=arm("orders?limit=20",{method:"GET",after:true});await a.getByTestId("toggle-order-history").click();
  assert.equal(await historyLoading.result.promise,200);await expect(a.getByTestId("order-history").getByRole("status")).toBeVisible();historyLoading.release.resolve();
  await expect(a.locator(".history-list li")).toHaveCount(2);
  await expect(a.locator(".history-list li").first().locator("button")).toHaveAttribute("data-order-id",orderB);
  await capture(a,"desktop-history.png",true,historyReview);
  await a.setViewportSize({width:390,height:844});await capture(a,"mobile-history.png",true,historyReview);
  assert(await a.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await a.setViewportSize({width:1440,height:900});
  await a.locator(`button[data-order-id="${order1}"]`).click();await expect(a.getByTestId("order-id")).toHaveText(order1);
  await expect(a.getByTestId("order-state")).toHaveAttribute("data-state","CANCELLED");
  assert.deepEqual(await stored(a,"commerce-purchase-order-v1:"),pointerB);assert.deepEqual((await api(a,"GET","cart")).body,cartB);
  await a.getByRole("button",{name:"Back to orders",exact:true}).click();
  for(const [locale,title] of [["zh-CN","我的订单"],["zh-TW","我的訂單"],["en","Your orders"]]){
    await a.locator("header select").selectOption(locale);await expect(a.locator("html")).toHaveAttribute("lang",locale);
    await expect(a.getByTestId("order-history").getByRole("heading",{name:title,exact:true})).toBeVisible();
    await expect(a.locator(".history-list li")).toHaveCount(2);assert.deepEqual(await stored(a,"commerce-purchase-order-v1:"),pointerB);
  }
  await a.getByTestId("toggle-order-history").click();await expect(a.getByTestId("order-id")).toHaveText(orderB);
  pass("BH04 owned keyset pages and history loading/detail/locales/back preserve current B locator and cart");

  // Lose only the convenience locator, retaining the same HttpOnly credential.
  const cookieHistory=(await c1.cookies(origin))[0].value;
  await a.evaluate(()=>{for(const key of Object.keys(localStorage))if(key.startsWith("commerce-purchase-order-v1:"))localStorage.removeItem(key);});
  await a.reload();await expect(a.getByTestId("toggle-order-history")).toBeEnabled();await expect(a.getByTestId("order-section")).toHaveCount(0);
  await a.getByTestId("toggle-order-history").click();await expect(a.locator(".history-list li")).toHaveCount(2);
  await a.locator(`button[data-order-id="${order1}"]`).click();await expect(a.getByTestId("order-id")).toHaveText(order1);
  assert.equal(await stored(a,"commerce-purchase-order-v1:"),null);assert.equal((await c1.cookies(origin))[0].value,cookieHistory);
  await a.getByTestId("toggle-order-history").click();await expect(a.getByTestId("order-section")).toHaveCount(0);
  pass("BH05 authoritative history survives noncredential locator loss without repinning an old order");

  // Preserve an already advanced cart, including another tab's selected items.
  const previousCart=(await api(tab1,"GET","cart")).body;
  const advanced=await api(tab2,"PUT","cart",{expected_version:previousCart.version,items:previousCart.items.map(x=>({...x,quantity:2}))},crypto.randomUUID());assert.equal(advanced.status,200);
  const advancedStart=calls.length;await tab1.getByTestId("continue-shopping").click();
  await expect(tab1.getByRole("button",{name:"Choose delivery",exact:true})).toBeEnabled();
  assert.deepEqual((await api(tab1,"GET","cart")).body,advanced.body);
  assert.equal(calls.slice(advancedStart).filter(x=>x.path==="/api/buyer/cart"&&x.method==="PUT").length,0);
  pass("BH06 explicit continuation preserves a newer authoritative cart without issuing a clearing PUT");

  // A quote removal no-op must retain the journal and old order pointer.
  await fault.evaluate(()=>window.__keepQuoteLocator=true);
  const quoteFailureStart=calls.length;await fault.getByTestId("continue-shopping").click();await expect(fault.getByTestId("recover-purchase")).toBeVisible();
  const failedNext=await stored(fault);assert.equal(failedNext.kind,"next-cart");assert(await stored(fault,"commerce-purchase-order-v1:"));
  await fault.reload();await expect(fault.getByTestId("recover-purchase")).toBeVisible();assert.deepEqual(await stored(fault),failedNext);
  await fault.getByTestId("recover-purchase").click();await expect(fault.getByRole("button",{name:"Choose delivery",exact:true})).toBeEnabled();
  const quoteFailureCalls=calls.slice(quoteFailureStart).filter(x=>x.path==="/api/buyer/cart"&&x.method==="PUT");assert.equal(quoteFailureCalls.length,2);
  assert.equal(quoteFailureCalls[0].key,quoteFailureCalls[1].key);assert.equal(quoteFailureCalls[0].body,quoteFailureCalls[1].body);
  assert.equal(await stored(fault),null);assert.equal(await stored(fault,"commerce-purchase-order-v1:"),null);
  pass("BH07 failed quote removal preserves continuation recovery and original key across reload");

  // Cross-owner read is a real HTTP denial and must never display a snapshot.
  const c7=await newContext(),{p:foreign}=await quotePage(c7);const otherOrder=await api(foreign,"GET",`orders/${order1}`);assert.equal(otherOrder.status,404);
  await expect(foreign.getByTestId("order-section")).toHaveCount(0);
  const foreignHistory=await api(foreign,"GET","orders?limit=1");assert.equal(foreignHistory.status,200);assert.deepEqual(foreignHistory.body,{items:[],next_cursor:""});
  assert.equal((await api(foreign,"GET",`orders?cursor=${encodeURIComponent(firstHistory.body.next_cursor)}`)).status,422);
  assert.equal((await api(a,"GET","orders?cursor=bad")).status,422);
  await foreign.getByTestId("toggle-order-history").click();await expect(foreign.getByTestId("order-history")).toContainText("No orders in this shopping session.");
  await foreign.getByTestId("toggle-order-history").click();await expect(foreign.getByTestId("address-section")).toBeVisible();
  pass("BH08 foreign buyer history is empty and foreign or malformed cursors are denied");
  await fill(foreign);await confirm(foreign);const serviceBefore=(await control("facts")).length;
  await control("service-drift","POST");const serviceDenied=foreign.waitForResponse(r=>requestIs(r,"checkout","POST"));
  await foreign.getByTestId("create-order").click();assert.equal((await serviceDenied).status(),409);
  await expect(foreign.locator(".purchase-error")).toContainText("changed");assert.equal((await control("facts")).length,serviceBefore);
  await expect(foreign.getByTestId("create-order")).toBeDisabled();
  pass("BO02 service revision drift rejects a previously confirmed quotation without an order");
  for(const c of contexts){await rememberCookie(c);for(const p of c.pages()){
    assert.equal(await p.evaluate(()=>document.cookie),"");
    const state=await p.evaluate(()=>JSON.stringify({local:{...localStorage},session:{...sessionStorage}}));
    for(const value of [...Object.values(pii),"Synthetic Changed Unit","Synthetic Concurrent Address","Synthetic Confirmed New Address",...secrets])assert(!state.includes(value),"PII/bearer leaked into persistent storage");
  }}
  const attempted=JSON.stringify(storageWrites),urls=requestURLs.join("\n"),messages=consoleText.join("\n");
  for(const value of [...Object.values(pii),"Synthetic Changed Unit","Synthetic Concurrent Address","Synthetic Confirmed New Address",...secrets]){
    assert(!attempted.includes(value),"PII/bearer attempted persistent write");assert(!urls.includes(value)&&!urls.includes(encodeURIComponent(value)),"PII/bearer URL leak");assert(!messages.includes(value),"PII/bearer console leak");
  }
  assert.equal(pageErrors.length,0,"browser application exception");
  pass("BO06 all attempted local/session writes, URLs and console exclude PII/bearer; other owner denied");
  await writeFile(path.join(evidence,"result.json"),JSON.stringify({cases:observations.length,orders,repeated_orders:[order1,orderB],observations,storage_write_attempts:storageWrites.length,scope:"actual UI/Next/Go/isolated PG; synthetic TLS and buyer data; no PSP/production",not_run:["full foundation/race/vet and existing browser regression are separate root gates","independent visual review"]},null,2),{mode:0o600});
}finally{
  if(hook?.release)hook.release.resolve();
  if(browser)await browser.close();for(const s of sockets)s.destroy();
  for(const server of [proxy,edge])if(server)await new Promise(r=>server.close(r));
  for(const child of children){if(child.exitCode===null){const ended=once(child,"exit");child.kill("SIGTERM");await Promise.race([ended,pause(2000)]);if(child.exitCode===null){child.kill("SIGKILL");await ended;}}}
  await Promise.all(logs.map(log=>new Promise(r=>log.end(r))));await rm(certDir,{recursive:true,force:true});
}
