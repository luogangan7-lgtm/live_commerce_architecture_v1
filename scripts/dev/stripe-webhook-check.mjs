// Independent Node verifier for Stripe webhook signature vectors (contracts/stripe-psp-v1.md §5.8, SP05).
// Non-goals: no network, no Stripe SDK, no real secrets; strict JSON (duplicate keys, depth) stays Go-only.
// Dependencies: node:crypto only, so the Go verifier is cross-checked by a second implementation.
// Callers: `node scripts/dev/stripe-webhook-check.mjs tests/payments/stripe-webhook-vectors.json`
// and TestStripeSP05Webhook (when node is on PATH). Exit 0 only if every vector agrees.
import crypto from 'node:crypto';
import { readFileSync } from 'node:fs';

const TOLERANCE = 300;          // fixed, both directions
const MAX_HEADER = 2048;
const MAX_ELEMENTS = 16;
const MAX_T = 2 ** 40;

// Returns true when the header authenticates body under any secret at `now`.
function signatureValid(secrets, header, body, now) {
  if (typeof header !== 'string' || header.length === 0 || Buffer.byteLength(header) > MAX_HEADER) return false;
  const parts = header.split(',');
  if (parts.length > MAX_ELEMENTS) return false;
  let t = null;
  const v1 = [];
  for (const part of parts) {
    const i = part.indexOf('=');
    if (i <= 0) return false;
    const k = part.slice(0, i);
    const v = part.slice(i + 1);
    if (k === 't') {
      if (t !== null || !/^[1-9][0-9]{0,12}$/.test(v)) return false;
      const n = Number(v);
      if (n < 1 || n > MAX_T) return false;
      t = n;
    } else if (k === 'v1') {
      if (!/^[0-9a-f]{64}$/.test(v)) return false;
      v1.push(Buffer.from(v, 'hex'));
    }
  }
  if (t === null || v1.length === 0) return false;
  if (Math.abs(now - t) > TOLERANCE) return false;
  for (const secret of secrets) {
    const mac = crypto.createHmac('sha256', secret).update(Buffer.concat([Buffer.from(`${t}.`), body])).digest();
    if (v1.some((s) => s.length === mac.length && crypto.timingSafeEqual(s, mac))) return true;
  }
  return false;
}

const path = process.argv[2];
if (!path) {
  console.error('usage: node scripts/dev/stripe-webhook-check.mjs <vectors.json>');
  process.exit(2);
}
const { vectors } = JSON.parse(readFileSync(path, 'utf8'));
if (!Array.isArray(vectors) || vectors.length < 20) {
  console.error('stripe-webhook-check: too few vectors');
  process.exit(1);
}
const counts = { ok: 0, malformed: 0, signature: 0 };
let failures = 0;
const names = new Set();
for (const v of vectors) {
  if (names.has(v.name)) { console.error(`duplicate vector name ${v.name}`); failures++; }
  names.add(v.name);
  const body = v.body_b64 !== undefined ? Buffer.from(v.body_b64, 'base64') : Buffer.from(v.body, 'utf8');
  const valid = signatureValid(v.secrets, v.header, body, v.now);
  const want = v.expect === 'signature' ? false : true;
  if (!(v.expect in counts) || valid !== want) {
    console.error(`FAIL ${v.name}: signature valid=${valid}, expect=${v.expect}`);
    failures++;
    continue;
  }
  if (v.expect === 'ok') {
    const root = JSON.parse(body.toString('utf8'));
    const e = v.event;
    if (!e || root.object !== 'event' || root.id !== e.id || root.type !== e.type ||
        root.livemode !== e.livemode || (root.account !== undefined) !== e.account_present ||
        root.data.object.id !== e.session_id ||
        // refund/charge vectors (stripe-refund-v1 §3 projection): only checked where the vector states them
        (e.payment_intent_id !== undefined && (root.data.object.payment_intent ?? '') !== e.payment_intent_id) ||
        (e.metadata_refund !== undefined && ((root.data.object.metadata ?? {}).lc_refund ?? '') !== e.metadata_refund)) {
      console.error(`FAIL ${v.name}: projection mismatch`);
      failures++;
      continue;
    }
  }
  counts[v.expect]++;
}
for (const k of Object.keys(counts)) {
  if (counts[k] === 0) { console.error(`no vectors of kind ${k}`); failures++; }
}
if (failures > 0) {
  console.error(`stripe-webhook-check: ${failures} failure(s)`);
  process.exit(1);
}
console.log(`stripe-webhook-check: ${vectors.length} vectors agree (ok=${counts.ok} malformed=${counts.malformed} signature=${counts.signature})`);
