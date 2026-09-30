// Production Next -> private Go -> isolated PG; the exact sandbox PSP POST is
// intercepted in Chromium. No form, credential or buyer token is logged.
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
import { engine, launch, ctxOpts, phone, phoneToken } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const root=process.cwd(), evidence=process.env.LC_PAYMENT_EVIDENCE;
assert(evidence && /^http:\/\/127\.0\.0\.1:\d+$/.test(process.env.LC_PAYMENT_CONTROL));
const origin="https://buyer.example", product=`${origin}/en/products/${process.env.LC_PAYMENT_PRODUCT}`;
const psp="https://sandbox-api.payuni.com.tw/api/upp";
const pii={recipient_name:"Synthetic Gate Recipient",phone:"+886900000091",region:"Synthetic Region",city:"Synthetic City",postal_code:"99991",line1:"Synthetic Address Ninety One",line2:"Synthetic Unit Ninety Two"};
const children=new Set(),sockets=new Set(),contexts=[],logs=[],calls=[],posts=[],cases=[];
const pause=ms=>new Promise(r=>setTimeout(r,ms));
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const listen=async s=>{s.listen(0,"127.0.0.1");await once(s,"listening");return s.address().port;};
const passed=name=>{cases.push(name);console.log(`PASS ${name}`);};
const certDir=await mkdtemp(path.join(tmpdir(),"lc-payment-edge-"));
let browser,edge,proxy,hook,postOrder="";
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
  for(const key of Object.keys(env))if(key.startsWith("LC_PAYMENT_"))delete env[key];
  const child=spawn(process.execPath,[path.join(root,"apps/storefront/node_modules/next/dist/bin/next"),"start","--hostname","127.0.0.1","--port",String(port)],{cwd:path.join(root,"apps/storefront"),env,stdio:["ignore",log,log]});children.add(child);
  for(let i=0;i<100;i++){
    if(child.exitCode!==null)throw new Error("owned Next failed readiness");
    try{if((await relay(port,{url:"/api/buyer/session",method:"GET",headers:{host:"buyer.example"}},Buffer.alloc(0))).status===200)return port;}catch{}
    await pause(50);
  }throw new Error("owned Next readiness timeout");
}
function arm(path,mode){return hook={path,mode,entered:deferred(),release:deferred(),result:deferred()};}
async function control(){
  const response=await fetch(`${process.env.LC_PAYMENT_CONTROL}/facts`,{headers:{"X-Gate-Key":process.env.LC_PAYMENT_CONTROL_KEY}});
  assert.equal(response.status,200);return response.json();
}
async function context(mobile=false){
  const c=await browser.newContext(ctxOpts(mobile?{...phone,viewport:{width:390,height:844},screen:{width:390,height:844},ignoreHTTPSErrors:true}:{ignoreHTTPSErrors:true,viewport:{width:1440,height:900}}));contexts.push(c);
  await c.route(/https:\/\/(?:sandbox-api|api)\.payuni\.com\.tw\//,route=>{throw new Error(`unexpected PSP route ${route.request().url()}`);});
  await c.route(psp,async route=>{
    const req=route.request();assert.equal(req.method(),"POST");assert(req.isNavigationRequest());assert.equal(req.url(),psp);assert(postOrder);
    const fields=new URLSearchParams(req.postData());
    assert.deepEqual([...fields.keys()].sort(),["EncryptInfo","HashInfo","MerID","Version"]);
    for(const key of ["EncryptInfo","HashInfo","MerID","Version"])assert.equal(fields.getAll(key).length,1);
    assert.equal(fields.get("Version"),"2.0");assert.equal(fields.get("MerID"),"mock-account");
    assert.match(fields.get("EncryptInfo"),/^[0-9a-f]+$/);assert.match(fields.get("HashInfo"),/^[0-9A-F]{64}$/);
    const canonical=[psp,...["Version","MerID","EncryptInfo","HashInfo"].map(k=>fields.get(k))].join("\n");
    posts.push({order_id:postOrder,digest:createHash("sha256").update(canonical).digest("hex")});
    await route.fulfill({status:200,contentType:"text/html; charset=utf-8",body:"<!doctype html><title>Synthetic mock PSP</title>"});
  });
  return c;
}
const requestIs=(response,suffix,method)=>new URL(response.url()).pathname===`/api/buyer/${suffix}`&&response.request().method()===method;
async function makeOrder(page){
  await expect(page.locator("#quantity")).toBeEnabled();
  await page.locator("#quantity").fill("2");
  await page.getByRole("button",{name:"Choose delivery",exact:true}).click();
  const quotation=page.waitForResponse(r=>requestIs(r,"quotes","POST"));
  await page.getByRole("button",{name:"Get current total",exact:true}).click();assert.equal((await quotation).status(),200);
  await expect(page.getByTestId("address-section")).toBeVisible();
  for(const [key,value] of Object.entries(pii))await page.locator(`input[name="${key}"]`).fill(value);
  await page.getByTestId("confirm-address").click();await expect(page.getByTestId("create-order")).toBeEnabled();
  await page.getByTestId("create-order").click();await expect(page.getByTestId("order-section")).toBeVisible();
  const id=(await page.getByTestId("order-id").innerText()).trim();assert.match(id,/^[0-9a-f-]{36}$/);
  await expect(page.getByTestId("order-payment")).toBeVisible();await expect(page.getByTestId("payment-status")).toHaveAttribute("data-state","NOT_STARTED");
  const payment=await page.evaluate(async orderID=>{
    const session=await(await fetch("/api/buyer/session",{cache:"no-store"})).json();
    const response=await fetch(`/api/buyer/orders/${orderID}/payment`,{headers:{"X-Buyer-Context":session.context},cache:"no-store"});
    return {status:response.status,body:await response.json()};
  },id);
  assert.equal(payment.status,200);assert.equal(payment.body.currency,"TWD");assert.equal(payment.body.total_minor,2500);
  assert.deepEqual(payment.body.methods.map(x=>[x.code,x.version]),[["payuni_credit",1]]);
  await expect(page.getByTestId("pay-order")).toBeVisible();
  await expect(page.getByTestId("payment-test-mode")).toBeVisible();return id;
}
async function paymentMarker(page,id){
  return page.evaluate(id=>{const key=Object.keys(localStorage).find(k=>k.endsWith(`:${id}`)&&k.startsWith("commerce-order-payment-v1:"));return key?JSON.parse(localStorage.getItem(key)):null;},id);
}
function paymentCalls(id,step){return calls.filter(x=>x.path===`/api/buyer/orders/${id}/payment/${step}`&&x.method==="POST");}
async function storageSafe(page){
  assert.equal(await page.evaluate(()=>{const text=JSON.stringify(localStorage);return /EncryptInfo|HashInfo|mock-account|recipient_name|phone/.test(text);}),false);
  await expect(page.locator('form[action*="payuni.com.tw"]')).toHaveCount(0);
}
try{
  execFileSync("openssl",["req","-x509","-newkey","rsa:2048","-nodes","-keyout",path.join(certDir,"key.pem"),"-out",path.join(certDir,"cert.pem"),"-days","1","-subj","/CN=buyer.example"],{stdio:"ignore"});
  const port=await startNext();
  edge=https.createServer({key:await readFile(path.join(certDir,"key.pem")),cert:await readFile(path.join(certDir,"cert.pem"))},async(req,res)=>{
    try{
      const chunks=[];for await(const x of req)chunks.push(x);const body=Buffer.concat(chunks);
      const path=new URL(req.url,origin).pathname;
      if(path.startsWith("/api/buyer/orders/")&&path.includes("/payment/"))calls.push({path,method:req.method,key:req.headers["idempotency-key"]??null,body:body.length?body.toString():null});
      const active=hook&&hook.path===path&&req.method==="POST"?hook:null;
      if(active){hook=null;active.entered.resolve();}
      const out=await relay(port,req,body);if(active)active.result.resolve(out.status);
      if(active?.mode==="drop"){res.destroy();return;}
      if(active?.mode==="partial"){
        const headers={...out.headers};delete headers.connection;delete headers["transfer-encoding"];delete headers["content-encoding"];
        headers["content-length"]=String(out.body.length+16);
        res.writeHead(out.status,headers);res.write(out.body.subarray(0,1));res.flushHeaders();await pause(10);res.destroy();return;
      }
      if(active?.mode==="hold")await active.release.promise;
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
  const c=await context(),page=await c.newPage();
  const entry=await page.goto(product);assert(entry);const csp=entry.headers()["content-security-policy"]??"";
  assert.match(csp,/form-action/);assert(csp.includes("'self'")&&csp.includes(psp)&&csp.includes("https://api.payuni.com.tw/api/upp"));
  const a=await makeOrder(page);passed("BPU02 actual fresh order/payment snapshot and exact PSP CSP");
  await expect(page.getByTestId("payment-status")).toHaveText("Payment status: Not paid");
  await expect(page.getByTestId("payment-commercial-status")).toHaveText("Order status: Not paid");
  await page.getByTestId("order-payment").screenshot({path:path.join(evidence,"desktop-payment-ready.png")});
  await page.screenshot({path:path.join(evidence,"desktop-order-ready.png"),fullPage:true});
  await page.getByTestId("continue-shopping").click();await expect(page.getByRole("button",{name:"Choose delivery",exact:true})).toBeEnabled();
  const b=await makeOrder(page);assert.notEqual(a,b);
  const initial=await control();assert.equal(initial.attempts,0);assert.equal(initial.pages,0);
  await page.evaluate(()=>{window.__nativeOpen=window.open;window.open=()=>null;});
  await page.getByTestId("pay-order").click();await expect(page.getByTestId("payment-error")).toBeVisible();
  assert.deepEqual(await control(),initial);assert.equal(await paymentMarker(page,b),null);passed("BPU02 blocked child causes zero prepare/take");
  await page.evaluate(()=>{window.open=(...args)=>{const target=window.__nativeOpen(...args);target?.close();return target;};});
  await page.getByTestId("pay-order").click();await expect(page.getByTestId("payment-error")).toBeVisible();
  assert.deepEqual(await control(),initial);assert.equal(await paymentMarker(page,b),null);passed("BPU02 closed child causes zero prepare/take");
  await page.evaluate(()=>{window.open=window.__nativeOpen;});
  postOrder=b;const firstPrepare=arm(`/api/buyer/orders/${b}/payment/prepare`,"hold");
  const pop=page.waitForEvent("popup");await page.getByTestId("pay-order").click();const child=await pop;
  assert.equal(await firstPrepare.result.promise,200);
  await expect(page.getByTestId("pay-order")).toHaveCount(0);
  const other=await c.newPage();await other.goto(product);
  await expect(other.getByTestId("order-id")).toHaveText(b);
  await expect(other.getByTestId("pay-order")).toBeVisible();
  const secondPop=other.waitForEvent("popup");await other.getByTestId("pay-order").click();await secondPop;
  await expect.poll(()=>other.evaluate(async()=>(await navigator.locks.query()).pending.filter(x=>x.name==="commerce-purchase-write-v1").length)).toBeGreaterThan(0);
  firstPrepare.release.resolve();
  await expect.poll(()=>posts.length).toBe(1);await child.waitForURL(psp);await child.waitForLoadState("domcontentloaded");
  assert.equal(await child.evaluate(()=>window.opener),null);
  await expect(other.getByTestId("payment-error")).toBeVisible();
  assert.equal(paymentCalls(b,"prepare").length,1);assert.equal(paymentCalls(b,"handoff").length,1);
  await expect(page.getByTestId("payment-status")).toHaveAttribute("data-state","PENDING");
  assert.equal((await paymentMarker(page,b)).stage,"handoff_started");await storageSafe(page);
  assert.equal(paymentCalls(b,"prepare").length,1);assert.equal(paymentCalls(b,"handoff").length,1);
  passed("BPU02 fresh native POST equals one prepare/take, one pending fact, no persisted form");
  passed("BPU02 concurrent tabs queue on actual Web Lock; duplicate Pay adds no prepare/take/form");

  const mobile=await context(true);await mobile.addCookies(await c.cookies(origin));
  const mobilePage=await mobile.newPage();await mobilePage.goto(product);
  // Engine-specific: Playwright's macOS WebKit reports navigator.maxTouchPoints===0 even with hasTouch (real iOS Safari reports 5), so under webkit
  // touch capability is asserted through ontouchstart + (pointer: coarse); Chromium keeps the original maxTouchPoints>0 assertion.
  assert(await mobilePage.evaluate(([tok,wk])=>(wk?"ontouchstart" in window&&matchMedia("(pointer: coarse)").matches:navigator.maxTouchPoints>0)&&navigator.userAgent.includes(tok),[phoneToken,engine==="webkit"]));
  await mobilePage.locator("header select").selectOption("zh-TW");
  await mobilePage.getByTestId("toggle-order-history").click();await mobilePage.locator(`button[data-order-id="${a}"]`).click();
  await expect(mobilePage.getByTestId("order-id")).toHaveText(a);await expect(mobilePage.getByTestId("payment-status")).toHaveAttribute("data-state","NOT_STARTED");
  await expect(mobilePage.locator("html")).toHaveAttribute("lang","zh-TW");
  await expect(mobilePage.locator(".order-total")).toContainText(/TWD\s*25\.00/);
  await expect(mobilePage.getByTestId("payment-status")).toHaveText("付款狀態: 尚未付款");
  await expect(mobilePage.getByTestId("payment-commercial-status")).toHaveText("訂單狀態: 尚未付款");
  await mobilePage.getByTestId("order-payment").screenshot({path:path.join(evidence,"mobile-native-history-ready.png")});
  const stalled=arm(`/api/buyer/orders/${a}/payment/prepare`,"hold");
  const changed=mobilePage.waitForEvent("popup");await mobilePage.getByTestId("pay-order").click();const unowned=await changed;
  assert.equal(await stalled.result.promise,200);await unowned.goto("about:blank#foreign");stalled.release.resolve();
  await expect(mobilePage.getByTestId("payment-error")).toBeVisible();assert.equal(paymentCalls(a,"handoff").length,0);
  const marker=await paymentMarker(mobilePage,a);assert.equal(marker.stage,"prepare");
  const beforeReplay=paymentCalls(a,"prepare").length;postOrder=a;
  const historical=mobilePage.waitForEvent("popup");await mobilePage.getByTestId("pay-order").click();const historyChild=await historical;
  await expect.poll(()=>posts.length).toBe(2);await historyChild.waitForURL(psp);await historyChild.waitForLoadState("domcontentloaded");
  assert.equal(await historyChild.evaluate(()=>window.opener),null);
  const replay=paymentCalls(a,"prepare").slice(beforeReplay);assert.equal(replay.length,1);
  assert.equal(replay[0].key,paymentCalls(a,"prepare")[0].key);assert.equal(replay[0].body,paymentCalls(a,"prepare")[0].body);
  assert.equal(paymentCalls(a,"handoff").length,1);await storageSafe(mobilePage);
  await expect(mobilePage.getByTestId("payment-status")).toHaveAttribute("data-state","PENDING");
  await expect(mobilePage.getByTestId("order-payment")).toHaveAttribute("aria-busy","false");
  await mobilePage.screenshot({path:path.join(evidence,"mobile-native-history-readonly.png"),fullPage:true});
  passed("BPU02 touch/phone-UA history Pay replays original key/body after navigated child, then posts once");

  await page.locator("header select").selectOption("zh-CN");await expect(page.locator("html")).toHaveAttribute("lang","zh-CN");
  await expect(page.getByTestId("order-id")).toHaveText(b);
  await page.getByTestId("continue-shopping").click();await page.locator("header select").selectOption("en");
  await expect(page.getByRole("button",{name:"Choose delivery",exact:true})).toBeEnabled();const d=await makeOrder(page);
  await page.setViewportSize({width:390,height:844});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await page.getByTestId("order-payment").screenshot({path:path.join(evidence,"mobile-payment-ready.png")});
  const lostPrepare=arm(`/api/buyer/orders/${d}/payment/prepare`,"partial");const preparePopup=page.waitForEvent("popup");
  await page.getByTestId("pay-order").click();await preparePopup;assert.equal(await lostPrepare.result.promise,200);
  await expect(page.getByTestId("payment-error")).toBeVisible();assert.equal((await paymentMarker(page,d)).stage,"prepare");
  assert.equal(paymentCalls(d,"handoff").length,0);
  const originalCookies=await c.cookies(origin);assert(originalCookies.length>0);
  const contextAtPrepare=(await paymentMarker(page,d)).context;
  const changedContext=arm(`/api/buyer/orders/${d}/payment/prepare`,"hold");const stalePopup=page.waitForEvent("popup");
  await page.getByTestId("pay-order").click();await stalePopup;assert.equal(await changedContext.result.promise,200);
  await c.clearCookies();const replacement=await c.newPage();await replacement.goto(product);
  await expect(replacement.locator("#quantity")).toBeEnabled();
  const replacementSession=await replacement.evaluate(async()=> (await(await fetch("/api/buyer/session",{cache:"no-store"})).json()).context);
  assert(replacementSession&&replacementSession!==contextAtPrepare);
  changedContext.release.resolve();
  await expect(page.getByTestId("order-payment")).toHaveAttribute("aria-busy","false");
  assert.equal(paymentCalls(d,"handoff").length,0);assert.equal((await paymentMarker(page,d)).stage,"prepare");
  await replacement.close();await c.clearCookies();await c.addCookies(originalCookies);
  await page.reload();await expect(page.getByTestId("order-id")).toHaveText(d);await expect(page.getByTestId("pay-order")).toBeVisible();
  passed("BPU02 committed prepare with replaced buyer context cannot Take; original session can explicitly replay");
  const closedAfterTake=arm(`/api/buyer/orders/${d}/payment/handoff`,"hold");const closedPopup=page.waitForEvent("popup");
  await page.getByTestId("pay-order").click();const closedChild=await closedPopup;assert.equal(await closedAfterTake.result.promise,200);
  await closedChild.close();closedAfterTake.release.resolve();
  await expect(page.getByTestId("payment-error")).toBeVisible();assert.equal((await paymentMarker(page,d)).stage,"handoff_started");
  const prepareCalls=paymentCalls(d,"prepare");assert.equal(prepareCalls.length,3);
  assert(prepareCalls.every(x=>x.key===prepareCalls[0].key&&x.body===prepareCalls[0].body));
  assert.equal(paymentCalls(d,"handoff").length,1);assert.equal(posts.length,2);
  passed("BPU02 child closed after committed Take cannot release cached form or retry");

  await page.getByTestId("continue-shopping").click();await expect(page.getByRole("button",{name:"Choose delivery",exact:true})).toBeEnabled();
  const lostOrder=await makeOrder(page);
  const lost=arm(`/api/buyer/orders/${lostOrder}/payment/handoff`,"partial");const lostPopup=page.waitForEvent("popup");
  await page.getByTestId("pay-order").click();await lostPopup;assert.equal(await lost.result.promise,200);
  await expect(page.getByTestId("payment-error")).toBeVisible();assert.equal((await paymentMarker(page,lostOrder)).stage,"handoff_started");
  assert.equal(paymentCalls(lostOrder,"prepare").length,1);assert.equal(paymentCalls(lostOrder,"handoff").length,1);assert.equal(posts.length,2);
  await page.reload();await page.bringToFront();await expect(page.getByTestId("order-payment")).toBeVisible();
  assert.equal(paymentCalls(lostOrder,"handoff").length,1);await expect(page.getByTestId("pay-order")).toHaveCount(0);
  assert.equal((await control()).issued,4);await storageSafe(page);
  await page.screenshot({path:path.join(evidence,"mobile-payment-readonly.png"),fullPage:true});
  passed("BPU02 lost prepare exact replay; separate lost handoff/reload/focus is GET-only on mobile");

  const markerText="untrusted-payment-callback-marker";
  const get=await relay(port,{url:`/payment/return?proof=${markerText}`,method:"GET",headers:{host:"buyer.example"}},Buffer.alloc(0));
  const ret=await relay(port,{url:"/payment/return",method:"POST",headers:{host:"buyer.example","content-type":"application/x-www-form-urlencoded"}},Buffer.from(markerText));
  assert.equal(get.status,200);assert.equal(ret.status,200);assert.equal(get.body.toString(),ret.body.toString());
  assert(!get.body.toString().includes(markerText));
  for(const response of [get,ret]){
    assert.match(response.headers["cache-control"]??"",/no-store/);
    assert.equal(response.headers["referrer-policy"],"no-referrer");
    assert.equal(response.headers.location,undefined);assert.equal(response.headers["set-cookie"],undefined);
    const policy=response.headers["content-security-policy"]??"";
    assert.match(policy,/default-src 'none'/);assert.match(policy,/style-src 'sha256-[^']+'/);
    assert.match(policy,/form-action 'none'/);assert(!policy.includes("form-action 'self'"));
  }
  assert.equal((await control()).issued,4);assert.equal(posts.length,2);
  const returnPage=await c.newPage();await returnPage.goto(`${origin}/payment/return`);
  await returnPage.screenshot({path:path.join(evidence,"desktop-payment-return.png"),fullPage:true});
  const mobileReturn=await mobile.newPage();await mobileReturn.goto(`${origin}/payment/return`);
  await mobileReturn.screenshot({path:path.join(evidence,"mobile-payment-return.png"),fullPage:true});await mobileReturn.close();
  await returnPage.close();
  passed("BPU02 neutral GET/POST return never reports payment or changes facts");
  for(const locale of ["zh-CN","zh-TW","en"]){await page.locator("header select").selectOption(locale);await expect(page.locator("html")).toHaveAttribute("lang",locale);await expect(page.getByTestId("payment-test-mode")).toBeVisible();}
  passed("BPU02 three locales and mobile retain server test-mode disclosure");
  await writeFile(path.join(evidence,"result.json"),JSON.stringify({cases:cases.length,posts}),{flag:"wx",mode:0o600});
}finally{
  for(const c of contexts)await c.close().catch(()=>{});
  await browser?.close().catch(()=>{});
  for(const s of sockets)s.destroy();
  for(const server of [proxy,edge])if(server)await new Promise(r=>server.close(r));
  for(const child of children)child.kill("SIGKILL");
  for(const log of logs)log.end();
  await rm(certDir,{recursive:true,force:true});
}
