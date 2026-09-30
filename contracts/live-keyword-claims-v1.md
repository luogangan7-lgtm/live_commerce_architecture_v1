# Live keyword claims v1 — T10 comment → claim → prefilled cart

Status: FROZEN for T10 implementation (integrator 2026-09-28, cloud lane `claude/gallant-bohr-9rs3yo`, base 6bb5092 + c3878a4); NOT_PUBLIC; MOCK social ingress only until the Meta hookup gate (T10c) passes. See §0.1 for binding integrator rulings.

Synthesized from candidates A (base) and B and three lens reviews; every P1 is closed
below (§0). Contributes the T10 slice of R06 and the G03 case "重复评论和修改数量语义固定".
**G07: no contribution** (nothing is sent). Ingress in this slice is **MOCK/manual**:
an operator records what a buyer commented. Meta comment ingestion is follow-up T10c
(§11). No inventory hold, Quote, checkout, order, payment, message or provider effect.
Architecture §§10.1, 11.1–11.2, 14.1–14.2, 17.2, 21.4 and `cart-quote-v1`,
`buyer-capability-v1`, `buyer-http-v1`, `live-planning-v1`, `studio-v1`,
`meta-consumer-v1` remain authoritative and unchanged except the integrator helpers
named in §10. Evidence labels: this file is DESIGN; §9 gates produce UNIT, REAL_PG and
HTTP_PG evidence over MOCK ingress; provider SANDBOX/LIVE are NOT_APPLICABLE to this
slice; every gate is NOT_RUN until recorded.

## 0. Integrator decisions: review result

No P0 in D1–D9. Where the literal reading would be P1, this refinement is binding:

| # | Decision | Literal-reading risk | Adopted rule | Gate |
| --- | --- | --- | --- | --- |
| R1 | D6 set targets on sync | Re-adds lines the buyer removed; overwrites buyer edits | **Delta apply**: a claim line is written only when `version > applied_version` (§4.4) | KC11 |
| R2 | D6 GetCart→SetCart | GetCart `FOR SHARE` then SetCart `FOR UPDATE`: two same-owner txs deadlock; nested `cart.set` receipt lock unordered | `storefront.LockCartOwner` first; derived `cart.set` key prefix `clm:` reserved at buyerhttp (§5.6) | KC07, KC14 |
| R3 | D7 IssueLink | Token persisted in receipt JSON; admin BFF always sends Idempotency-Key | `command.Run` with a **token-free receipt**; token only in the first execution's response, written after COMMIT (§6) | KC09, KC13 |
| R4 | D7 first owner binds | In-app browser vs Safari, leaked or mis-sent link → permanent 404 | Release only inside the `claims.issue_link` definer, which re-resolves `live:manage` in the DB and swaps the token hash in the same call; resets applied state (§3.3, §6) | KC03, KC09, KC11 |
| R5 | D5 persist parse result | Keyword-shaped chat (`0912345678`) stored in plaintext on rejection | Persist `offer_id` only when the head resolves to an offer; never an unresolved keyword (§3.1) | KC06, KC08 |
| R6 | D5 duplicate no-op | Dedup over mutable offer resolution turns a redelivery into 409 | Dedup compares immutable delivery/parse facts only (§4.3) | KC06 |
| R7 | D8 ingest scope | Store-wide OPEN lookup races "close S1, open S2" → wrong keyword map | `IngestInput.SessionID`; lock that session's OPEN window (§4.3) | KC07 |
| R8 | D8 manual ingress | Hashing typed display names merges two commenters | Manual actor = server-random key + merchant label, unique per session (§4.2) | KC08 |
| R9 | D7 redeem | Applies lines the buyer never previewed | `expected_bundle_version` CAS (§4.4) | KC11 |
| R10 | D2 store-wide window | Cannot tell a live-video comment from any other post | Safe for operator-attested manual entry; **T10c blocker** (§11.2) | — |

D8 decision: the Meta consumer does **not** call `Ingest` in this slice (§11.2). Adopted
review P2s: pure `internal/claims/grammar` + `IngestParsed` for T10c; keyset on
immutable `(created_at,id)`; advisory offer cap (no `live.sessions` row lock);
occurred-at skew 120 s; authorize again after `command.Run`; json tags on every result;
redacted formatting on every input/credential type; token only in a header; retention
classes without fixed days; UI BLOCKED pending composition approval (T10b).

### 0.1 Integrator rulings (2026-09-28, binding for implementation)

- **Migration number 0060**, not 0044: 0044–0059 are left to parallel local lanes (BRW07 etc.).
  The integrator renumbers at merge if needed. Every "0044" in this file means 0060.
- Open questions resolved: Meta ingestion deferred to T10c (manual MOCK ingress only, R10);
  integrator-owned helpers in §10 approved (`storefront.LockCartOwner`, pagination collection
  `claim-bundles`, `internal/httpapi` + `internal/buyerhttp` routes, tests in
  `tests/foundation/live_claims*_test.go` + pure vectors under `tests/claims/`); delta apply
  (R1) adopted; retention classes as §8 with the purge job in T14 (production-mount blocker
  stays listed in §12); final quantity follows server acceptance order (§11.1 of 架构.md);
  link TTL fixed 72 h; kw-v1 strictness confirmed (`A1 +2`, `A1+02`, `A1+` never match);
  claim windows may open on a DRAFT session until T08 defines LIVE; keywords immutable;
  claims reuse `live:read`/`live:manage` in this slice.
- **T10b UI is UNBLOCKED** for this lane: the owner instructed on 2026-09-28 to build every
  page to a deployable state with browser tests. UI must reuse the approved Studio/admin and
  storefront design systems (DESIGN.md, `.impeccable/`); the composition is still recorded
  for owner visual review at delivery.
- Open adversarial P2s that implementation MUST close (each with a test):
  (a) permissive RLS UPDATE policies are OR-ed by PostgreSQL — do not rely on policy
  "pairing"; enforce buyer-vs-merchant writes by column grants + definer functions and test
  that a buyer tx cannot release/rotate and a merchant tx cannot bind;
  (b) manual actor labels are personal data — never let them reach PG server logs via
  constraint-violation DETAIL (validate in Go before SQL; map 23505/23514 without echo);
  (c) duplicate ingest must return a fully reconstructible result (persist or re-read
  `bundle_version`; no unstable fields);
  (d) `NormalizeLabel` strips `@` before trimming and is idempotent;
  (e) define `match_mode` behaviour on OPEN→CLOSED/CLOSED→OPEN (mode may change only while
  CLOSED; mismatching mode on transition = `ErrConflict`);
  (f) pending lines on unavailable SKUs: SetCart receives only applicable lines merged into
  the current cart; unavailable claim lines stay pending and are reported in the result;
  (g) link TTL/expiry enforced in the database (CHECK / definer), not only in Go.

## 1. Business boundary

- An **offer** binds one normalized keyword to one SKU inside one live session.
  Keyword and SKU are immutable; `max_quantity_per_claim` 1..999 and `active` change
  with version CAS. Keyword unique per session (active or not); at most one **active**
  offer per SKU per session, so a redeem has at most one applicable line per SKU (a cart
  merge never sums two claim lines; lines on inactive offers are skipped, §4.4). A keyword
  typo is fixed by deactivating the offer and creating the right keyword for the same SKU.
  ≤200 offers per session.
- A **claim window** per session is `CLOSED`/`OPEN` with `match_mode` `EXACT`
  (default) or `KEYWORD_QTY_ONLY`. At most one OPEN window per store. Mode changes only
  while CLOSED. Each opening increments `generation`.
- A **claim bundle** is one `(tenant, store, session, platform, actor_key)`;
  `actor_key` is opaque 64-hex, never a handle or sender id. A **claim line** is one
  `(bundle, offer)` with an absolute target quantity. ≤50 lines per bundle.
- A command is matched only if **this session's** window is OPEN at ingest and
  `opened_at <= occurred_at <= clock_timestamp()+120s`. `KEYWORD` alone = target 1
  (EXACT only); `KEYWORD+N` **sets** the target to N, never adds (arch §11.1). A later bare
  `A1` after `A1+3` sets it back to 1. Server commit order decides, not `occurred_at`.
- Comments and claims **never** touch inventory, reservations, carts, Quotes or orders.
  Only an explicit buyer POST applies claims to that buyer's own cart via
  `storefront.SetCart`; inventory is touched only by a future BeginCheckout (arch §11.2).
- A claim link is a 256-bit bearer capability for **one bundle's prefill only**. The
  first buyer owner that redeems binds the bundle; other owners get the uniform
  not-found. A forwarded link binds the forwardee (arch §14.2). It never unlocks orders,
  addresses, payment methods or the commenter's identity. Only a merchant can release
  a binding, and releasing always kills the old token (enforced inside the
  `claims.issue_link` definer, not only in Go; §3.3).
- Social actor ≠ verified customer. No identity edge, consent, message window or
  audience fact is created (arch §14.1/§14.4).

## 2. Grammar `kw-v1` (package `internal/claims/grammar`, pure, frozen)

`Parse`, `NormalizeKeyword` and `NormalizeLabel` are pure Go (stdlib `unicode/utf8`
only): no DB, clock, locale, regexp backtracking, logging or global state. The mapping is
an **explicit table, not full NFKC**: NFKC would also accept `①`, `²`, `ﬁ`, Roman
numerals and the Kelvin sign. D3 "NFKC-style" is satisfied by the full-width block only.
Parsing is **mode-independent**; the mode rule is applied at ingest (§2.3).

### 2.1 Normalization (in this order)

| Step | Input code points | Output |
| --- | --- | --- |
| 0 | `len(text) > 256` bytes or invalid UTF-8 | stop: `NO_MATCH` |
| 1 | U+FF01–U+FF5E (full-width ASCII incl. `０-９ Ａ-Ｚ ａ-ｚ ＋`) | code point − 0xFEE0 |
| 1 | U+3000 ideographic space, U+00A0 NBSP | U+0020 |
| 2 | leading/trailing U+0020, U+0009, U+000A, U+000D | removed (ends only) |
| 3 | ASCII `a`–`z` only | `A`–`Z` (never `unicode.ToUpper`: `ſ`→S, `ı`→I, U+212A→K traps) |
| – | everything else (U+FE62 ﹢, U+2795 ➕, U+207A ⁺, ①, ², ſ, ı, U+212A, U+200B, U+FEFF, emoji, CJK, half-width kana) | unchanged → fails syntax |

### 2.2 Syntax

Let `s` be the normalized string; split at the **first** `+` into `head`, `tail`.

1. `head` must match `^[A-Z0-9]{1,16}$` → else `NO_MATCH`.
2. No `+` → `MATCH`, quantity 1, `explicit=false`.
3. `tail` empty or containing any non-`[0-9]` byte (space, `+`, `-`, …) → `NO_MATCH`.
4. `tail` all digits but `len>3` or leading `0` → `INVALID_QUANTITY` (keyword = head;
   length checked before conversion, no overflow).
5. Otherwise `MATCH`, quantity 1..999, `explicit=true`.

Never substring/"contains" matching (SHOPLINE "contains" mode rejected), never a guessed
SKU, never negation/question interpretation. `NormalizeKeyword` applies steps 1–3 and
requires `^[A-Z0-9]{1,16}$` (no `+`); offers store only that canonical form.
`NormalizeLabel` (manual actor label): steps 1–2, collapse interior whitespace runs to
one U+0020, strip one leading `@`, ASCII-lowercase, then require 1..60 code points and no
Unicode control/format characters.

### 2.3 Ingest precedence (decided on locked rows, §4.3)

`WINDOW_CLOSED` (not persisted) → `NO_MATCH` → `UNKNOWN_KEYWORD` (head is not an offer
keyword of the session; includes `INVALID_QUANTITY` heads) → `OFFER_INACTIVE` (inactive,
or `occurred_at < activated_at`) → `INVALID_QUANTITY` → `QUANTITY_REQUIRED`
(`KEYWORD_QTY_ONLY` and `explicit=false`) → `QUANTITY_OVER_MAX` (`N > max`) →
`BUNDLE_LIMIT` (new line when `line_count=50`) → `ACCEPTED`.

### 2.4 Canonical vectors (`tests/claims/kw-v1-vectors.json`, KC01/KC06)

Parse (mode-independent). "→ K×N" = `MATCH` keyword K, quantity N.

| ID | Input | Parse |
| --- | --- | --- |
| G01 | `A1` / `a1` / `␠␠A1␠␠` | → A1×1 implicit |
| G02 | `A1+2` / `ａ１＋２` / `A1＋2` | → A1×2 explicit |
| G03 | `Ａ１` / `A１７` | → A1×1 / A17×1 |
| G04 | `A1+999` / `101` / `101+2` | → A1×999 / 101×1 / 101×2 |
| G05 | `A1　` / `\tA1+3\n` / ` A1` | → A1×1 / A1×3 / A1×1 |
| G06 | `ABCDEFGHIJKLMNOP` (16) / `0912345678` | → MATCH (shape only) |
| R01 | `不要A1` / `A1是不是红色` / `A1 是不是红色` | NO_MATCH |
| R02 | `A1 B2` / `A1,B2` / `A1、B2` / `A1 A1` / `A1\nA1` | NO_MATCH |
| R03 | `A1+0` / `Ａ１＋０` / `A1+1000` / `A1+02` / `A1+99999999999999999999` | INVALID_QUANTITY (keyword A1) |
| R04 | `A1+` / `A1+-1` / `A1-1` / `A1+2+3` / `A1++2` | NO_MATCH |
| R05 | `A1 +2` / `A1+ 2` / `A1+2 謝謝` / `A1+２件` | NO_MATCH |
| R06 | `+2` / `` / `␠␠␠` / `#A1` / `A1?` / `A1!!` / `A1👍` | NO_MATCH |
| R07 | 17 chars / 257-byte string starting `A1` / invalid UTF-8 | NO_MATCH |
| R08 | `ſ1` / `ı1` / `K1` (U+212A) / `A1​` / `﻿A1` / `①` / `A²` / `A1﹢2` / `A1➕2` / `A1⁺2` | NO_MATCH |
| R09 | `a1x2` | → A1X2×1 (UNKNOWN_KEYWORD unless offer `A1X2` exists; hazard, §12) |

Ingest (offers `A1` max 3 active, `B2` max 999 active; EXACT unless marked):

| ID | Command | Outcome / persisted fields |
| --- | --- | --- |
| I01 | `A1` | ACCEPTED target 1 |
| I02 | `A1+5` | QUANTITY_OVER_MAX; `offer_id`, quantity 5 |
| I03 | `Z9` / `0912345678` / `0912345678+0` | UNKNOWN_KEYWORD; **no keyword, quantity or offer stored** |
| I04 | `A1+0` | INVALID_QUANTITY; `offer_id`, quantity NULL |
| I05 | `A1` (KEYWORD_QTY_ONLY) / `A1+3` (KEYWORD_QTY_ONLY) | QUANTITY_REQUIRED / ACCEPTED 3 |
| I06 | `A1` with A1 inactive or before `activated_at` | OFFER_INACTIVE |
| I07 | `不要A1` | NO_MATCH; reason only |
| I08 | any, window of this session not OPEN | WINDOW_CLOSED; nothing persisted |
| S01 | same actor `A1` → `A1+3` → `A1+2` → `A1` | target 1 → 3 → 2 → 1; line version 4 |
| S02 | redeliver the `A1+3` source after S01 | prior ACCEPTED×3, `duplicate=true`, line stays 1, no write |
| S03 | `Z9` → CreateOffer `Z9` → redeliver same source | `duplicate=true`, UNKNOWN_KEYWORD, no new row |
| S04 | 51st distinct offer for one bundle | BUNDLE_LIMIT |
| S05 | new manual actor `A1` via `actor_label`, then `B2` via `bundle_id` with `actor_label` empty | both ACCEPTED on one bundle; label unchanged; no 23514 |

## 3. Data model — migration `0060_live_claims.sql` (integrator-owned)

Six tables, one new NOLOGIN role, four definer functions. Every table ENABLE + FORCE
RLS; `REVOKE ALL ... FROM PUBLIC`; no DELETE grant; no object owned by a login; no River
job kind; no change to `social`, `meta_inbox`, `storefront`, `inventory`, `buyer` or
`live.sessions/programs`. `S` below =
`tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND
store_id=nullif(current_setting('app.store_id',true),'')::uuid`.

```sql
CREATE ROLE commerce_claims_writer NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE SCHEMA claims; REVOKE ALL ON SCHEMA claims FROM PUBLIC;
GRANT USAGE ON SCHEMA claims TO commerce_runtime, commerce_buyer_runtime, commerce_claims_writer;
GRANT USAGE ON SCHEMA live TO commerce_claims_writer;

CREATE TABLE live.offers (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 session_id uuid NOT NULL,
 keyword text NOT NULL CHECK (keyword ~ '^[A-Z0-9]{1,16}$' AND octet_length(keyword)=char_length(keyword)),
 sku_id uuid NOT NULL,
 max_quantity_per_claim integer NOT NULL CHECK (max_quantity_per_claim BETWEEN 1 AND 999),
 active boolean NOT NULL DEFAULT true,
 activated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 version bigint NOT NULL DEFAULT 1 CHECK (version>0),
 principal_id uuid NOT NULL,                                      -- creator
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (tenant_id,store_id,session_id,keyword),
 UNIQUE (tenant_id,store_id,session_id,id),
 UNIQUE (tenant_id,store_id,session_id,id,sku_id),
 FOREIGN KEY (tenant_id,store_id,session_id) REFERENCES live.sessions(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,store_id,sku_id) REFERENCES catalog.skus(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id));
CREATE UNIQUE INDEX live_offer_active_sku ON live.offers(tenant_id,store_id,session_id,sku_id) WHERE active;

CREATE TABLE live.claim_windows (                                -- one row per session; FK anchor for claims
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, session_id uuid NOT NULL,
 state text NOT NULL CHECK (state IN ('CLOSED','OPEN')),
 match_mode text NOT NULL CHECK (match_mode IN ('EXACT','KEYWORD_QTY_ONLY')),
 generation bigint NOT NULL CHECK (generation>=0),
 opened_at timestamptz, closed_at timestamptz,
 version bigint NOT NULL DEFAULT 1 CHECK (version>0),
 principal_id uuid NOT NULL,                                      -- last changing principal
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,session_id),
 FOREIGN KEY (tenant_id,store_id,session_id) REFERENCES live.sessions(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK ((state='OPEN' AND generation>0 AND opened_at IS NOT NULL AND closed_at IS NULL)
     OR (state='CLOSED' AND ((generation=0 AND opened_at IS NULL AND closed_at IS NULL)
        OR (generation>0 AND opened_at IS NOT NULL AND closed_at IS NOT NULL AND closed_at>=opened_at)))));
CREATE UNIQUE INDEX live_claim_window_one_open ON live.claim_windows(tenant_id,store_id) WHERE state='OPEN';

CREATE TABLE claims.bundles (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 session_id uuid NOT NULL,
 platform text NOT NULL CHECK (platform='manual'),                -- T10c widens
 actor_key text NOT NULL CHECK (actor_key ~ '^[0-9a-f]{64}$'),    -- manual: server-random
 label text CHECK (char_length(label) BETWEEN 1 AND 60 AND label=btrim(label) AND label !~ '[[:cntrl:]]'),
 owner_id uuid, bound_at timestamptz,                             -- buyer binding (writer only)
 line_count integer NOT NULL DEFAULT 0 CHECK (line_count BETWEEN 0 AND 50),
 version bigint NOT NULL DEFAULT 0 CHECK (version>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (tenant_id,store_id,session_id,id),
 UNIQUE (tenant_id,store_id,session_id,platform,actor_key),
 FOREIGN KEY (tenant_id,store_id,session_id) REFERENCES live.claim_windows(tenant_id,store_id,session_id),
 FOREIGN KEY (tenant_id,store_id,owner_id) REFERENCES buyer.owners(tenant_id,store_id,id),
 CHECK ((owner_id IS NULL)=(bound_at IS NULL)),
 CHECK ((platform='manual')=(label IS NOT NULL)));
CREATE UNIQUE INDEX claims_bundle_label ON claims.bundles(tenant_id,store_id,session_id,label) WHERE label IS NOT NULL;
CREATE INDEX claims_bundle_page ON claims.bundles(tenant_id,store_id,session_id,created_at DESC,id DESC);

CREATE TABLE claims.lines (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, session_id uuid NOT NULL,
 bundle_id uuid NOT NULL, offer_id uuid NOT NULL, sku_id uuid NOT NULL,
 quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 999),
 version bigint NOT NULL CHECK (version>0),
 applied_version bigint CHECK (applied_version BETWEEN 1 AND version),  -- writer only
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,bundle_id,offer_id),
 FOREIGN KEY (tenant_id,store_id,session_id,bundle_id) REFERENCES claims.bundles(tenant_id,store_id,session_id,id),
 FOREIGN KEY (tenant_id,store_id,session_id,offer_id,sku_id) REFERENCES live.offers(tenant_id,store_id,session_id,id,sku_id));

CREATE TABLE claims.events (   -- append-only; never comment text, unresolved keyword, label or actor key
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 session_id uuid NOT NULL, window_generation bigint NOT NULL CHECK (window_generation>0),
 source_kind text NOT NULL CHECK (source_kind='manual'),          -- T10c adds 'meta'
 source_event_id uuid NOT NULL,
 platform text NOT NULL CHECK (platform='manual'),
 occurred_at timestamptz NOT NULL,
 grammar_version text NOT NULL CHECK (grammar_version='kw-v1'),
 grammar_kind text NOT NULL CHECK (grammar_kind IN ('MATCH','NO_MATCH','INVALID_QUANTITY')),
 match_mode text NOT NULL CHECK (match_mode IN ('EXACT','KEYWORD_QTY_ONLY')),
 outcome text NOT NULL CHECK (outcome IN ('ACCEPTED','REJECTED')),
 reason text CHECK (reason IN ('NO_MATCH','UNKNOWN_KEYWORD','OFFER_INACTIVE','INVALID_QUANTITY',
   'QUANTITY_REQUIRED','QUANTITY_OVER_MAX','BUNDLE_LIMIT')),
 offer_id uuid,
 quantity integer CHECK (quantity BETWEEN 1 AND 999),
 explicit_quantity boolean,
 bundle_id uuid, line_version bigint CHECK (line_version>0),
 previous_quantity integer CHECK (previous_quantity BETWEEN 1 AND 999),
 principal_id uuid,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (tenant_id,store_id,source_event_id),
 FOREIGN KEY (tenant_id,store_id,session_id) REFERENCES live.claim_windows(tenant_id,store_id,session_id),
 FOREIGN KEY (tenant_id,store_id,session_id,offer_id) REFERENCES live.offers(tenant_id,store_id,session_id,id),
 FOREIGN KEY (tenant_id,store_id,session_id,bundle_id) REFERENCES claims.bundles(tenant_id,store_id,session_id,id),
 FOREIGN KEY (tenant_id,store_id,bundle_id,offer_id) REFERENCES claims.lines(tenant_id,store_id,bundle_id,offer_id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK ((outcome='ACCEPTED')=(reason IS NULL)),
 CHECK ((outcome='ACCEPTED')=(bundle_id IS NOT NULL)),
 CHECK ((outcome='ACCEPTED')=(line_version IS NOT NULL)),
 CHECK (previous_quantity IS NULL OR outcome='ACCEPTED'),
 CHECK (coalesce(reason='NO_MATCH',false)=(grammar_kind='NO_MATCH')),
 CHECK ((offer_id IS NULL)=coalesce(reason IN ('NO_MATCH','UNKNOWN_KEYWORD'),false)),
 CHECK ((quantity IS NOT NULL)=(offer_id IS NOT NULL AND grammar_kind='MATCH')),
 CHECK ((explicit_quantity IS NOT NULL)=(quantity IS NOT NULL)),
 CHECK (outcome<>'ACCEPTED' OR quantity IS NOT NULL),
 CHECK (grammar_kind<>'INVALID_QUANTITY' OR coalesce(reason IN ('UNKNOWN_KEYWORD','OFFER_INACTIVE','INVALID_QUANTITY'),false)),
 CHECK (reason IS DISTINCT FROM 'INVALID_QUANTITY' OR grammar_kind='INVALID_QUANTITY'),
 CHECK (reason IS DISTINCT FROM 'QUANTITY_REQUIRED' OR (match_mode='KEYWORD_QTY_ONLY' AND explicit_quantity IS FALSE)),
 CHECK ((source_kind='manual')=(principal_id IS NOT NULL)),
 CHECK (occurred_at<=recorded_at+interval '120 seconds'));
CREATE UNIQUE INDEX claims_event_line_version ON claims.events(tenant_id,store_id,bundle_id,offer_id,line_version)
 WHERE outcome='ACCEPTED';
CREATE INDEX claims_event_stats ON claims.events(tenant_id,store_id,session_id,window_generation,outcome,reason);

CREATE TABLE claims.links (    -- credential table: no runtime role can SELECT token_hash
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, bundle_id uuid NOT NULL,
 token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
 generation bigint NOT NULL CHECK (generation>0),
 issued_at timestamptz NOT NULL,     -- no DEFAULT: issue_link sets both from one clock read
 expires_at timestamptz NOT NULL,
 principal_id uuid NOT NULL,
 PRIMARY KEY (tenant_id,store_id,bundle_id),
 FOREIGN KEY (tenant_id,store_id,bundle_id) REFERENCES claims.bundles(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK (expires_at>issued_at AND expires_at<=issued_at+interval '72 hours'));
```

KC03 tests every CHECK in both directions. Hot-path FKs
point at `live.claim_windows`/`live.offers`, never `live.sessions`, so `UpdateDraft`'s
session `FOR UPDATE` and media `FOR SHARE` never block ingest. Committed bundles always
have ≥1 line (bundle upsert only on an accepted command, same transaction; KC06).

### 3.1 Event shape matrix (what may be persisted; KC06 asserts every row)

| reason / outcome | grammar_kind | offer_id | quantity, explicit | bundle_id, line_version |
| --- | --- | --- | --- | --- |
| NO_MATCH | NO_MATCH | NULL | NULL | NULL |
| UNKNOWN_KEYWORD | MATCH / INVALID_QUANTITY | NULL | NULL | NULL |
| OFFER_INACTIVE | MATCH / INVALID_QUANTITY | set | set iff MATCH | NULL |
| INVALID_QUANTITY | INVALID_QUANTITY | set | NULL | NULL |
| QUANTITY_REQUIRED | MATCH | set | 1, false | NULL |
| QUANTITY_OVER_MAX / BUNDLE_LIMIT | MATCH | set | N, flag | NULL |
| ACCEPTED | MATCH | set | N, flag | set (+ previous_quantity or NULL for a new line) |

No column anywhere stores the comment, its unresolved head, a label outside
`claims.bundles`, or an actor key outside `claims.bundles`.

### 3.2 Privileges and RLS (exact matrix; KC03 compares for equality)

| Role | Object | Privilege | RLS policy |
| --- | --- | --- | --- |
| commerce_runtime | live.offers | SELECT, INSERT; UPDATE(max_quantity_per_claim,active,activated_at,version,updated_at) | read/update `S`; insert `S AND principal_id=app.principal_id` |
| commerce_runtime | live.claim_windows | SELECT, INSERT; UPDATE(state,match_mode,generation,opened_at,closed_at,version,principal_id,updated_at) | read `S`; insert/update WITH CHECK `S AND principal_id=app.principal_id` |
| commerce_runtime | claims.bundles | SELECT(all except owner_id); INSERT(tenant_id,store_id,session_id,platform,actor_key,label); UPDATE(line_count,version,updated_at) | read/update `S`; insert `S AND platform='manual'` |
| commerce_runtime | claims.lines | SELECT; INSERT(all except applied_version); UPDATE(quantity,version,updated_at) | read/update/insert `S` |
| commerce_runtime | claims.events | SELECT, INSERT | read `S`; insert `S AND source_kind='manual' AND principal_id=app.principal_id` |
| commerce_runtime | claims.links | SELECT(tenant_id,store_id,bundle_id,generation,issued_at,expires_at) only (link state for ListBundles; **no INSERT/UPDATE**) | read `S` |
| commerce_runtime | functions | EXECUTE `claims.issue_link(bytea,uuid,uuid,uuid,bigint,bytea,boolean)` only | — |
| commerce_buyer_runtime | claims schema | USAGE; EXECUTE `preview_link`, `redeem_link`, `mark_applied` only | **no table privilege on `claims.*` or `live.*`** |
| commerce_claims_writer | claims.links | SELECT(tenant_id,store_id,bundle_id,token_hash,generation,expires_at); INSERT(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id); UPDATE(token_hash,generation,issued_at,expires_at,principal_id) | read `S`; `issue`: insert WITH CHECK `S AND MERCHANT AND principal_id=app.principal_id`; `rotate`: USING `S AND MERCHANT` WITH CHECK `S AND MERCHANT AND principal_id=app.principal_id` |
| commerce_claims_writer | identity | USAGE; SELECT(token_hash,principal_id,audience,revoked_at,expires_at) on `identity.sessions`; EXECUTE `identity.resolve_access(bytea,uuid,text)` (same as `commerce_media_writer`, 0035) | sessions keep ACL isolation; no new policy |
| commerce_claims_writer | claims.bundles | SELECT(tenant_id,store_id,id,session_id,owner_id,bound_at,version); UPDATE(owner_id,bound_at) | read `S`; `bind`: USING `S AND BUYER AND (owner_id IS NULL OR owner_id=app.buyer_id)` WITH CHECK `S AND owner_id=app.buyer_id`; `release`: USING `S AND MERCHANT` WITH CHECK `S AND owner_id IS NULL AND bound_at IS NULL` |
| commerce_claims_writer | claims.lines | SELECT(tenant_id,store_id,bundle_id,offer_id,sku_id,quantity,version,applied_version); UPDATE(applied_version) | read/update `S` |
| commerce_claims_writer | live.offers | SELECT(tenant_id,store_id,id,session_id,keyword,active) | read `S` |

Shorthand: `app.x` = `nullif(current_setting('app.x',true),'')::uuid`; `BUYER` =
`app.principal_id IS NULL AND app.buyer_id IS NOT NULL`; `MERCHANT` = `app.principal_id
IS NOT NULL AND app.buyer_id IS NULL`, so `bind` can never match in a merchant
transaction and `release`/`issue`/`rotate` never in a buyer one. `commerce_runtime` may read
`claims.lines.applied_version` (the merchant list shows "applied") but never write it.
No policy references another claims table (no subquery), so RLS recursion (42P17) is
impossible. No privilege for
PUBLIC, `commerce_buyer_issuer`, `commerce_identity`, checkout, hosted, worker, any
`commerce_meta_*` or `commerce_media_*` role. No membership backfill. The existing
`validatePoolAuthority` object-owner reachability rule must reject any runtime login that
can reach `commerce_claims_writer`; KC03 proves it.

### 3.3 Definer functions

Owner `commerce_claims_writer`; `SECURITY DEFINER`; `SET search_path=pg_catalog`; fully
qualified relations; `REVOKE ALL FROM PUBLIC`; `COMMENT ON FUNCTION` names the owning
Go package and the only caller role. Every statement inside filters
`tenant_id=app.tenant_id AND store_id=app.store_id` explicitly (in addition to RLS);
a link, bundle or line of another store/tenant yields zero rows.

Buyer guard (preview, redeem, mark_applied; EXECUTE to `commerce_buyer_runtime`): READ
COMMITTED; `app.tenant_id/app.store_id/app.buyer_id/app.buyer_session_id` canonical
non-empty UUIDs and `app.principal_id=''` (set only by `buyer.WithScope`); `p_hash`
exactly 32 bytes. Violations: SQLSTATE `22023`. "Not found" is **zero rows**, identical
for unknown, expired, rotated, other-store, other-tenant and bound-to-another-owner.

- `claims.preview_link(p_hash bytea) RETURNS TABLE(bundle_version bigint, bound boolean,
  expires_at timestamptz, offer_id uuid, keyword text, sku_id uuid, quantity integer,
  pending boolean, offer_active boolean)` — `STABLE`; no locks, no writes. One row per
  line. `bound` = bound to the caller; `pending = applied_version IS DISTINCT FROM version`.
- `claims.redeem_link(p_hash bytea, p_expected_version bigint) RETURNS TABLE(bundle_id
  uuid, bundle_version bigint, offer_id uuid, sku_id uuid, quantity integer, line_version
  bigint, pending boolean, offer_active boolean)` — `VOLATILE`. Read link (no lock) →
  lock bundle `FOR NO KEY UPDATE` → **re-read link** (hash, scope,
  `expires_at>clock_timestamp()`) → another owner → zero rows → `version <>
  p_expected_version` → `RAISE SQLSTATE 'PT409'` → if unbound set `owner_id=app.buyer_id,
  bound_at=clock_timestamp()` → lock lines `FOR NO KEY UPDATE` → return all lines. Never
  marks anything applied. Not-found always precedes the version check (no existence leak).
- `claims.mark_applied(p_bundle uuid, p_offers uuid[], p_versions bigint[]) RETURNS
  integer` — `VOLATILE`. Arrays of equal length 1..50, no duplicate offers; bundle owner
  must equal `app.buyer_id`; sets `applied_version=version` only where `version=p_versions[i]`;
  updated count ≠ array length → `PT409`.

Merchant guard (issue_link; EXECUTE to `commerce_runtime` only): READ COMMITTED;
`app.tenant_id/app.store_id/app.principal_id` canonical non-empty, `app.authz_revision`
≥1, `app.buyer_id` unset/empty; both hashes exactly 32 bytes; non-zero UUIDs;
`p_expected_generation>=0`; `p_release` not NULL; else `22023`. Authority is decided in
the DB, not by the caller (house `live.request_media_stop` pattern, 0037):
`identity.resolve_access(p_auth_hash,p_store,'live:manage')` before any lock
(`unauthorized`→`PT401`, `not_found`→`PT404`, other non-ok→`PT403`); resolved
tenant/principal/revision and `p_store` must equal the GUCs (`PT403`); after the last
write a fresh `resolve_access` plus an `identity.sessions` expiry read at
`clock_timestamp()` must return the same tenant/principal/revision (`PT401`/`PT403`).

- `claims.issue_link(p_auth_hash bytea, p_store uuid, p_session uuid, p_bundle uuid,
  p_expected_generation bigint, p_new_hash bytea, p_release boolean) RETURNS
  TABLE(generation bigint, expires_at timestamptz, released boolean)` — `VOLATILE`.
  Authorize → lock the bundle of `p_session` `FOR NO KEY UPDATE` (zero rows → zero rows
  = not found) → current link generation (0 when none) ≠ `p_expected_generation` →
  `PT409` → `v_now := clock_timestamp()` **once** → if `p_release` and bound:
  `owner_id=NULL, bound_at=NULL`, every line's `applied_version=NULL` → upsert
  `claims.links` with `token_hash=p_new_hash, generation=p_expected_generation+1,
  issued_at=v_now, expires_at=v_now+interval '72 hours', principal_id=app.principal_id`;
  the `ON CONFLICT DO UPDATE` branch sets the same five columns, so every rotation resets
  `issued_at` and the §3 CHECK holds with equality → final authorize → return
  (`released` = a binding was cleared). Release and hash replacement happen only here and
  together; no other function or grant can clear `owner_id`, so a release without
  rotation is impossible (KC03).

## 4. Frozen Go interfaces

All functions take a caller-owned `pgx.Tx`; every error requires rollback; never a
partial value with an error. `internal/claims` imports only `command`, `platform`,
`buyer`, `storefront`, `pagination`, `internal/claims/grammar`, pgx and stdlib; only
the HTTP adapters import `claims`. It never imports `inventory`, `checkout`,
`integrations/meta` or River. Every input or credential type listed as "redacted"
implements constant `String`, `GoString` and `MarshalJSON` (`"[redacted]"`).

### 4.1 `internal/claims/grammar` (pure)

```go
// Package grammar owns the pure kw-v1 live-comment grammar (width map, trim, keyword, +N).
// It has no I/O, database, clock, logging or offer knowledge; callers resolve keywords.
package grammar

const Version = "kw-v1"

type Kind string // "MATCH" | "NO_MATCH" | "INVALID_QUANTITY"

type Result struct {
	Version  string // always Version
	Kind     Kind
	Keyword  string // canonical head; "" for NO_MATCH
	Quantity int64  // 1..999 for MATCH; 0 otherwise
	Explicit bool   // "+N" present (MATCH only)
} // redacted: String/GoString/MarshalJSON emit only Version and Kind (a keyword can be a phone number)

func Parse(text string) Result
func NormalizeKeyword(raw string) (string, bool)
func NormalizeLabel(raw string) (string, bool)
```

### 4.2 Merchant (`commerce_runtime` tx from `platform.WithScope`)

Every function mirrors `live.authorize`: canonical UUIDs, READ COMMITTED, exact
tenant/store/principal GUC equality, `platform.RequirePermission(token, perm)` **before
work, again inside the command after lock waits, and again after `command.Run` returns
(including replay)**. Writes use `command.Run` + `command.Audit` in the same
transaction; every canonical request includes `principal_id`.

```go
type MatchMode string // "EXACT" | "KEYWORD_QTY_ONLY"
type Reason string    // "" | NO_MATCH | UNKNOWN_KEYWORD | OFFER_INACTIVE | INVALID_QUANTITY |
                      // QUANTITY_REQUIRED | QUANTITY_OVER_MAX | BUNDLE_LIMIT | WINDOW_CLOSED

type OfferInput struct {
	Keyword             string `json:"keyword"`
	SKUID               string `json:"sku_id"`
	MaxQuantityPerClaim int64  `json:"max_quantity_per_claim"`
}
type OfferUpdate struct {
	ExpectedVersion     int64 `json:"expected_version"`
	MaxQuantityPerClaim int64 `json:"max_quantity_per_claim"`
	Active              bool  `json:"active"`
}
type Offer struct {
	ID                  string    `json:"offer_id"`
	SessionID           string    `json:"session_id"`
	Keyword             string    `json:"keyword"`
	SKUID               string    `json:"sku_id"`
	SKUCode             string    `json:"sku_code"`
	ProductName         string    `json:"product_name"`
	MaxQuantityPerClaim int64     `json:"max_quantity_per_claim"`
	Active              bool      `json:"active"`
	Version             int64     `json:"version"`
	ActivatedAt         time.Time `json:"activated_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}
type WindowInput struct {
	ExpectedVersion int64     `json:"expected_version"`
	State           string    `json:"state"` // OPEN | CLOSED
	MatchMode       MatchMode `json:"match_mode"`
}
type Window struct {
	SessionID  string     `json:"session_id"`
	State      string     `json:"state"`
	MatchMode  MatchMode  `json:"match_mode"`
	Generation int64      `json:"generation"`
	Version    int64      `json:"version"` // 0 = no row yet (reported CLOSED/EXACT)
	OpenedAt   *time.Time `json:"opened_at"`
	ClosedAt   *time.Time `json:"closed_at"`
}
type Stats struct {
	Generation int64            `json:"generation"`
	Accepted   int64            `json:"accepted"`
	Rejected   map[Reason]int64 `json:"rejected"` // all 7 persisted reasons present, 0 when none
}
type Board struct {
	Window Window  `json:"window"`
	Offers []Offer `json:"offers"` // ≤200, ORDER BY keyword
	Stats  Stats   `json:"stats"`
}
type ManualClaimInput struct {
	BundleID   string `json:"bundle_id"`   // append to an existing manual bundle of this session, or
	ActorLabel string `json:"actor_label"` // a new manual actor; exactly one of the two is non-empty
	Text       string `json:"text"`        // valid UTF-8, 1..256 bytes, no control characters
} // redacted
type ManualClaimResult struct {
	Outcome          string `json:"outcome"`           // ACCEPTED | REJECTED
	Reason           Reason `json:"reason"`            // "" when ACCEPTED
	OfferID          string `json:"offer_id"`          // "" unless offer-resolved
	Keyword          string `json:"keyword"`           // the offer's keyword; "" unless offer-resolved
	Quantity         int64  `json:"quantity"`          // 0 unless stored per §3.1
	PreviousQuantity int64  `json:"previous_quantity"` // 0 = new line or not accepted
	BundleID         string `json:"bundle_id"`         // "" unless ACCEPTED
	BundleVersion    int64  `json:"bundle_version"`
	LineVersion      int64  `json:"line_version"`
} // no label, text or actor key: this is the command receipt
type Line struct {
	OfferID  string `json:"offer_id"`
	Keyword  string `json:"keyword"`
	SKUID    string `json:"sku_id"`
	Quantity int64  `json:"quantity"`
	Version  int64  `json:"version"`
	Applied  bool   `json:"applied"` // applied_version = version
}
type LinkState struct {
	State      string     `json:"state"` // NONE | ACTIVE | EXPIRED
	Generation int64      `json:"generation"`
	ExpiresAt  *time.Time `json:"expires_at"`
}
type Bundle struct {
	ID        string    `json:"bundle_id"`
	Ref       string    `json:"ref"`      // upper(first 8 hex of ID), display only
	Platform  string    `json:"platform"`
	Label     string    `json:"label"`    // manual only; merchant-only personal data
	Bound     bool      `json:"bound"`    // bound_at IS NOT NULL; owner never exposed
	Version   int64     `json:"version"`
	Link      LinkState `json:"link"`
	Lines     []Line    `json:"lines"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type LinkInput struct {
	ExpectedGeneration int64 `json:"expected_generation"` // 0 = no link yet
	ReleaseBinding     bool  `json:"release_binding"`
}
type LabelKey struct{ /* 32 bytes */ } // server-held HMAC key for label_mac; redacted; never in the DB
func NewLabelKey(raw []byte) (LabelKey, error) // exactly 32 bytes; command.ErrInvalid
type LinkToken string // 43-char base64url of 32 bytes; redacted String/GoString/MarshalJSON
func ParseLinkToken(raw string) (LinkToken, error) // strict canonical base64url; command.ErrInvalid
type IssuedLink struct {
	Token      LinkToken // "" when Replayed; never stored or marshalled
	Generation int64
	ExpiresAt  time.Time
	Released   bool
	Replayed   bool
} // redacted

func GetBoard(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, sessionID string) (Board, error)                                  // live:read
func SetWindow(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID string, in WindowInput) (Window, error)          // live:manage
func CreateOffer(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID string, in OfferInput) (Offer, error)          // live:manage
func UpdateOffer(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID, offerID string, in OfferUpdate) (Offer, error) // live:manage
func RecordManualClaim(ctx context.Context, tx pgx.Tx, scope platform.Scope, labels LabelKey, token, key, sessionID string, in ManualClaimInput) (ManualClaimResult, error) // live:manage
func ListBundles(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, sessionID string, page pagination.Request) (pagination.Page[Bundle], error) // live:read
func IssueLink(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID, bundleID string, in LinkInput) (IssuedLink, error) // live:manage
```

| Function | Receipt operation | Audit action(s) | Canonical request (besides `principal_id`) |
| --- | --- | --- | --- |
| SetWindow | `live.claim.window.set` | `live.claim.window.opened` / `.closed` / `.mode_set` | `session_id, expected_version, state, match_mode` |
| CreateOffer | `live.claim.offer.create` | `live.claim.offer.created` | `session_id, keyword` (canonical), `sku_id, max_quantity_per_claim` |
| UpdateOffer | `live.claim.offer.update` | `live.claim.offer.updated` | `session_id, offer_id, expected_version, max_quantity_per_claim, active` |
| RecordManualClaim | `live.claim.manual` | `live.claim.manual.recorded` | `session_id, bundle_id\|"", label_mac\|"", grammar_version, kind, offer_id\|"", quantity, explicit` |
| IssueLink | `live.claim.link.issue` | `live.claim.link.issued` (+ `live.claim.binding.released`) | `session_id, bundle_id, expected_generation, release_binding` |

Rules:

- **SetWindow**: expected version 0 inserts the row in the requested state (CLOSED:
  generation 0; OPEN: generation 1) at version 1; later calls CAS on version.
  CLOSED→OPEN: `generation+1, opened_at=clock_timestamp(), closed_at=NULL`. OPEN→CLOSED:
  `closed_at=clock_timestamp()`, mode unchanged. CLOSED→CLOSED with a new mode:
  `mode_set`. OPEN→OPEN, mode change while OPEN, identical CLOSED→CLOSED →
  `ErrConflict`. A second OPEN window in the store → 23505 → `ErrConflict`. Close waits
  for in-flight ingests holding the window `FOR SHARE`.
- **CreateOffer**: after the receipt lock, `pg_advisory_xact_lock('claims-offers|t|s|session')`
  then count <200 (201st → `ErrConflict`); **no `live.sessions` row lock** (the FK
  `KEY SHARE` proves existence). SKU must be in scope (`ErrNotFound`) with active SKU +
  product and SKU currency = store currency (`ErrConflict`). Duplicate keyword in the
  session, or another **active** offer on the SKU (`live_offer_active_sku`) → `ErrConflict`.
- **UpdateOffer**: offer `FOR NO KEY UPDATE` (waits for in-flight ingests holding it
  `FOR SHARE`), CAS; reactivation sets `activated_at=clock_timestamp()` and, while another
  active offer holds the SKU, fails 23505 → `ErrConflict`; lowering max does not rewrite
  accepted lines.
- **ListBundles**: `pagination` collection `claim-bundles` (ParentID = session; keys
  `created_at,id` with the `live-sessions` time-key validation), `ORDER BY created_at DESC,
  id DESC`, default 50/max 100, **one SQL statement** (bundles + aggregated lines + link
  state) so a page cannot tear. No label/handle filter exists.
- **RecordManualClaim**: validate input and text; `ActorLabel` → `NormalizeLabel`
  (`ErrInvalid`) and `label_mac = hex(HMAC-SHA256(labels, "claims.manual-label.v1|" +
  tenant + "|" + store + "|" + session + "|" + normalized label))` ("" for `BundleID`),
  so the same key with another actor is a different canonical request → `ErrConflict`
  (I02); `p := grammar.Parse(text)`; resolve `offer_id` by `(session, p.Keyword)` (read,
  immutable keyword) for the canonical request only; then `command.Run`. Inside:
  `BundleID` → load that manual bundle of this session (`ErrNotFound` otherwise) and
  reuse its `platform/actor_key`; `ActorLabel` → label already used in this session →
  `ErrConflict` (pick the existing bundle), else a fresh 32-byte `crypto/rand` hex
  `actor_key`. Then `IngestParsed` with a fresh random `SourceEventID`,
  `OccurredAt=clock_timestamp()` and this session. **Text, label and any unkeyed hash of
  either are never in the canonical request, receipt, audit or logs**; `label_mac` reaches
  storage only inside the SHA-256 request digest and cannot be dictionary-tested without
  the key. `labels` comes from `COMMERCE_CLAIMS_LABEL_KEY` (base64 32 bytes, distinct from
  every other configured key, loaded by `cmd/api`; never in the DB). The label is stored
  only in `claims.bundles.label` and only on ACCEPTED (a rejected first comment of a new
  actor leaves no label anywhere).
- **IssueLink**: inside `command.Run`: generate the link token (32 bytes `crypto/rand`)
  in the closure, set `app.authz_revision` (media_stop pattern), then one call
  `claims.issue_link(sha256(merchant token), store, session, bundle, expected_generation,
  sha256(link token), release_binding)` (§3.3): zero rows → `ErrNotFound`; `PT409` →
  `ErrConflict`; `PT401/PT403/PT404` → `platform.ErrUnauthorized/ErrForbidden/
  ErrScopeNotFound`. Go takes no bundle/link lock itself and has no link or binding
  write grant. The receipt result is the token-free struct
  `{bundle_id, generation, expires_at, released}`; replay returns it with `Token=""`,
  `Replayed=true`.

### 4.3 Ingest (D8 entry point)

```go
type IngestInput struct {
	TenantID      string    // must equal app.tenant_id of tx (server-derived, never from request)
	StoreID       string    // must equal app.store_id of tx
	SessionID     string    // claim context; this session's window is locked
	SourceKind    string    // v1: "manual" only ("meta" reserved for T10c -> ErrInvalid)
	SourceEventID string    // canonical UUID, unique per store across kinds
	Platform      string    // v1: "manual" only
	ActorKey      string    // 64 lowercase hex, opaque, derived by the caller
	ActorLabel    string    // manual only; required only when the bundle does not exist yet, ignored otherwise
	PrincipalID   string    // manual: must equal app.principal_id
	Text          string    // Ingest only; parsed then discarded; must be "" for IngestParsed
	OccurredAt    time.Time // UTC µs; <= clock_timestamp()+120s
} // redacted
type IngestResult struct {
	Outcome          string `json:"outcome"`
	Reason           Reason `json:"reason"`
	Duplicate        bool   `json:"duplicate"`
	EventID          string `json:"event_id"` // "" for WINDOW_CLOSED
	SessionID        string `json:"session_id"`
	WindowGeneration int64  `json:"window_generation"`
	GrammarVersion   string `json:"grammar_version"`
	OfferID          string `json:"offer_id"`
	Keyword          string `json:"keyword"` // offer keyword only
	BundleID         string `json:"bundle_id"`
	Quantity         int64  `json:"quantity"`
	PreviousQuantity int64  `json:"previous_quantity"`
	LineVersion      int64  `json:"line_version"`
	BundleVersion    int64  `json:"bundle_version"`
}
func Ingest(ctx context.Context, tx pgx.Tx, in IngestInput) (IngestResult, error) // = IngestParsed(grammar.Parse(Text))
func IngestParsed(ctx context.Context, tx pgx.Tx, in IngestInput, p grammar.Result) (IngestResult, error)
```

No permission check: callers must already hold authority. v1 has exactly one production
caller (`RecordManualClaim`); KC15 source guard enforces it. In the caller's transaction:

1. Validate shapes (UUIDs, enums, hex key, `len(Text)<=65536`, well-formed `p`,
   non-zero `OccurredAt <= clock_timestamp()+120s`), GUC equality and READ COMMITTED →
   else `ErrInvalid`.
2. `pg_advisory_xact_lock('claim-source|t|s|source')`. If the source exists: compare only
   **immutable facts** — `source_kind, platform, session_id, occurred_at` (µs),
   `grammar_version`, `p.Kind = grammar_kind`; if stored `offer_id` is set,
   `p.Keyword = live.offers.keyword` of that offer (immutable); if stored `quantity` is set,
   `p.Quantity/p.Explicit` equal; if ACCEPTED, the bundle's `actor_key` equals. Mismatch →
   `ErrConflict` (I02); else return the stored outcome, `Duplicate=true`, **write
   nothing**. Offer state is never re-resolved (S03).
3. `SELECT … FROM live.claim_windows WHERE (t,s,session_id)=… AND state='OPEN' FOR SHARE`.
   No row or `occurred_at < opened_at` → `WINDOW_CLOSED`, **not persisted**.
4. `NO_MATCH` → persist REJECTED (§3.1).
5. Offer by `(session, p.Keyword)` `FOR SHARE`. Missing → persist `UNKNOWN_KEYWORD`
   (nothing but the reason). Otherwise decide OFFER_INACTIVE → INVALID_QUANTITY →
   QUANTITY_REQUIRED → QUANTITY_OVER_MAX on the locked offer; persist with `offer_id`.
6. Bundle `SELECT … WHERE (t,s,session,platform,actor_key)=… FOR NO KEY UPDATE`; found →
   use it and **ignore `ActorLabel`**. None → `ActorLabel` required (else `ErrInvalid`),
   `INSERT … ON CONFLICT (t,s,session,platform,actor_key) DO NOTHING` with that label (a
   duplicate label → 23505 → `ErrConflict`), then the same `SELECT … FOR NO KEY UPDATE`.
   Never INSERT for an existing bundle: CHECK/RLS `WITH CHECK` run on the proposed row
   before conflict arbitration, so a NULL-label manual INSERT would raise 23514 (S05).
   Line `FOR NO KEY UPDATE`. New line with `line_count=50` → persist `BUNDLE_LIMIT` (the
   bundle pre-existed).
7. Upsert line `quantity=N, version=version+1`; bundle `version+1, line_count(+1 if new),
   updated_at`; insert the ACCEPTED event with `line_version`, `previous_quantity`.
   Every accepted command bumps versions even if N is unchanged (a repeated buyer
   instruction re-applies on the next sync).

### 4.4 Buyer (`buyer.Scope` tx from `buyer.WithScope`)

```go
type PreviewLine struct {
	Keyword        string `json:"keyword"`
	SKUID          string `json:"sku_id"`
	SKUCode        string `json:"sku_code"`
	ProductName    string `json:"product_name"`
	Currency       string `json:"currency"`
	UnitPriceMinor int64  `json:"unit_price_minor"` // display only; Quote remains the price authority
	Quantity       int64  `json:"quantity"`
	Pending        bool   `json:"pending"`
	Available      bool   `json:"available"`
}
type Preview struct {
	BundleVersion int64         `json:"bundle_version"`
	Bound         bool          `json:"bound"`
	ExpiresAt     time.Time     `json:"expires_at"`
	Lines         []PreviewLine `json:"lines"` // ORDER BY keyword
}
type RedeemInput struct {
	ExpectedBundleVersion int64 `json:"expected_bundle_version"`
}
type Skipped struct {
	SKUID  string `json:"sku_id"`
	Reason string `json:"reason"` // unavailable | offer_inactive
}
type Redeemed struct {
	BundleVersion int64             `json:"bundle_version"`
	Cart          storefront.Cart   `json:"cart"`
	Applied       []storefront.Item `json:"applied"`
	Skipped       []Skipped         `json:"skipped"`
}

func PreviewLink(ctx context.Context, tx pgx.Tx, s buyer.Scope, token LinkToken) (Preview, error)
func RedeemLink(ctx context.Context, tx pgx.Tx, s buyer.Scope, key string, token LinkToken, in RedeemInput) (Redeemed, error)
```

`PreviewLink`: `buyer.CheckScope`; `claims.preview_link(sha256(token))`; zero rows →
`ErrNotFound`; enrich from existing buyer-readable catalog columns; `Available` = offer
active ∧ SKU and product active ∧ SKU currency = store currency. No receipt, event, lock
or write. No session title, label, actor key, platform, owner or principal.

`RedeemLink`: `buyer.RunCommand(ctx, tx, s, "claims.redeem", key, {link_sha256: hex,
expected_bundle_version}, &out, fn)`, where fn:

1. `claims.redeem_link(hash, expected)`; zero rows → `ErrNotFound`; `PT409` →
   `ErrConflict` (the whole command, including the bind, rolls back).
2. Pending lines whose offer is inactive or SKU/product unavailable are `skipped` and
   **stay pending**; the rest override the cart quantity for that SKU (absolute).
   Non-claim and non-pending lines are untouched. `live_offer_active_sku` leaves at most
   one applicable line per SKU in the `redeem_link` snapshot; Go asserts it (`ErrConflict`).
3. Nothing to apply → `out.Cart = storefront.GetCart` (no cart write).
4. Otherwise `storefront.LockCartOwner` → `storefront.GetCart` → merged set (>50 SKUs →
   `ErrInvalid`, bind rolled back) → `storefront.SetCart(ctx, tx, s, "clm:" +
   hex(sha256("claims.redeem|"+key))[:48], {ExpectedVersion: cart.Version, Items})`;
   post-condition: the returned cart has every applied SKU at its target, else
   `ErrConflict`; then `claims.mark_applied` count must match.
5. A pre-existing **non-claim** cart line that is archived or in another currency makes
   SetCart's `lockCatalog` return `ErrConflict`: 409, bind rolled back, nothing partial.
   Buyer lines are never dropped silently.

Same key + same token + same body replays the stored result (no new cart version); a
changed token or body with the same key → `ErrConflict`. Several bundles of one owner
that claim the same SKU resolve by last apply.

Integrator-owned helper in `internal/storefront/cart.go`:
`func LockCartOwner(ctx context.Context, tx pgx.Tx, s buyer.Scope) error` —
`buyer.CheckScope`, then the exact advisory key SetCart already uses
(`"cart|"+tenant+"|"+store+"|"+owner`); SetCart calls it (no behaviour change; advisory
locks are re-entrant in a transaction).

### 4.5 Error mapping (claims `mapError`)

| Condition | Go error |
| --- | --- |
| malformed UUID/keyword/label/text/mode/state/quantity/token; both or neither of bundle_id/actor_label; merged cart >50; SQLSTATE 22023/23514/22P02; v1 non-manual source | `command.ErrInvalid` |
| version/generation CAS; 23505 (keyword, second active offer on a SKU, label, second OPEN window); forbidden window transition; 201st offer; inactive/foreign SKU at offer create; duplicate source with different facts; PT409; cart post-condition; pre-existing unavailable cart item | `command.ErrConflict` |
| session/offer/bundle/SKU outside scope; 23503; unknown/expired/rotated/other-owner/other-store link | `command.ErrNotFound` |
| authority | unchanged `platform.*` / `buyer.*` errors; `issue_link` `PT401`/`PT403`/`PT404` → `platform.ErrUnauthorized`/`ErrForbidden`/`ErrScopeNotFound` |
| 40P01, 55P03, statement timeout, other DB errors | returned unchanged (HTTP 503) |

Grammar/offer/window rejections are **data** (`Outcome=REJECTED`), never errors.

## 5. Idempotency, ordering and concurrency

1. **Replay**: merchant commands via `command.Run` (store-global receipts, principal in
   hash; cart-quote-v1 caveat applies); buyer redeem via `buyer.RunCommand`
   (owner+session). Ingest replay by `UNIQUE(tenant,store,source_event_id)` + advisory.
2. **Absolute quantities** (D4): each accepted command sets the line; no path adds.
3. **Order**: commands for one line serialize on the bundle row; final quantity = last
   committed accepted event (arch §11.1 server order).
4. **Window fence**: ingest holds this session's window `FOR SHARE` to commit; close waits;
   any ingest after close sees CLOSED. **Offer fence**: ingest holds the offer `FOR SHARE`;
   deactivation waits.
5. **Binding**: first redeem commits `owner_id`; a concurrent second owner waits on the
   bundle row, then gets zero rows. Release and redeem serialize on the bundle row.
6. **Global lock order** (skip absent steps; no path reverses it): receipt advisory
   (`command|…` / `buyer-command|…`) → `claim-source` advisory → `claims-offers` advisory →
   `live.claim_windows` → `live.offers` → `claims.bundles` → `claims.lines` →
   `claims.links` → cart advisory (`LockCartOwner`) → nested `cart.set` receipt advisory
   (derived `clm:` key) → `storefront.carts` → catalog products → SKUs → inserts (events,
   audit, receipts). The nested receipt lock follows the cart advisory only on redeem; a
   direct `PUT /v1/buyer/cart` takes them in the opposite order, so buyerhttp **rejects
   any client `Idempotency-Key` starting `clm:` with 422** — the two orders can then never
   share a key. Parent locks use `FOR NO KEY UPDATE` so child FK `KEY SHARE` is not blocked.
7. **Bounds** (I23): ≤200 offers/session, ≤50 lines/bundle, ≤50 cart SKUs, manual text
   ≤256 bytes, parser short-circuit at 256 bytes, ingest text ≤64 KiB, page ≤100,
   request ≤64 KiB, inherited 5 s statement / 1 s lock timeouts.

## 6. Buyer handoff token lifecycle

| State | Entered by | Preview (GET) | Redeem (POST) |
| --- | --- | --- | --- |
| NONE | bundle created | 404 | 404 |
| ACTIVE, unbound | IssueLink (`expected_generation=0`), rotation, or release+rotation | 200 `bound=false` | binds caller, applies pending lines |
| ACTIVE, bound to caller | first redeem | 200 `bound=true` | applies lines with newer versions |
| ACTIVE, bound to other owner | — | 404 (identical body) | 404, no write |
| EXPIRED (`expires_at<=now`) | DB clock | 404 | 404 |
| ROTATED (old token) | IssueLink `expected_generation=g` | 404 | 404 |

- Token: 32 bytes `crypto/rand`, base64url raw (43 chars); DB stores only
  `sha256(token)`; `issued_at` and `expires_at=issued_at+72h` come from one
  `clock_timestamp()` read in `claims.issue_link` (every rotation resets both); generation
  CAS. Rotation keeps the
  binding unless `release_binding=true`; release always rotates, resets every line's
  applied state (the new owner receives all lines) and leaves the released owner's cart
  untouched.
- The token exists only in the IssueLink closure and in the HTTP body written **after**
  `platform.WithScope` returns nil (COMMIT acknowledged; studio `input/token` pattern).
  It is never in a receipt, audit row, log, URL path/query, cache or error.
- Buyer URL: `https://<published storefront origin>/<locale>/claim#t=<token>`. The
  fragment never reaches servers, logs or `Referer`; a crawler/preview-bot GET carries no
  token and writes nothing. Redeem requires an explicit click (T10b).
- Sync (D7): later accepted commands bump line and bundle versions; the bound buyer
  reopens the link, previews the new `bundle_version` and POSTs again; only newer lines
  are applied (R1). After TTL the merchant rotates (binding kept) and resends.

## 7. HTTP

All responses `Cache-Control: private, no-store` (+ existing `nosniff`); strict JSON
(`studioDecodeBody` / `decodeJSON`: allowed keys only, no duplicates, no trailing data);
existing error codes only (`invalid_json` 400, `unauthorized` 401, `forbidden` 403,
`not_found` 404, `method_not_allowed` 405, `conflict` 409, `json_required` 415,
`invalid_request` 422, `unavailable`/`retry_later` 503). Success is 200, including
REJECTED/WINDOW_CLOSED manual claims.

### 7.1 Merchant (`internal/httpapi/claims.go`, integrator)

Base `/v1/admin/stores/{store_id}/live-sessions/{session_id}/claims`, mounted with the
studio routes; methods limited to GET/POST/PATCH (admin BFF live-sessions rule).

| # | Method path | Perm | Idempotency-Key | Body keys | 200 body |
| --- | --- | --- | --- | --- | --- |
| M1 | `GET …/claims` | live:read | forbidden | none | Board |
| M2 | `POST …/claims/window` | live:manage | required | `expected_version,state,match_mode` | Window |
| M3 | `POST …/claims/offers` | live:manage | required | `keyword,sku_id,max_quantity_per_claim` | Offer |
| M4 | `PATCH …/claims/offers/{offer_id}` | live:manage | required | `expected_version,max_quantity_per_claim,active` | Offer |
| M5 | `POST …/claims/manual` | live:manage | required | `text` + exactly one of `bundle_id`/`actor_label` | ManualClaimResult |
| M6 | `GET …/claims/bundles?limit=&cursor=` | live:read | forbidden | none | `{items:[Bundle],next_cursor}` |
| M7 | `POST …/claims/bundles/{bundle_id}/link` | live:manage | required | `expected_generation,release_binding` | `{token,generation,expires_at,released,replayed}` + `Referrer-Policy: no-referrer` |

M7 builds an explicit response DTO after COMMIT: `token` is the string on first execution
and `null` with `replayed:true` on replay. Query strings are rejected (422) on every route
except M6 (`limit`, `cursor` only; no label/handle parameter). Methodless fallbacks keep
405 inside the private boundary (studio pattern). `core-openapi.json` gains M1–M7.

### 7.2 Buyer (`internal/buyerhttp`, BFF-only, integrator)

| # | Method path | Credential / headers | Body | 200 body |
| --- | --- | --- | --- | --- |
| B1 | `GET /v1/buyer/claim-link` | buyer bearer + BFF key; `X-Commerce-Claim-Token` exactly once (43-char canonical); `Idempotency-Key` forbidden | none | `{bundle_version,bound,expires_at,lines:[{keyword,sku_id,sku_code,product_name,currency,unit_price_minor,quantity,pending,available}]}` |
| B2 | `POST /v1/buyer/claim-link/redeem` | same + `Idempotency-Key` required | `{expected_bundle_version}` | `{bundle_version,cart:<existing cart projection>,applied:[{sku_id,quantity}],skipped:[{sku_id,reason}]}` |

`X-Commerce-Claim-Token` on any other route → 403 `forbidden` (extend `forbiddenInput`);
the token is never accepted in a path, query or body. Malformed token → 422;
unknown/expired/rotated/other-owner/other-store → 404 with a byte-identical body; bundle
changed or cart conflict → 409. `PUT /v1/buyer/cart` with an `Idempotency-Key` starting
`clm:` → 422 (§5.6). Store is resolved from the published origin, never from input.

## 8. Privacy and retention

- Raw comment text, an unresolved keyword, and any hash of either are **never persisted**
  (not in claims/live tables, `ops.command_results`, `buyer.command_results`, audit,
  storefront events or logs). Meta plaintext stays in the encrypted inbox under its own
  retention. Arch §11.1 "raw comment retained for audit" applies to provider comments via that
  inbox; the manual path keeps the parse result plus principal (documented difference).
- `actor_key` is pseudonymous personal data (arch §14.4); manual keys are random, T10c keys are
  decided in T10c. Never returned by any API; never in events.
- `bundles.label` is merchant-entered personal data (typically a display name): merchant
  only (live:read), never in receipts, audit, logs, URLs or buyer projections (the receipt
  request digest covers only the keyed `label_mac`, §4.2); the future UI warns against
  phone numbers and addresses.
- Tokens: SHA-256 only; never in logs, audit, receipts, URL path/query, browser storage or
  BFF caches. All input/credential types are redacted (§4).
- **Retention classes** (days are not fixed here, arch §21.4; U08 sets them): claim events;
  actor keys; manual labels; bindings (`owner_id`, `bound_at`); link hashes. Offers and
  windows are merchant configuration. Purge job and DSAR erasure (T14) are **not in this
  slice**; **production mount is blocked** until they cover `bundles.actor_key`,
  `bundles.label`, bindings and links and pass their own gate.

## 9. Required gates (all NOT_RUN)

Real-PG tests reuse `fixture()→t04Fixture→pricingFixture→cqSetup`, so they live in
`tests/foundation/live_claims_test.go` and `live_claims_http_test.go` (write_paths
amendment, §10), one top-level `TestLiveClaimsKCnn…` per gate. Pure tests live in
`internal/claims/grammar` and `internal/claims`. Every sentinel below is synthetic.

| Gate | Test | Evidence | Required |
| --- | --- | --- | --- |
| KC01 | `TestGrammarKC01Vectors`, `FuzzParse` | UNIT | Every §2.4 parse vector from the JSON file; fuzz ≥60 s: no panic, bounded time, MATCH ⇒ `^[A-Z0-9]{1,16}$` and 1..999, deterministic, idempotent on normalized input, ASCII-only case mapping |
| KC02 | `TestClaimsKC02Units` | UNIT | NormalizeKeyword/NormalizeLabel vectors (`@Amy ` ≡ `amy`); ParseLinkToken strictness; NewLabelKey rejects ≠32 bytes; `label_mac` differs per label and per tenant/store/session, equal for `@Amy `/`amy`; `%v/%+v/%#v/json` of Result, IngestInput, ManualClaimInput, LabelKey, LinkToken, IssuedLink emit no sentinel |
| KC03 | `TestLiveClaimsKC03Schema` | REAL_PG | Fresh and populated-0043 upgrade; migrate twice; FORCE RLS on six tables; §3.2 matrix equality from `information_schema.column_privileges`/`table_privileges`; definer owner, `prosecdef`, `proconfig`, EXECUTE ACLs; CHECK/FK negatives incl. every §3.1 row and the NO_MATCH iff; 42501 for buyer/meta/checkout/worker/issuer/identity on every table; `live_offer_active_sku` partial (inactive duplicates allowed); no function EXECUTE-able by `commerce_runtime` or `commerce_buyer_runtime` can clear `owner_id` without replacing `token_hash` (catalog enumeration + direct call of each); `claims.links` has no DEFAULT on `issued_at`; pool validator rejects a login reaching `commerce_claims_writer` |
| KC04 | `TestLiveClaimsKC04Offers` | REAL_PG | Create/update replay; changed body 409; CAS race one winner; dup keyword 409; second active offer on one SKU 409 on create and on reactivation; typo recovery: `A11→X` deactivated → create `A1→X` 200 → `A1` ACCEPTED in the same session and redeem applies only the active offer's line; same keyword other session OK; 201st 409 under concurrency; SKU missing 404 / archived or foreign currency 409 / cross-store 404; canonical keyword before hashing; keyword/SKU immutable (42501); reactivation moves `activated_at`; create while another tx holds the session `FOR SHARE` succeeds within lock_timeout; missing live:manage; revoked token after lock wait and on replay; audit failure rolls back |
| KC05 | `TestLiveClaimsKC05Window` | REAL_PG | Transition table; two sessions open concurrently → one 409; OPEN→OPEN and mode-while-OPEN 409; generation increments; close blocks on an in-flight ingest (`pg_stat_activity`, not sleeps); ingest after close → WINDOW_CLOSED with zero rows |
| KC06 | `TestLiveClaimsKC06Ingest` | REAL_PG | Every §2.4 ingest vector incl. S01–S05; §3.1 shape per reason; I03 rows carry no keyword/quantity/offer; S03 unknown→create offer→redeliver = duplicate; changed immutable fact under same source → 409; `occurred_at` before `opened_at` / beyond +120 s; committed bundles ≥1 line; row-count and version snapshot of inventory, storefront, checkout, buyer, social, meta_inbox unchanged |
| KC07 | `TestLiveClaimsKC07Concurrency` | REAL_PG | Same source ×20 concurrently → one event; same line, different commands → final = last committed, `line_version` = accepted count; lock-hook interleave "close S1, open S2" between manual request start and ingest → WINDOW_CLOSED, zero S2 rows; deactivate vs ingest; release vs redeem; mixed ingest/redeem/issue/offer-update/window-close/`UpdateDraft`/`PUT cart`/Quote workload → zero 40P01 |
| KC08 | `TestLiveClaimsKC08ManualPrivacy` | REAL_PG | Replay identical receipt after later commands; changed body 409; same key with a different `actor_label` 409 (I02), same key with `@Amy ` vs `amy` replays; new actor with a used label 409; two labels → two bundles; rejected first comment leaves no bundle/label; **sentinel scan** of every column of claims/live/ops/buyer receipts, audit, storefront events and captured logs finds no `0912345678`, `A1是不是红色` or text sentinel; label sentinel found only in `claims.bundles.label`, its unkeyed SHA-256 nowhere |
| KC09 | `TestLiveClaimsKC09Link` | REAL_PG | 43-char token; DB-wide scan finds only its SHA-256; token absent from receipts/audit/logs; replay → `token:""`, `replayed:true`; concurrent issue → one CAS winner; rotation kills old token; first issue, then 3 rotations and 3 release+rotations: each resets `issued_at` and `expires_at = issued_at+72h` exactly (DB, no 23514); release+rotate: old token 404, new owner binds, previous owner's preview 404; direct `claims.issue_link` by a `live:read`-only principal → PT403, and `live:manage` revoked while it waits on the bundle lock → PT403 (`pg_stat_activity`, not sleeps), nothing changed in either case; other-session/store/tenant bundle 404 |
| KC10 | `TestLiveClaimsKC10Preview` | REAL_PG | Read-only (row counts, versions, receipts, events identical after repeated previews by several owners); unknown/expired/rotated/other-owner/other-store/other-tenant → identical not-found; bound flag; pending/available flags; exact projection keys (no title/label/actor/platform/owner/principal) |
| KC11 | `TestLiveClaimsKC11Redeem` | REAL_PG | Bind + set targets keeping unrelated lines; replay identical, no new cart version; stale `expected_bundle_version` → 409 and **no binding**; second owner 404, no write; later `A1+3` then new-key redeem sets 3; buyer-removed line not re-added unless its claim changed; inactive offer/archived SKU skipped and still pending, applied after reactivation; pre-existing archived non-claim cart item → 409, bind rolled back; >50 → 422 rolled back; release → new owner receives all lines; two bundles same SKU → last apply wins; no inventory/reservation/order rows |
| KC12 | `TestLiveClaimsKC12Isolation` | REAL_PG | 2 tenants × 2 stores: forged buyer GUCs + foreign token hash → zero rows / 22023; merchant with another store's GUC sees nothing; merchant cannot read `token_hash`/`owner_id` or write `owner_id`/`applied_version`/`claims.links` (42501); `claims.issue_link` with a `live:read`-only principal, a foreign `p_store`, or GUCs not matching the resolved session → PT403/PT404, no write; every policy executed as every role raises no 42P17 |
| KC13 | `TestLiveClaimsKC13MerchantHTTP` | HTTP_PG | M1–M7 exact routes/methods/keys/status codes; api refuses to start without a valid, distinct `COMMERCE_CLAIMS_LABEL_KEY`; live:read vs live:manage 403; cross-store 404; no-store/no-referrer; query rejection; token only in first M7 body and only after COMMIT (fault-injected commit failure → no token); replay body byte-identical except `token`/`replayed` |
| KC14 | `TestLiveClaimsKC14BuyerHTTP` | HTTP_PG | B1–B2 exact routes; token header only there (path/query/body/other route rejected); strict JSON; identical 404; first and replay B2 bodies byte-identical; `PUT cart` with `clm:` key → 422; log capture has no token/text/actor key |
| KC15 | `TestLiveClaimsKC15Guards` + root | REVIEW + regression | Source guards: only `RecordManualClaim` calls `Ingest`/`IngestParsed`; `internal/integrations/meta` unchanged; no River kind; `internal/claims` imports per §4; package headers and D9 comments present; full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`, prior CQ/buyer/LSP/STU/MC01–07 gates unchanged; independent test_worker + security_reviewer verdict; author is not sole acceptor |

Commands (record exit codes): `go test -count=1 ./internal/claims/...`;
`go test -run='^$' -fuzz='^FuzzParse$' -fuzztime=60s ./internal/claims/grammar`;
`bash scripts/dev/test-local.sh --live-claims` (integrator adds this mode: guard
`grep -q '^func TestLiveClaimsKC' tests/foundation/live_claims_test.go`, then
`go test -race -count=1 -timeout=300s -run '^TestLiveClaims' -v ./internal/claims/... ./tests/foundation`,
printing a PASS line that states "MOCK manual ingress; no provider"); full
`bash scripts/dev/test-local.sh`; `go vet ./...`.

## 10. Ownership, comments (D9) and sequencing

| Artifact | Owner |
| --- | --- |
| `internal/claims/**` incl. `grammar/` (+ unit/fuzz tests) | T10 commerce_worker |
| `tests/claims/kw-v1-vectors.json`; `tests/foundation/live_claims*_test.go` | independent test_worker (T10 write_paths amendment adds the foundation glob) |
| `migrations/0060_live_claims.sql` | integrator |
| `storefront.LockCartOwner`; pagination collection `claim-bundles` | integrator |
| `internal/httpapi/claims.go` + mount; `internal/buyerhttp` B1–B2, `forbiddenInput`, `clm:` rejection; `core-openapi.json` | integrator |
| `cmd/api` loading/validating `COMMERCE_CLAIMS_LABEL_KEY` into `claims.LabelKey` | integrator |
| `scripts/dev/test-local.sh --live-claims`; `contracts/tasks.json` T10 amendment | integrator |

`internal/cart/**` stays unused (no second cart). Every Go file starts with one ownership
line and one non-goal line, e.g. `// Package claims owns live keyword offers, claim
windows, claim bundles and claim-link redemption.` / `// It never reserves inventory,
sends messages, reads Meta storage or creates orders.` Each cross-package call and each
SQL statement on another domain's table carries a one-line comment naming the table/role
and why (e.g. `storefront.SetCart // only cart writer; claims never writes storefront
tables`). Every table and function gets `COMMENT ON` naming its owning package and the
roles that may use it. Order: freeze → migration + helpers ∥ grammar + KC01 → domain +
KC02–KC12 → HTTP + KC13–14 → KC15. Initial writers ≤2 (commerce_worker, integrator);
test_worker starts KC01 immediately and PG gates once 0060 lands.

## 11. Follow-up (not in this slice)

### 11.1 T10b — UI and BFF (ui_worker; **BLOCKED** on owner composition approval, studio-v1 rule — request now, in parallel)

- Studio › Claims panel: window badge + Open/Close, mode selector (CLOSED only), offers
  table, per-reason counters ("N not understood — pin the host prompt"), manual entry
  (bundle picker or new label + text; fresh Idempotency-Key per submit), bundles list,
  issue/rotate/release link. The one-time link dialog **masks the token by default**
  (copy-link / copy-message buttons; reveal only on explicit click), never renders it on a
  capturable/broadcast surface, and discards it on close. Every surface is labelled
  "MOCK capture — comments are not read automatically yet".
- Storefront `/[locale]/claim`: read `#t=`, `history.replaceState` immediately, token in
  memory only, `Referrer-Policy: no-referrer`, no third-party scripts; preview with "current
  price, final at checkout" and "claims do not reserve stock"; explicit button → B2 with one
  Idempotency-Key per click; 404 copy "This link expired or was replaced — ask the seller
  for a new link"; 409 copy "Your cart has an item that is no longer available, or the
  claim changed. Review your cart, then try again." (link to cart; no auto-retry loop).
- Admin and storefront BFF allowlists for exactly M1–M7 and B1–B2 (token header forwarded
  only on B1/B2). KC16 browser gate (Next-Go-PG Chromium, three locales, MOCK).
- Frozen host prompt copy (EXACT; `{KW}` = an active offer keyword):
  zh-TW「留言關鍵字就能登記：{KW} = 1 件；{KW}+2 = 數量改成 2 件（不是再加 2 件）。之後再留言，以最新數量為準。請只留關鍵字，不要加其他文字。留言不代表已保留庫存，結帳時才確認。」
  zh-CN「评论口令即可登记：{KW} = 1 件；{KW}+2 = 数量改成 2 件（不是再加 2 件）。再次评论以最新数量为准。请只发口令，不要加其他文字。评论不代表已保留库存，结账时才确认。」
  en "Comment the code to claim: {KW} = 1 item; {KW}+2 = set your quantity to 2 (it does not add 2 more). Your latest comment replaces the earlier quantity. Comment only the code. Claims don't reserve stock; stock is confirmed at checkout."
  KEYWORD_QTY_ONLY replaces the first clause with 「請留言「{KW}+數量」，例如 {KW}+1；只留 {KW} 不會登記」 /
  「请评论“{KW}+数量”，例如 {KW}+1；只发 {KW} 不会登记」 / "Comment {KW}+quantity, e.g. {KW}+1; {KW} alone is not counted".

### 11.2 T10c — Meta consumer hookup (integration_worker + integrator; separate frozen amendment `meta-claims-intake-v1`)

Why not now: `meta-consumer-v1` is FROZEN/PASS (MC01–07) with no cross-domain rows and
no generic table access; the store-wide window cannot attribute a comment to the live
video (R10); `projectSocial` does not project `from.id` or the object id; links cannot be
delivered to Meta actors before T07 eligibility. Preconditions, each with MCL gates:

- H1 session ↔ source binding (FB live video/post id, IG live media id per session,
  entered or copied from T08 destinations); unbound objects never claim.
- H2 only comment-add events qualify; edit/remove never claim; the seller's own comments
  (`from.id` = asset) ignored; missing `from.id` or object id fails closed.
- H3 actor key domain-separated per app/asset and distinct from messaging `peer_key`;
  decide keyed derivation (HMAC under a server-held secret) versus unkeyed hash, because
  sender ids are enumerable.
- H4 staged intake: the consumer runs `grammar.Parse` in memory and writes a text-free
  intake row **only through a claims-owned SECURITY DEFINER function** with a fixed
  signature (no claims table grant to `commerce_meta_consumer`), enqueuing a River job or
  fenced poll in the same transaction; a claims worker applies it via `IngestParsed`
  (`source_event_id` = inbox event). Needs its own MC01 amendment review and a
  rollback-leaves-no-intake test.
- H5 append-only window interval history with a bounded late-arrival grace, so "occurred
  inside an open window" holds for late webhooks (v1 requires OPEN at ingest).
- H6 per-actor rate limit and per-session bundle cap before real traffic (arch §21.5).
- H7 any private-reply link delivery goes through T07 `CanSendMessage` (I07); claim
  acceptance never implies send eligibility. CHECK widening (`meta`, `facebook`,
  `instagram`) ships in that amendment's migration.

### 11.3 T10d — merchant corrections and buyer sync (commerce_worker, after T10b)

AdjustLine (set/void/revive, operator override with audit), buyer "my claims" list and
cart sync banner, typed HTTP error subcodes, claims export. Each needs its own contract
delta and gates.

### 11.4 T14 / U08 — retention purge and DSAR erasure (production-mount blocker, §8).

## 12. Known limits / NOT_RUN

- Everything is DESIGN; implementation, unit, real PG, HTTP, browser and independent
  review are NOT_RUN. Provider sandbox/live: NOT_APPLICABLE. Nothing may be recorded as
  PASS without the §9 evidence.
- Ingress is MOCK/manual only; no automatic comment matching, no DM/private reply (the
  operator copies the link by hand), no G07 contribution; no UI in this slice (T10b).
- No inventory hold at claim or redeem (arch §11.2); a claimed item may be sold out at
  BeginCheckout (T11). Prices shown are informational; Quote decides.
- No claim void/adjust by the merchant (T10d); buyer edits the cart. Keyword and SKU are
  immutable per offer; a typo is fixed by deactivating it and creating the right keyword
  for the same SKU (SKU uniqueness covers active offers only; KC04). Lines already
  accepted on the deactivated offer stay pending and are skipped; they apply only if that
  offer is reactivated, which first requires deactivating its replacement.
- Strict grammar rejects `A1 +2`, `A1+02`, emoji, interior spaces, Chinese numerals;
  merchants should avoid keywords shaped `<keyword>X<digits>` (`a1x2` hazard).
- One OPEN window per store; window close drops in-flight commands (fail-closed). A
  window may open on a DRAFT session (no broadcast state exists yet). v1 has no window
  interval history (T10c H5).
- Actor ≠ person: `max_quantity_per_claim` is per actor per offer. Manual labels are
  operator-chosen; two commenters with the same display name need distinct labels.
- Same key with a different new-actor label → 409 (keyed `label_mac`, §4.2). Rotating
  `COMMERCE_CLAIMS_LABEL_KEY` turns an in-flight same-key retry of a new-actor command
  into 409 (fail-closed; no key-ring in v1). A same-key retry after the merchant created the offer for
  a previously unknown keyword returns 409 (the canonical request carries `offer_id|""`).
- Several bundles of one owner claiming one SKU: last apply wins. A released owner keeps
  whatever is already in their cart.
- A pre-existing unavailable item in the buyer's cart blocks redeem with 409 until the
  buyer removes it (SetCart revalidates the whole cart).
- Retention purge, DSAR erasure, capacity/latency and hot-bundle contention are
  unmeasured; no performance claim. `live:read`/`live:manage` are not provisioned for
  existing memberships (0033); tests grant them explicitly.

## 13. SHOPLINE parity (SOURCE_NOT_VERIFIED — product knowledge, no fetched evidence)

| Capability | v1 decision |
| --- | --- |
| Keyword per variant, `KW+N` comments | Adopted; `+N` **sets**, never adds |
| "Contains keyword" matching | **Rejected** (arch §11.1; D3) |
| "+N only" mode | Adopted as `KEYWORD_QTY_ONLY` |
| Start/stop a product mid-live | `offer.active` + session window |
| Per-buyer limit | Partial: per actor per offer |
| Auto-lock stock on comment | Rejected for v1 (arch §11.2) |
| Auto Messenger reply with cart link | Deferred to T10c/T07 (G07); manual copy now |
| Live-only price, claim export, buyer tags | Deferred |

## Amendment by meta-claims-intake-v1 (integrator, 2026-09-29)

Append-only note; nothing above is edited. `meta-claims-intake-v1.md` §4.4 changes this
contract by clauses 1–10 (1 §1 window rule for `source_kind='meta'`; 2 §3 River wording; 3 §3.1
`RATE_LIMITED` row; 4 §3.2 KC03 matrix gains the §4.3 rows; 5 §3.3 `issue_system_link`
INSERT-only; 6 §4.3 `IngestInput`/`IngestResult`; 7 §4.3 `IngestMetaIntake` and KC15; 8 §6
system link row; 9 §3.3 filter wording for the no-GUC definers; 10 §3.2 closing paragraph
replaced). Clause 10 replaces the closing "No policy references another claims table … No
privilege for … role" sentences of §3.2 with the wording in meta-claims-intake-v1 §4.4 clause
10. KC03 change: subtest `foreign-roles-denied` drops `commerce_integration_writer` for
`SELECT` on `claims.events` only (its column SELECT from meta-claims-intake-v1 §4.3 makes the
query succeed). The implementing unit applies this amendment; the gate change is
integrator-approved at freeze of meta-claims-intake-v1 and is not an implementer rewrite of a
gate.

## Amendment by claims-retention-purge-v1 (integrator, 2026-09-30, U08 merge)

Recorded from `contracts/claims-retention-purge-v1.md` §6 (FROZEN 2026-09-30); that file is the source of the rows.

- Clause 1: §3 "no DELETE grant" and the §3.2 matrix (KC03 equality) gain the claims-retention-purge-v1 §4
  `commerce_retention_writer` rows (SELECT/UPDATE column sets on `claims.bundles`, `claims.lines`, `claims.links`,
  `live.claim_windows`; DELETE on `claims.links` only) and the `claims.bundles.purged_at` column,
  `bundle_purged_unbound`/`bundle_label_reserved` CHECKs and the `claims.links_not_purged` trigger. `purged_at` is not
  readable by `commerce_runtime`/`commerce_claims_intake`. Schema `claims` has 7 tables (+`retention_policy`,
  `retention_log`); the 7 U08 functions are owned by `commerce_retention_writer`. KC03 subtests are otherwise unchanged.
- Clause 2: §3.3 R4 "no other function or grant can clear `owner_id`" gains: "except the U08 definers, which clear it
  only together with deleting the bundle's link and setting `purged_at` (RD8: no link can be issued afterwards)".
  KC03's "no function EXECUTE-able by `commerce_runtime` or `commerce_buyer_runtime` can clear `owner_id` without
  replacing `token_hash`" stays true unchanged.
- Clause 3: §8/§11.4/§12 "production mount blocked by T14/U08" is replaced by "lifted per claims-retention-purge-v1
  §10" (all six conditions; until they hold, waiver W1 governs).
