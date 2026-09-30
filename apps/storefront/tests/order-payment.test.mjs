import test, { mock } from "node:test";
import assert from "node:assert/strict";
import {payOrder,pendingOrderPayment,readOrderPayment,openPaymentDestination,requestPaymentSignal} from "../lib/order-payment.ts";

const id=n=>`00000000-0000-0000-0000-${String(n).padStart(12,"0")}`;
const context="a".repeat(43),orderID=id(6),expiry=new Date(Date.now()+3600000).toISOString();
const method={code:"payuni_credit",version:1,name_hans:"测试",name_hant:"測試",name_en:"Mock"};
const form={action:"https://sandbox-api.payuni.com.tw/api/upp",fields:{Version:"2.0",MerID:"synthetic_merchant",EncryptInfo:"ab".repeat(8),HashInfo:"A".repeat(64)}};
const fresh={order_id:orderID,currency:"TWD",total_minor:2500,commercial_state:"DRAFT",test_mode:true,payment_state:"NOT_STARTED",handoff_state:"NONE",handoff_expires_at:null,methods:[method]};
const pending={...fresh,commercial_state:"AWAITING_PAYMENT",payment_state:"PENDING",handoff_state:"PREPARED",handoff_expires_at:expiry,methods:[]};
const order={order_id:orderID,cart_id:id(1),cart_version:2,commercial_state:"DRAFT",fulfillment_state:"MANUAL_UNASSIGNED",hold_expires_at:expiry,snapshot:{quote:{currency:"TWD",lines:[{sku_id:id(2),name:"Synthetic item",code:"SYNTH",quantity:1,unit_price_minor:2500}],amount:{subtotal_minor:2500,discount_minor:0,shipping_minor:0,shipping_tax_minor:0,tax_minor:0,total_minor:2500}},destination:{kind:"home",country:"TW",recipient_name:"Synthetic Recipient",phone:"+886900000001",home_address:{region:"",city:"Synthetic City",postal_code:"",line1:"Synthetic Street",line2:""}},service:{code:"home",name_hans:"测试",name_hant:"測試",name_en:"Synthetic",delivery_kind:"home",mode:"MANUAL"}}};

async function fixture(run){
  const old={fetch:globalThis.fetch,storage:globalThis.localStorage,window:globalThis.window,locks:Object.getOwnPropertyDescriptor(navigator,"locks")};
  const data=new Map(),writes=[],calls=[],locks=[];
  let failWrite=false,active=context,view=fresh,route=async req=>{
    if(req.path===`orders/${orderID}/payment`&&req.method==="GET")return Response.json(view);
    if(req.path.endsWith("/prepare")){view=pending;return Response.json({order_id:orderID,state:"PAYMENT_PENDING",currency:"TWD",amount_minor:2500});}
    if(req.path.endsWith("/handoff"))return Response.json({order_id:orderID,disposition:"ISSUED",expires_at:expiry,form});
    throw Error("unexpected request");
  };
  const store={getItem:k=>data.get(k)??null,setItem:(k,v)=>{if(failWrite)throw Error("synthetic storage denial");writes.push([k,String(v)]);data.set(k,String(v));},removeItem:k=>data.delete(k)};
  globalThis.localStorage=store;globalThis.window={localStorage:store};
  const tails=new Map();Object.defineProperty(navigator,"locks",{configurable:true,value:{request(name,...args){locks.push(name);const next=(tails.get(name)??Promise.resolve()).then(args.at(-1));tails.set(name,next.catch(()=>{}));return next;}}});
  globalThis.fetch=async(url,init)=>{
    if(url.endsWith("/session"))return Response.json({state:"active",context:active,expires_at:expiry});
    const req={path:url.replace("/api/buyer/",""),method:init.method,key:new Headers(init.headers).get("Idempotency-Key"),body:init.body?JSON.parse(init.body):undefined};
    calls.push(req);return route(req);
  };
  const submitted=[],navigated=[];let ready=true,closed=0;
  const destination={ready:()=>ready,submit:x=>submitted.push(x),navigate:x=>navigated.push(x),close:()=>{closed++;}};
  const gate={calls,writes,data,locks,submitted,navigated,destination,get closed(){return closed;},get view(){return view;},set view(x){view=x;},get route(){return route;},set route(x){route=x;},set active(x){active=x;},set ready(x){ready=x;},set failWrite(x){failWrite=x;},pay:(extra={})=>payOrder({context,order,locale:"en",method,destination,isCurrent:()=>true,...extra})};
  try{return await run(gate);}finally{globalThis.fetch=old.fetch;globalThis.localStorage=old.storage;globalThis.window=old.window;if(old.locks)Object.defineProperty(navigator,"locks",old.locks);else delete navigator.locks;}
}
const posts=(gate,suffix)=>gate.calls.filter(x=>x.path===`orders/${orderID}/payment/${suffix}`&&x.method==="POST");

test("BPU01 fresh payment writes exact metadata before prepare, one bodyless Take, no persistent form",async()=>fixture(async g=>{
  await g.pay();
  const marker=pendingOrderPayment(context,orderID);
  assert.deepEqual(Object.keys(marker).sort(),["body","context","key","order_id","stage","v"]);
  assert.deepEqual(marker.body,{method_code:"payuni_credit",method_version:1,locale:"en"});assert.equal(marker.stage,"handoff_started");
  assert.equal(posts(g,"prepare").length,1);assert.equal(posts(g,"handoff").length,1);
  assert.deepEqual(posts(g,"prepare")[0].body,marker.body);assert.equal(posts(g,"prepare")[0].key,marker.key);
  assert.equal(posts(g,"handoff")[0].body,undefined);assert.equal(posts(g,"handoff")[0].key,null);
  assert.deepEqual(g.submitted,[form]);assert(g.locks.includes("commerce-purchase-write-v1"));
  assert.deepEqual(g.writes.map(([,value])=>JSON.parse(value).stage),["prepare","handoff_started"]);
  assert(!JSON.stringify(g.writes).includes("EncryptInfo"));assert(!JSON.stringify(g.writes).includes("Synthetic Recipient"));
  await assert.rejects(g.pay(),{code:"uncertain"});assert.equal(posts(g,"handoff").length,1);
}));

test("BPU01 lost prepare response replays original key/body only on explicit next click",async()=>fixture(async g=>{
  let count=0;const normal=g.route;
  g.route=async req=>{if(req.path.endsWith("/prepare")&&++count===1){g.view=pending;throw Error("synthetic response loss");}return normal(req);};
  await assert.rejects(g.pay(),{code:"uncertain"});const first=pendingOrderPayment(context,orderID);assert.equal(first.stage,"prepare");
  assert.equal(posts(g,"handoff").length,0);assert.equal(g.submitted.length,0);
  assert.equal((await readOrderPayment(context,orderID)).payment_state,"PENDING");assert.equal(posts(g,"prepare").length,1);
  await g.pay({locale:"zh-TW",method:undefined});
  assert.equal(posts(g,"prepare").length,2);assert.equal(posts(g,"prepare")[0].key,posts(g,"prepare")[1].key);
  assert.deepEqual(posts(g,"prepare")[0].body,posts(g,"prepare")[1].body);assert.equal(posts(g,"prepare")[1].body.locale,"en");
  assert.equal(posts(g,"handoff").length,1);assert.equal(g.submitted.length,1);
}));

test("BPU01 lost handoff is permanently GET-only, even after a second click",async()=>fixture(async g=>{
  const normal=g.route;g.route=req=>req.path.endsWith("/handoff")?Promise.reject(Error("synthetic lost handoff")):normal(req);
  await assert.rejects(g.pay(),{code:"uncertain"});assert.equal(pendingOrderPayment(context,orderID).stage,"handoff_started");
  assert.equal(posts(g,"handoff").length,1);assert.equal(g.submitted.length,0);
  await readOrderPayment(context,orderID);await assert.rejects(g.pay(),{code:"uncertain"});
  assert.equal(posts(g,"prepare").length,1);assert.equal(posts(g,"handoff").length,1);assert.equal(g.submitted.length,0);
}));

test("BPU01 storage, amount, scope and child/screen fences reject before irreversible writes",async()=>{
  for(const change of [
    g=>{g.failWrite=true;},
    g=>{g.view={...fresh,total_minor:2501};},
    g=>{g.active="b".repeat(43);},
    g=>{g.ready=false;},
    g=>{g.data.set(`commerce-order-payment-v1:${context}:${orderID}`,"{}");},
  ])await fixture(async g=>{change(g);await assert.rejects(g.pay());assert.equal(posts(g,"prepare").length,0);assert.equal(posts(g,"handoff").length,0);});
  await fixture(async g=>{await assert.rejects(g.pay({isCurrent:()=>false}),{code:"uncertain"});assert.equal(posts(g,"prepare").length,0);});
});

test("BPU01 document/child loss after prepare but before Take leaves exact replay marker",async()=>fixture(async g=>{
  const normal=g.route;g.route=async req=>{const result=await normal(req);if(req.path.endsWith("/prepare"))g.ready=false;return result;};
  await assert.rejects(g.pay(),{code:"uncertain"});assert.equal(pendingOrderPayment(context,orderID).stage,"prepare");
  assert.equal(posts(g,"prepare").length,1);assert.equal(posts(g,"handoff").length,0);assert.equal(g.submitted.length,0);
}));

test("BPU01 malformed server handoff never submits and Take remains nonretryable",async()=>fixture(async g=>{
  const normal=g.route;g.route=req=>req.path.endsWith("/handoff")?Response.json({order_id:orderID,disposition:"ISSUED",expires_at:expiry,form:{...form,action:"https://evil.example/charge"}}):normal(req);
  await assert.rejects(g.pay(),{code:"uncertain"});assert.equal(g.submitted.length,0);
  assert.equal(pendingOrderPayment(context,orderID).stage,"handoff_started");await assert.rejects(g.pay());assert.equal(posts(g,"handoff").length,1);
}));

test("BPU01 marker replacement before Take and after Take blocks release/form reuse",async()=>{
  await fixture(async g=>{
    const original=localStorage.getItem.bind(localStorage);let changed=false;
    localStorage.getItem=key=>{
      const value=original(key);
      if(!changed&&key.startsWith("commerce-order-payment-v1:")&&value?.includes('"handoff_started"')){
        changed=true;g.data.set(key,JSON.stringify({...JSON.parse(value),key:id(99)}));
      }
      return value;
    };
    await assert.rejects(g.pay(),{code:"uncertain"});assert(changed);
    assert.equal(posts(g,"prepare").length,1);assert.equal(posts(g,"handoff").length,0);assert.equal(g.submitted.length,0);
  });
  await fixture(async g=>{
    const normal=g.route;g.route=async req=>{
      const response=await normal(req);
      if(req.path.endsWith("/handoff")){
        const key=`commerce-order-payment-v1:${context}:${orderID}`;
        g.data.set(key,JSON.stringify({...JSON.parse(g.data.get(key)),key:id(98)}));
      }
      return response;
    };
    await assert.rejects(g.pay(),{code:"uncertain"});
    assert.equal(posts(g,"handoff").length,1);assert.equal(g.submitted.length,0);
  });
});

test("BPU01 context replacement after prepare and after Take closes destination without form",async()=>{
  await fixture(async g=>{
    const normal=g.route;g.route=async req=>{const response=await normal(req);if(req.path.endsWith("/prepare"))g.active="b".repeat(43);return response;};
    await assert.rejects(g.pay(),{code:"context_changed"});assert.equal(posts(g,"prepare").length,1);
    assert.equal(posts(g,"handoff").length,0);assert.equal(g.submitted.length,0);assert(g.closed>0);
  });
  await fixture(async g=>{
    const normal=g.route;g.route=async req=>{const response=await normal(req);if(req.path.endsWith("/handoff"))g.active="b".repeat(43);return response;};
    await assert.rejects(g.pay(),{code:"context_changed"});assert.equal(posts(g,"handoff").length,1);
    assert.equal(g.submitted.length,0);assert(g.closed>0);
  });
});

test("BPU01 synchronous destination refuses invalid locale before opening a window",()=>{
  assert.throws(()=>openPaymentDestination("fr"),{code:"request_failed"});
});

// ---- SU03 (author-side): Stripe controller, stripe-buyer-ui-v1 §4. Independent gates live elsewhere.
const stripeMethod={...method,code:"stripe_checkout",name_en:"Card"};
const stripeFresh={...fresh,methods:[stripeMethod]};
const attemptView=(handoff_state,extra={})=>({...fresh,methods:[],commercial_state:"AWAITING_PAYMENT",payment_state:"PENDING",handoff_state,handoff_expires_at:expiry,cancel_requested:false,...extra});
const checkoutURL="https://checkout.stripe.com/c/pay/cs_test_a1B2#frag";
const redirect={order_id:orderID,disposition:"REDIRECT",expires_at:expiry,redirect_url:checkoutURL};
const stripePrepared={order_id:orderID,state:"PAYMENT_PENDING",currency:"TWD",amount_minor:2500};
// Stripe server: prepare moves the view to CREATING; `after` = view once prepared.
function stripeServer(g,{after=attemptView("READY"),handoff=redirect}={}){
  g.view=stripeFresh;
  g.route=async req=>{
    if(req.path===`orders/${orderID}/payment`&&req.method==="GET")return Response.json(g.view);
    if(req.path.endsWith("/prepare")){g.view=after;return Response.json(stripePrepared);}
    if(req.path.endsWith("/handoff"))return handoff instanceof Function?handoff(req):Response.json(handoff);
    throw Error("unexpected request "+req.path);
  };
}
const stripePay=g=>g.pay({method:stripeMethod});
const gets=g=>g.calls.filter(x=>x.path===`orders/${orderID}/payment`&&x.method==="GET").length;

test("SU03 fresh Stripe pay: marker before prepare, one prepare, one handoff, navigate, URL never stored",async()=>fixture(async g=>{
  stripeServer(g);
  assert.equal(await stripePay(g),"redirected");
  const marker=pendingOrderPayment(context,orderID);
  assert.deepEqual(marker.body,{method_code:"stripe_checkout",method_version:1,locale:"en"});
  assert.equal(marker.stage,"prepare","Stripe never writes handoff_started");
  assert.deepEqual(g.writes.map(([,v])=>JSON.parse(v).stage),["prepare"]);
  assert.equal(posts(g,"prepare").length,1);assert.equal(posts(g,"handoff").length,1);
  assert.deepEqual(posts(g,"prepare")[0].body,marker.body);assert.equal(posts(g,"prepare")[0].key,marker.key);
  assert.equal(posts(g,"handoff")[0].body,undefined);assert.equal(posts(g,"handoff")[0].key,null);
  assert.deepEqual(g.navigated,[checkoutURL]);assert.equal(g.submitted.length,0);
  assert.equal(JSON.stringify([...g.data]).includes("checkout.stripe.com"),false);
  assert.equal(JSON.stringify(g.writes).includes("cs_test"),false);
  assert.equal(g.locks.filter(x=>x==="commerce-purchase-write-v1").length,2,"lock released after prepare and re-taken for the handoff POST");
}));

test("SU03 marker replay: lost prepare response replays key/body, no uncertain on PENDING view with leftover marker",async()=>{
  await fixture(async g=>{
    stripeServer(g);let count=0;const normal=g.route;
    g.route=async req=>{if(req.path.endsWith("/prepare")&&++count===1){g.view=attemptView("CREATING");throw Error("synthetic response loss");}return normal(req);};
    await assert.rejects(stripePay(g));assert.equal(pendingOrderPayment(context,orderID).stage,"prepare");
    assert.equal(posts(g,"handoff").length,0);
    g.view=attemptView("READY");
    await g.pay({method:undefined});
    assert.equal(posts(g,"prepare").length,1,"view is PENDING now: prepare is skipped even though a marker exists");
    assert.equal(posts(g,"handoff").length,1);assert.equal(g.navigated.length,1);
  });
  await fixture(async g=>{
    stripeServer(g);let count=0;const normal=g.route;
    g.route=async req=>{if(req.path.endsWith("/prepare")&&++count===1)throw Error("synthetic loss before commit");return normal(req);};
    await assert.rejects(stripePay(g));const first=pendingOrderPayment(context,orderID);
    await g.pay({method:undefined,locale:"zh-TW"});
    assert.equal(posts(g,"prepare").length,2);assert.equal(posts(g,"prepare")[1].key,first.key);
    assert.deepEqual(posts(g,"prepare")[1].body,first.body);assert.equal(g.navigated.length,1);
  });
});

test("SU03 cross-device Continue needs no marker and never prepares",async()=>fixture(async g=>{
  stripeServer(g);g.view=attemptView("READY");
  assert.equal(await g.pay({method:undefined}),"redirected");
  assert.equal(posts(g,"prepare").length,0);assert.equal(posts(g,"handoff").length,1);
  assert.equal(pendingOrderPayment(context,orderID),null);assert.deepEqual(g.navigated,[checkoutURL]);
}));

test("SU03 marker/view matrix: PAYUNi marker on Stripe view and Stripe marker on PAYUNi view send nothing",async()=>{
  const key=`commerce-order-payment-v1:${context}:${orderID}`;
  const marker=code=>JSON.stringify({v:1,context,order_id:orderID,key:id(77),body:{method_code:code,method_version:1,locale:"en"},stage:"prepare"});
  for(const [markerCode,view] of [["payuni_credit",attemptView("READY")],["payuni_credit",stripeFresh],["stripe_checkout",pending],["stripe_checkout",fresh]])
    await fixture(async g=>{
      stripeServer(g);g.view=view;g.data.set(key,marker(markerCode));
      await assert.rejects(g.pay(),{code:"request_failed"});
      assert.equal(posts(g,"prepare").length,0);assert.equal(posts(g,"handoff").length,0);assert(g.closed>0);
    });
  await fixture(async g=>{
    stripeServer(g);g.view=attemptView("READY");g.data.set(key,JSON.stringify({...JSON.parse(marker("stripe_checkout")),stage:"handoff_started"}));
    await assert.rejects(g.pay(),{code:"request_failed"});assert.equal(posts(g,"handoff").length,0);
  });
});

test("SU03 any other view renders without a POST",async()=>{
  for(const view of [attemptView("READY",{cancel_requested:true}),attemptView("UNAVAILABLE"),attemptView("CLOSED"),{...attemptView("CLOSED"),payment_state:"CLOSED_UNPAID",commercial_state:"CANCELLED"},{...attemptView("CLOSED"),payment_state:"CAPTURED",commercial_state:"CONFIRMED"}])
    await fixture(async g=>{
      stripeServer(g);g.view=view;
      const outcome=await g.pay({method:undefined});
      assert.equal(outcome,"aborted");assert.equal(posts(g,"prepare").length,0);assert.equal(posts(g,"handoff").length,0);assert(g.closed>0);
    });
});

test("SU03 CREATING: waits on the schedule without the lock, then hands off once when READY",async()=>{
  mock.timers.enable({apis:["setTimeout"]});
  try{await fixture(async g=>{
    stripeServer(g,{after:attemptView("CREATING")});
    let n=0;const normal=g.route;
    g.route=req=>{if(req.path===`orders/${orderID}/payment`&&req.method==="GET"&&g.view.handoff_state==="CREATING"&&++n>=4)g.view=attemptView("READY");return normal(req);};
    const timers=[];const realSet=globalThis.setTimeout;
    let done=false;const p=stripePay(g).finally(()=>{done=true;});
    for(let i=0;i<200&&!done;i++){await new Promise(r=>setImmediate(r));mock.timers.tick(1000);}
    assert.equal(await p,"redirected");assert.equal(posts(g,"handoff").length,1);assert.equal(posts(g,"prepare").length,1);
    assert.equal(g.locks.filter(x=>x==="commerce-purchase-write-v1").length,2,"no purchase lock is held while polling; one for prepare, one for the POST");
  });}finally{mock.timers.reset();}
});

test("SU03 CREATING budget: 10 reads over 30 s, then the blank child is closed, no POST, no throw",async()=>{
  mock.timers.enable({apis:["setTimeout"]});
  try{await fixture(async g=>{
    stripeServer(g,{after:attemptView("CREATING")});
    let done=false,ticked=0;const p=stripePay(g).finally(()=>{done=true;});
    for(let i=0;i<200&&!done;i++){await new Promise(r=>setImmediate(r));mock.timers.tick(1000);ticked+=1000;}
    assert.equal(await p,"creating");
    assert.equal(posts(g,"handoff").length,0);assert(g.closed>0);
    assert.equal(gets(g),1+10,"1 in-lock read + 10 budget reads (1,1,2,2,3,3,5,5,8 s sleeps)");
    assert(ticked>=29000&&ticked<=31000,`budget spent ${ticked} ms`);
    assert.equal(pendingOrderPayment(context,orderID).stage,"prepare","marker stays so the button keeps working");
  });}finally{mock.timers.reset();}
});

test("SU03 cancel_requested seen on the re-GET aborts before the handoff POST",async()=>fixture(async g=>{
  stripeServer(g);g.view=attemptView("READY");let n=0;const normal=g.route;
  g.route=req=>{if(req.path===`orders/${orderID}/payment`&&req.method==="GET"&&++n===3)g.view=attemptView("READY",{cancel_requested:true});return normal(req);};
  assert.equal(await g.pay({method:undefined}),"aborted");
  assert.equal(posts(g,"handoff").length,0);assert(g.closed>0);assert.equal(g.navigated.length,0);
}));

test("SU03 handoff outcomes: CREATING closes child, CLOSED/UNAVAILABLE/error/malformed reject; one POST each, never retried",async()=>{
  for(const [name,handoff,expect] of [
    ["CREATING",{order_id:orderID,disposition:"CREATING",expires_at:expiry},"creating"],
    ["CLOSED",{order_id:orderID,disposition:"CLOSED",expires_at:expiry},{code:"unavailable"}],
    ["UNAVAILABLE",{order_id:orderID,disposition:"UNAVAILABLE",expires_at:expiry},{code:"unavailable"}],
    ["PAYUNi ISSUED on Stripe",{order_id:orderID,disposition:"ISSUED",expires_at:expiry,form},{code:"invalid_response"}],
    ["evil host",{...redirect,redirect_url:"https://checkout.stripe.com.evil.example/x"},{code:"invalid_response"}],
    ["http",{...redirect,redirect_url:"http://checkout.stripe.com/x"},{code:"invalid_response"}],
    ["503",()=>Response.json({code:"unavailable"},{status:503}),{code:"unavailable"}],
    ["network",()=>Promise.reject(Error("synthetic loss")),null],
  ])await fixture(async g=>{
    stripeServer(g,{handoff});g.view=attemptView("READY");
    if(expect==="creating")assert.equal(await g.pay({method:undefined}),"creating",name);
    else if(expect===null)await assert.rejects(g.pay({method:undefined}),name);
    else await assert.rejects(g.pay({method:undefined}),expect,name);
    assert.equal(posts(g,"handoff").length,1,name);assert.equal(g.navigated.length,0,name);assert(g.closed>0,name);
    assert.equal(pendingOrderPayment(context,orderID),null,name);
  });
});

test("SU03 Stripe prepare errors, amount/currency mismatch and popup loss stop before any handoff",async()=>{
  for(const [name,change]of[
    ["503",g=>{const n=g.route;g.route=req=>req.path.endsWith("/prepare")?Response.json({code:"unavailable"},{status:503}):n(req);}],
    ["amount",g=>{const n=g.route;g.route=req=>req.path.endsWith("/prepare")?Response.json({...stripePrepared,amount_minor:2600}):n(req);}],
    ["currency",g=>{const n=g.route;g.route=req=>req.path.endsWith("/prepare")?Response.json({...stripePrepared,currency:"USD"}):n(req);}],
    ["snapshot",g=>{g.view={...stripeFresh,total_minor:2501};}],
    ["child gone",g=>{g.ready=false;}],
    ["no method",g=>{}],
  ])await fixture(async g=>{
    stripeServer(g);change(g);
    await assert.rejects(name==="no method"?g.pay({method:undefined}):stripePay(g),undefined,name);
    assert.equal(posts(g,"handoff").length,0,name);assert.equal(g.navigated.length,0,name);
    if(name==="child gone"||name==="snapshot"||name==="no method")assert.equal(posts(g,"prepare").length,0,name);
  });
});

test("SU03 two clicks make two handoff POSTs only because there were two clicks; sequential, none from reads",async()=>fixture(async g=>{
  stripeServer(g);await stripePay(g);
  await readOrderPayment(context,orderID);await readOrderPayment(context,orderID);
  assert.equal(posts(g,"handoff").length,1);
  await g.pay({method:undefined});
  assert.equal(posts(g,"prepare").length,1,"second Continue skips prepare");assert.equal(posts(g,"handoff").length,2);
}));

function fakeChild(){
  const doc={URL:"about:blank",head:{appendChild(){}},body:{appendChild(){}},createElement:()=>({isConnected:true})};
  const state={replaced:[],closed:false};
  const child={get closed(){return state.closed;},opener:{},location:{href:"about:blank",replace(u){state.replaced.push(u);child.location.href=u;}},document:doc,close(){state.closed=true;}};
  return {child,state};
}
test("SU03 navigate: validates again, needs an owned blank child, sets used before replace, no URL in errors",async()=>fixture(async()=>{
  for(const bad of ["http://checkout.stripe.com/x","https://checkout.stripe.com.evil.example/x","https://checkout.stripe.com@evil.example/x","https://checkout.stripe.com:443/x","https://checkout.stripe.com/x y","https://checkout.stripe.com",{},null]){
    const {child,state}=fakeChild();globalThis.window.open=()=>child;
    const d=openPaymentDestination("en");
    assert.throws(()=>d.navigate(bad),e=>e.code==="invalid_response"&&!String(e.message).includes("evil"));
    assert.equal(state.replaced.length,0);assert.equal(d.ready(),true,"a rejected URL does not burn the child");
  }
  {const {child,state}=fakeChild();globalThis.window.open=()=>child;const d=openPaymentDestination("en");
    d.navigate(checkoutURL);assert.deepEqual(state.replaced,[checkoutURL]);assert.equal(d.ready(),false);
    assert.throws(()=>d.navigate(checkoutURL),{code:"invalid_response"});assert.equal(state.replaced.length,1);
    d.close();assert.equal(state.closed,false,"a navigated child is the provider's page: never closed");}
  {const {child,state}=fakeChild();globalThis.window.open=()=>child;const d=openPaymentDestination("en");
    state.closed=true;assert.throws(()=>d.navigate(checkoutURL),{code:"invalid_response"});assert.equal(state.replaced.length,0);}
  {const {child,state}=fakeChild();globalThis.window.open=()=>child;const d=openPaymentDestination("en");
    child.location.replace=()=>{throw Error("boom "+checkoutURL);};
    assert.throws(()=>d.navigate(checkoutURL),e=>e.code==="unavailable"&&!String(e.message).includes("checkout"));
    assert.equal(d.ready(),false,"a thrown navigation must not be retried");}
  globalThis.window.open=()=>null;
  assert.throws(()=>openPaymentDestination("en"),{code:"unavailable"});
}));

test("SU03 refresh signal: keyless single POST, validated, throttled 1/10 s and 30 per page life, cancel never throttled",async()=>fixture(async g=>{
  const oid=id(41);const sig=b=>Response.json(b);
  g.route=req=>req.path==`orders/${oid}/payment/refresh`||req.path==`orders/${oid}/payment/cancel`?sig({order_id:oid,scheduled:true}):Promise.reject(Error("unexpected"));
  mock.timers.enable({apis:["Date"],now:1_000_000});
  try{
    assert.deepEqual(await requestPaymentSignal(context,oid,"refresh"),{order_id:oid,scheduled:true});
    assert.deepEqual(await requestPaymentSignal(context,oid,"refresh"),{order_id:oid,scheduled:false});
    const sent=()=>g.calls.filter(x=>x.path.endsWith("/refresh")).length;
    assert.equal(sent(),1);assert.equal(g.calls[0].body,undefined);assert.equal(g.calls[0].key,null);
    mock.timers.tick(10_000);await requestPaymentSignal(context,oid,"refresh");assert.equal(sent(),2);
    for(let i=0;i<40;i++){mock.timers.tick(10_000);await requestPaymentSignal(context,oid,"refresh");}
    assert.equal(sent(),30,"page-life cap");
    await requestPaymentSignal(context,oid,"cancel");await requestPaymentSignal(context,oid,"cancel");
    assert.equal(g.calls.filter(x=>x.path.endsWith("/cancel")).length,2);
  }finally{mock.timers.reset();}
}));

test("SU03 signal failures are single-shot and sanitized: HTTP error, malformed body, wrong order",async()=>{
  for(const [name,response,code] of [
    ["503",()=>Response.json({code:"unavailable"},{status:503}),"request_failed"],
    ["429",()=>Response.json({code:"rate_limited"},{status:429}),"request_failed"],
    ["extra key",()=>Response.json({order_id:id(42),scheduled:true,x:1}),"invalid_response"],
    ["wrong order",()=>Response.json({order_id:id(43),scheduled:true}),"invalid_response"],
    ["not json",()=>new Response("<html>",{status:200}),"invalid_response"],
  ])await fixture(async g=>{
    g.route=response;
    await assert.rejects(requestPaymentSignal(context,id(42),"cancel"),{code},name);
    assert.equal(g.calls.length,1,name);
  });
});
