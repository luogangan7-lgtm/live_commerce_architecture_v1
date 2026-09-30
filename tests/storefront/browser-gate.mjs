// Real browser and production Next; only the TLS/domain edge is a local fixture.
// No APIRequestContext cookie jar: Set-Cookie reaches Chromium solely through
// the actual HTTPS response, including the headers-before-body abort gates.
import assert from "node:assert/strict";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { spawn, execFileSync } from "node:child_process";
import { once } from "node:events";
import { readFile, writeFile, mkdtemp, rm, mkdir } from "node:fs/promises";
import { createWriteStream } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { stripTypeScriptTypes } from "node:module";
import { expect } from "@playwright/test";
import { launch, ctxOpts } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const root = process.cwd(), evidence = process.env.LC_BUYER_EVIDENCE;
assert(evidence && process.env.COMMERCE_BUYER_API_ORIGIN?.startsWith("http://127.0.0.1:"));
const origin = "https://buyer.example";
const journal = "commerce-buyer-pending-v1";
const deferred = () => { let resolve; const promise = new Promise(r => resolve = r); return { promise, resolve }; };
const sockets = new Set(), children = new Set(), logs = [];
let browser, edge, proxy, nextIndex = 0, hook, cases = 0, prepares = 0, orderID = "";
const certDir = await mkdtemp(path.join(tmpdir(), "lc-buyer-edge-"));
const pass = name => { cases++; console.log(`PASS ${name}`); };
const wait = ms => new Promise(r => setTimeout(r, ms));
async function listen(server) { server.listen(0,"127.0.0.1"); await once(server,"listening"); return server.address().port; }

async function startNext(index) {
  const reserve=net.createServer(), port=await listen(reserve); await new Promise(r=>reserve.close(r));
  const log=createWriteStream(path.join(evidence,`next-${index}-${Date.now()}.log`),{flags:"wx",mode:0o600}); logs.push(log);
  await once(log,"open"); // spawn requires an open file descriptor, not a pending stream
  const child=spawn(process.execPath,[path.join(root,"apps/storefront/node_modules/next/dist/bin/next"),"start","--hostname","127.0.0.1","--port",String(port)],{
    cwd:path.join(root,"apps/storefront"),env:{...process.env,NODE_ENV:"production",NEXT_TELEMETRY_DISABLED:"1"},stdio:["ignore",log,log],
  }); children.add(child);
  for(let i=0;i<100;i++) {
    if(child.exitCode!==null) throw new Error("owned Next exited before readiness");
    // Node24 fetch ignores a supplied Host; raw http preserves the virtual
    // hostname the real TLS edge will forward. Readiness must test that route.
    try { const res=await relay(port,{url:"/api/buyer/session",method:"GET",headers:{host:"buyer.example"}},Buffer.alloc(0)); if(res.status===200) return {port,child}; } catch {}
    await wait(50);
  }
  throw new Error("owned Next readiness timeout");
}

// Fixed loopback target, no redirects and no cookie jar. Preserve Host/Origin
// for the actual production boundary; never rewrite them into a trusted scope.
function relay(port,req,body) {
  return new Promise((resolve,reject)=>{
    const headers={...req.headers}; delete headers.connection; delete headers["transfer-encoding"];
    if(body.length) headers["content-length"]=String(body.length); else delete headers["content-length"];
    const call=http.request({hostname:"127.0.0.1",port,path:req.url,method:req.method,headers},r=>{
      const chunks=[];r.on("data",x=>chunks.push(x));r.on("end",()=>resolve({status:r.statusCode,headers:r.headers,body:Buffer.concat(chunks)}));r.on("error",reject);
    }); call.setTimeout(15000,()=>call.destroy(new Error("fixture relay timeout")));call.on("error",reject);call.end(body);
  });
}

try {
  // A generated disposable cert, not a merchant certificate or DNS proof.
  execFileSync("openssl",["req","-x509","-newkey","rsa:2048","-nodes","-keyout",path.join(certDir,"key.pem"),"-out",path.join(certDir,"cert.pem"),"-days","1","-subj","/CN=buyer.example"],{stdio:"ignore"});
  const next=[await startNext(0),await startNext(1)];
  const hits=[0,0];
  const clientJS=stripTypeScriptTypes(await readFile(path.join(root,"apps/storefront/lib/buyer-client.ts"),"utf8"),{mode:"strip"});
  edge=https.createServer({key:await readFile(path.join(certDir,"key.pem")),cert:await readFile(path.join(certDir,"cert.pem"))},async(req,res)=>{
    try {
      if(req.url==="/") { res.writeHead(200,{"content-type":"text/html","cache-control":"no-store"});res.end('<!doctype html><title>Buyer transport gate</title><p>Test-only runner, not storefront UI.</p><script type="module">import * as buyer from "/__gate_client.js"; window.buyer=buyer;</script>');return; }
      if(req.url==="/__gate_client.js") {res.writeHead(200,{"content-type":"text/javascript","cache-control":"no-store"});res.end(clientJS);return;}
      // buyer-client.ts imports sibling modules ("./claim-contract.ts"); the browser resolves them against
      // /__gate_client.js, so serve exactly the storefront lib files it names, type-stripped, never Next.
      const sibling=/^\/([a-z][a-z-]*)\.ts$/.exec(req.url);
      if(sibling&&clientJS.includes(`"./${sibling[1]}.ts"`)) {res.writeHead(200,{"content-type":"text/javascript","cache-control":"no-store"});res.end(stripTypeScriptTypes(await readFile(path.join(root,"apps/storefront/lib",sibling[1]+".ts"),"utf8"),{mode:"strip"}));return;}
      const chunks=[];for await(const x of req) chunks.push(x);let body=Buffer.concat(chunks);
      if(req.url==="/api/buyer/session/prepare") prepares++;
      const current=hook&&hook.path===req.url?hook:null;
      if(current) {if(!current.repeat) hook=null;current.entered.resolve();if(current.before) await current.release.promise;if(current.invalid) body=Buffer.from('{"unexpected":true}');}
      const index=nextIndex++%2;hits[index]++;
      const out=await relay(next[index].port,req,body);
      if(current) current.result.resolve(out.status);
      if(current?.drop) {res.destroy();return;}
      const headers={...out.headers};delete headers["transfer-encoding"];delete headers.connection;
      if(current?.after) {if(current.followStatus) hook=current.followStatus;res.writeHead(out.status,headers);res.flushHeaders();current.headers.resolve();await current.release.promise;}
      else res.writeHead(out.status,headers);
      if(!res.destroyed) res.end(out.body);
    } catch {if(!res.headersSent) res.writeHead(502);res.end();}
  });
  const edgePort=await listen(edge);
  proxy=http.createServer((_,res)=>{res.writeHead(403);res.end();});
  proxy.on("connect",(req,socket,head)=>{
    // CONNECT terminates only in this fixture; never forward arbitrary hosts.
    if(req.url!=="buyer.example:443") {socket.destroy();return;}
    const upstream=net.connect(edgePort,"127.0.0.1",()=>{socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");if(head.length) upstream.write(head);socket.pipe(upstream).pipe(socket);});
    for(const s of [socket,upstream]) {sockets.add(s);s.on("close",()=>sockets.delete(s));s.on("error",()=>{socket.destroy();upstream.destroy();});}
  });
  const proxyPort=await listen(proxy);
  browser=await launch({headless:true,proxy:{server:`http://127.0.0.1:${proxyPort}`}});
  const context=()=>browser.newContext(ctxOpts({ignoreHTTPSErrors:true}));
  const page=async c=>{const p=await c.newPage();await p.goto(origin);await p.waitForFunction(()=>Boolean(window.buyer));return p;};
  const init=p=>p.evaluate(()=>window.buyer.initializeBuyerSession());
  const state=p=>p.evaluate(()=>window.buyer.readBuyerSession());
  const pending=p=>p.evaluate(k=>localStorage.getItem(k),journal);
  const caught=p=>p.evaluate(async()=>{try{await window.buyer.initializeBuyerSession();return "success";}catch(e){return e.code||"error";}});
  const arm=(suffix,fields={})=>(hook={path:"/api/buyer/session/"+suffix,entered:deferred(),release:deferred(),headers:deferred(),result:deferred(),...fields});
  const api=(p,method,suffix,context,body,key)=>p.evaluate(async x=>{const r=await window.buyer.buyerRequest(...x);return {status:r.status,body:r.status===204?null:await r.json()};},[method,suffix,context,body,key]);

  const c=await context(), a=await page(c), b=await page(c);
  const [sa,sb]=await Promise.all([init(a),init(b)]);
  assert.equal(sa.state,"active");assert.equal(sa.context,sb.context);assert.equal(await pending(a),null);
  const cookies=await c.cookies(origin);assert.equal(cookies.length,1);assert(cookies[0].httpOnly&&cookies[0].secure&&cookies[0].sameSite==="Lax");
  assert.equal(await a.evaluate(()=>document.cookie),"");
  const storage=await a.evaluate(()=>JSON.stringify({...localStorage}));assert(!storage.includes(cookies[0].value));
  // Inspect the signed test cookie in Node, never send its bearer into page JS.
  const bareToken=JSON.parse(Buffer.from(cookies[0].value.split(".")[0],"base64url").toString()).token;
  const dom=await a.content();assert(!storage.includes(bareToken)&&!dom.includes(bareToken)&&!JSON.stringify([sa,sb]).includes(bareToken));
  pass("multi-tab initialization reuses one context; HttpOnly cookie absent from JS");

  const scope=sa.context;
  const cat=await api(a,"GET","catalog",scope);assert.equal(cat.status,200);
  const options=await api(a,"GET",`checkout-options?market_id=${process.env.LC_BUYER_MARKET}&country=TW`,scope);assert.equal(options.status,200);
  const cartBody={items:[{sku_id:process.env.LC_BUYER_SKU,quantity:1}]};
  const cart=await api(a,"PUT","cart",scope,cartBody,"browser-cart-1");assert.equal(cart.status,200);
  assert.deepEqual((await api(b,"PUT","cart",scope,cartBody,"browser-cart-1")).body,cart.body);
  const quote=await api(a,"POST","quotes",scope,{cart_version:cart.body.version,market_id:process.env.LC_BUYER_MARKET,country:"TW",method:process.env.LC_BUYER_METHOD},"browser-quote-1");assert.equal(quote.status,200);
  const destination=await api(a,"PUT","destination",scope,{cart_version:cart.body.version,kind:"home",country:"TW",recipient_name:"Synthetic Buyer",phone:"+886900000001",home_address:{city:"Synthetic city",line1:"Synthetic home address"}},"browser-destination-1");assert.equal(destination.status,200);
  const checkout={quote_id:quote.body.id,destination_id:destination.body.id,cart_version:cart.body.version,service_version:1,allocation_version:1};
  const receipt=await api(a,"POST","checkout",scope,checkout,"browser-checkout-1");assert.equal(receipt.status,200);orderID=receipt.body.order_id;
  assert.deepEqual((await api(b,"POST","checkout",scope,checkout,"browser-checkout-1")).body,receipt.body);
  const order=await api(a,"GET","orders/"+orderID,scope);assert.equal(order.status,200);assert.equal(order.body.commercial_state,"DRAFT");
  assert(!JSON.stringify([cat,options,cart,quote,destination,receipt,order]).includes(bareToken));
  pass("actual catalog/options/cart/quote/home/checkout/order; cross-instance replay");

  next[0].child.kill("SIGTERM");await once(next[0].child,"exit");children.delete(next[0].child);next[0]=await startNext(0);
  assert.equal((await state(a)).context,scope);assert.equal((await api(a,"GET","orders/"+orderID,scope)).status,200);
  pass("Next process restart preserves cookie and real-PG order");

  const reset=await a.evaluate(x=>window.buyer.resetBuyerSession(x),scope);assert.notEqual(reset.context,scope);
  const stale=await b.evaluate(async x=>{try{await window.buyer.buyerRequest("PUT","cart",x,{items:[]},"stale-cart-1");return "success";}catch(e){return e.code;}},scope);
  assert.equal(stale,"context_changed");await init(a);
  const now=await state(a);await a.evaluate(x=>window.buyer.logoutBuyerSession(x),now.context);
  assert.equal((await state(b)).state,"inactive");assert.equal((await c.cookies(origin)).length,1);
  pass("reset denies stale-tab write; logout leaves inert cookie");
  await c.close();

  // Fully received negative response is safe to recover; it never Set-Cookie.
  const cn=await context(), pn=await page(cn);arm("prepare",{invalid:true});
  assert.notEqual(await caught(pn),"success");assert.equal(await pending(pn),null);assert.equal((await init(pn)).state,"active");await cn.close();
  pass("definite local422 clears pending journal and permits explicit retry");

  // Chromium may transparently retry an empty response on a reused connection.
  // Keep the fault window closed until fetch actually fails, not for one packet.
  const cl=await context(), pl=await page(cl);arm("prepare",{drop:true,repeat:true});
  assert.notEqual(await caught(pl),"success");hook=null;assert(await pending(pl));assert.equal((await cl.cookies(origin)).length,0);
  const prepareCount=prepares;assert.notEqual(await caught(pl),"success");assert(await pending(pl));assert.equal(prepares,prepareCount);await cl.close();
  pass("lost prepare before headers stays fail-closed without remint");

  const ca=await context();await ca.addInitScript(()=>{const original=window.fetch;window.fetch=(input,options)=>{if(String(input).endsWith("/session/prepare")){const controller=new AbortController();window.abortPrepare=()=>controller.abort();return original(input,{...options,signal:controller.signal});}return original(input,options);};});
  const pa=await page(ca), ha=arm("prepare",{after:true});const aborted=caught(pa);await ha.headers.promise;
  // Header delivery, not a rejected Promise, is the cookie evidence.
  await pa.waitForFunction(()=>Boolean(window.abortPrepare));
  for(let i=0;i<50&&(await ca.cookies(origin)).length===0;i++) await wait(10);
  assert.equal((await ca.cookies(origin)).length,1);const preparedCookie=(await ca.cookies(origin))[0].value;
  await pa.evaluate(()=>window.abortPrepare());await aborted;ha.release.resolve();
  // The client may already confirm headers through status without reading the
  // response body. Either path must retain precisely the prepared capability.
  const pa2=await page(ca);assert.equal((await init(pa2)).state,"active");assert.equal(await pending(pa2),null);assert((await ca.cookies(origin))[0].value===preparedCookie);await ca.close();
  pass("abort after Set-Cookie headers recovers same prepared context");

  const cc=await context(), pc=await page(cc);
  const pausedStatus={path:"/api/buyer/session",before:true,entered:deferred(),release:deferred(),result:deferred()};
  const hc=arm("prepare",{after:true,followStatus:pausedStatus});
  const closing=caught(pc).catch(()=>"closed");await hc.headers.promise;await pausedStatus.entered.promise;
  for(let i=0;i<50&&(await cc.cookies(origin)).length===0;i++) await wait(10);
  assert.equal((await cc.cookies(origin)).length,1);const closedCookie=(await cc.cookies(origin))[0].value;assert(await pc.evaluate(k=>Boolean(localStorage.getItem(k)),journal));await pc.close();await closing;
  const pc2=await page(cc);assert.equal((await init(pc2)).state,"active");assert((await cc.cookies(origin))[0].value===closedCookie);hc.release.resolve();pausedStatus.release.resolve();await cc.close();
  pass("tab death releases Web Lock; another tab reconciles persisted journal");

  const ci=await context(), pi=await page(ci);const hi=arm("activate",{drop:true,repeat:true});
  assert.notEqual(await caught(pi),"success");hook=null;assert.equal(await hi.result.promise,200);
  const existing=await state(pi);assert.equal(existing.state,"active");assert.equal((await init(pi)).context,existing.context);await ci.close();
  pass("lost activation response reads committed session without duplicate issue");

  const cr=await context(), pr=await page(cr);const hr=arm("activate",{before:true});
  const race=caught(pr).catch(()=>"closed");await hr.entered.promise;await pr.close();await race;
  const pr2=await page(cr), prepared=await state(pr2);assert.equal(prepared.state,"inactive");
  await pr2.evaluate(x=>window.buyer.logoutBuyerSession(x),prepared.context);hr.release.resolve();assert.equal(await hr.result.promise,401);
  assert.equal((await state(pr2)).state,"inactive");await cr.close();
  pass("logout retires token before delayed activation from closed tab");

  const raw=async(headers,body="{}",suffix="session/prepare",method="POST")=>relay(next[0].port,{url:`/api/buyer/${suffix}`,method,headers:{host:"buyer.example","content-type":"application/json",origin,...headers}},Buffer.from(method==="GET"?"":body));
  assert.equal((await raw({origin:"https://attacker.example"})).status,403);
  assert.equal((await raw({"x-store-id":"client-authority"})).status,403);
  assert.equal((await raw({authorization:"Bearer forged"})).status,403);
  assert((await raw({},'{"x":1,"x":2}')).status>=400);
  assert((await raw({cookie:"__Host-commerce_buyer=forged"},undefined,"session","GET")).status>=400);
  assert(hits.every(x=>x>5));
  pass("cross-origin, private authority injection, malformed/forged inputs rejected");

  // Render the actual approved B route, not the transport-only fixture page.
  // This proves catalog -> preserved cart -> current quote, not hosted payment.
  const productID=cat.body.items.find(item=>item.sku_id===process.env.LC_BUYER_SKU).product_id;
  const uiContext=await browser.newContext(ctxOpts({ignoreHTTPSErrors:true,viewport:{width:390,height:780},deviceScaleFactor:887/390}));
  const ui=await uiContext.newPage();
  await ui.goto(`${origin}/zh-TW/products/${productID}`);
  await expect(ui.getByRole("heading",{name:"帆布收納袋（兩入組）"})).toBeVisible();
  await expect(ui.getByRole("radio").first()).toBeEnabled();
  await expect(ui.getByText("示意資料 · 測試環境",{exact:true})).toBeVisible();
  assert.equal(await ui.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  // F7: run output never rewrites the tracked .impeccable/review baselines.
  await mkdir(path.join(root,"output/playwright/review/buyer-inline"),{recursive:true});
  await ui.screenshot({path:path.join(root,"output/playwright/review/buyer-inline/hero-repro.png")});
  await ui.screenshot({path:path.join(evidence,"buyer-mobile.png"),fullPage:true});
  await ui.getByRole("button",{name:"增加數量",exact:true}).click();
  await expect(ui.getByRole("spinbutton")).toHaveValue("2");
  await ui.getByRole("combobox",{name:"語言",exact:true}).selectOption("en");
  await ui.waitForURL(`**/en/products/${productID}`);
  await expect(ui.getByRole("spinbutton")).toHaveValue("2");
  await ui.getByRole("button",{name:"Choose delivery",exact:true}).click();
  await expect(ui.getByRole("heading",{name:"Delivery and quotation",exact:true})).toBeVisible();
  await ui.getByRole("button",{name:"Get current total",exact:true}).click();
  await expect(ui.getByRole("heading",{name:"Items in this quotation",exact:true})).toBeVisible();
  await expect(ui.getByText("No payment has been taken.",{exact:false})).toBeVisible();
  await expect(ui.getByRole("button",{name:"View quotation",exact:true})).toBeEnabled();
  await expect(ui.getByText("Not paid",{exact:true})).toBeVisible();
  await ui.reload();
  await expect(ui.getByRole("heading",{name:"Items in this quotation",exact:true})).toBeVisible();
  await expect(ui.getByRole("button",{name:"View quotation",exact:true})).toBeEnabled();
  await expect(ui.getByText("Not paid",{exact:true})).toBeVisible();
  await expect(ui.getByRole("button",{name:"Choose delivery",exact:true})).toHaveCount(0);
  const uiState=await ui.evaluate(async()=>{const r=await fetch("/api/buyer/session");return r.json();});
  const currentCart=await ui.evaluate(async ctx=>{const r=await fetch("/api/buyer/cart",{headers:{"X-Buyer-Context":ctx}});return r.json();},uiState.context);
  assert.equal(currentCart.version,1);assert.equal(currentCart.items[0].quantity,2);
  await ui.setViewportSize({width:1440,height:900});
  assert.equal(await ui.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  await ui.screenshot({path:path.join(evidence,"buyer-desktop.png"),fullPage:true});
  await ui.getByRole("combobox",{name:"Language",exact:true}).selectOption("zh-CN");
  await ui.waitForURL(`**/zh-CN/products/${productID}`);
  await expect(ui.getByRole("spinbutton")).toHaveValue("2");
  await expect(ui.getByRole("heading",{name:"本次报价商品",exact:true})).toBeVisible();
  // Actual cart commit with every response dropped leaves the original key.
  // A second tab changes the session; recovery must adopt it, not reset it or
  // restore the old act.finally pending flag and deadlock the page.
  await ui.getByRole("spinbutton").fill("3");
  const lostCart={path:"/api/buyer/cart",drop:true,repeat:true,entered:deferred(),release:deferred(),headers:deferred(),result:deferred()};hook=lostCart;
  await ui.getByRole("button",{name:"选择配送",exact:true}).click();
  await expect(ui.getByRole("button",{name:"恢复上一笔请求",exact:true})).toBeVisible();
  hook=null;assert.equal(await lostCart.result.promise,200);
  const otherTab=await page(uiContext), oldUI=await state(otherTab);
  await otherTab.evaluate(ctx=>window.buyer.resetBuyerSession(ctx),oldUI.context);
  const newUI=await init(otherTab);assert.notEqual(newUI.context,oldUI.context);
  await expect(ui.getByRole("button",{name:"更新购物会话",exact:true})).toBeVisible();
  await ui.getByRole("button",{name:"更新购物会话",exact:true}).click();
  await expect(ui.getByRole("radio").first()).toBeEnabled();
  await expect(ui.getByRole("button",{name:"选择配送",exact:true})).toBeEnabled();
  assert.equal((await state(otherTab)).context,newUI.context);
  await expect(ui.getByRole("spinbutton")).toHaveValue("1");
  pass("actual lost cart response plus second-tab reset recovers new session without revocation or stale pending deadlock");
  await ui.goto(`${origin}/xx/products/${productID}`);assert.equal((await ui.request.get(`${origin}/xx/products/${productID}`)).status(),404);
  await uiContext.close();
  pass("approved B real product route; mobile/desktop, three locales, preserved quantity, cart and recovered quote; no payment claim");
  await writeFile(path.join(evidence,"result.json"),JSON.stringify({cases,order_id:orderID,next_instances:2,edge:"synthetic TLS/CONNECT; not deployment proof"},null,2));
} finally {
  if(browser) await browser.close();
  for(const child of children) { if(child.exitCode===null) {child.kill("SIGTERM");await Promise.race([once(child,"exit"),wait(3000)]);if(child.exitCode===null) child.kill("SIGKILL");} }
  for(const socket of sockets) socket.destroy();
  for(const server of [proxy,edge]) if(server) {server.closeAllConnections();await new Promise(r=>server.close(r));}
  for(const log of logs) log.end();
  await rm(certDir,{recursive:true}); // exact mkdtemp-owned disposable cert only
}
