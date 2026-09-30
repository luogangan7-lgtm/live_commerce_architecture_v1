-- 0060 live keyword claims (T10; contracts/live-keyword-claims-v1.md §3, FROZEN + §0.1 rulings).
--
-- Owns: live.offers and live.claim_windows (claim configuration beside a live session),
-- schema claims (bundles, lines, events, links), the NOLOGIN definer role
-- commerce_claims_writer and its four SECURITY DEFINER functions.
--
-- Non-goals: no inventory hold, cart/Quote/order/payment write, message, provider call,
-- River job kind, Meta ingestion (T10c), retention purge (T14), membership backfill, and
-- no change to social, meta_inbox, storefront, inventory, buyer or live.sessions/programs.
--
-- Depends on: live.sessions (0033) as the FK anchor of offers/windows only, so ingest's
-- hot-path FKs point at claim_windows/offers and never wait on UpdateDraft's session
-- FOR UPDATE; catalog.skus (0002) for the offer SKU; identity.memberships (0001) for
-- principal FKs; buyer.owners (0006) for the buyer binding; identity.resolve_access
-- (0003) and identity.sessions for DB-side merchant authority inside claims.issue_link
-- (live.request_media_stop pattern, 0037).
--
-- Callers: Go package internal/claims only. commerce_runtime (merchant transactions from
-- platform.WithScope) writes offers, windows, bundles, lines and events directly under
-- RLS and column grants; commerce_buyer_runtime has no table privilege here and reaches
-- claims only through preview_link, redeem_link and mark_applied.
--
-- Security decisions (why each exists):
--  * Binding (owner_id/bound_at), applied state (applied_version) and link hashes are
--    written only by commerce_claims_writer, which has no LOGIN and is reachable only
--    through the four functions below. Permissive RLS UPDATE policies are OR-ed by
--    PostgreSQL (§0.1 P2(a)), so buyer-vs-merchant separation is enforced by EXECUTE
--    grants + function code, and each writer WITH CHECK additionally repeats its own
--    BUYER/MERCHANT context so an OR of two policies can never mix contexts.
--  * No policy references another claims table (no subquery), so RLS recursion (42P17)
--    is impossible.
--  * Every definer pins search_path=pg_catalog, fully qualifies relations and filters
--    tenant/store explicitly in addition to RLS.
--  * Nothing here stores comment text, an unresolved keyword or any hash of either.

CREATE ROLE commerce_claims_writer NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
COMMENT ON ROLE commerce_claims_writer IS
 'T10 claims definer owner (migrations/0060). NOLOGIN; owns only claims.preview_link, redeem_link, mark_applied and issue_link. No runtime login may reach it (platform validatePoolAuthority object-owner rule).';

CREATE SCHEMA claims;
REVOKE ALL ON SCHEMA claims FROM PUBLIC;
GRANT USAGE ON SCHEMA claims TO commerce_runtime, commerce_buyer_runtime, commerce_claims_writer;
-- The writer reads offer keywords/state (live) and resolves merchant authority (identity).
GRANT USAGE ON SCHEMA live TO commerce_claims_writer;
GRANT USAGE ON SCHEMA identity TO commerce_claims_writer;
COMMENT ON SCHEMA claims IS
 'T10 live keyword claims (internal/claims). Merchant tables via commerce_runtime; buyer access only through claims definer functions.';

-- Offers: keyword -> SKU inside one session. Keyword and SKU are immutable (no UPDATE grant).
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
-- At most one active offer per SKU per session: a redeem never merges two claim lines.
CREATE UNIQUE INDEX live_offer_active_sku ON live.offers(tenant_id,store_id,session_id,sku_id) WHERE active;

-- One row per session; FK anchor for every claims row of the session.
CREATE TABLE live.claim_windows (
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
 -- closed_at IS NOT NULL is explicit: `closed_at>=opened_at` alone is NULL for a NULL
 -- closed_at and a CHECK treats NULL as satisfied (KC03 caught CLOSED gen>0 without closed_at).
-- One OPEN window per store: a manual operator cannot attribute to two sessions at once.
CREATE UNIQUE INDEX live_claim_window_one_open ON live.claim_windows(tenant_id,store_id) WHERE state='OPEN';

-- One bundle per (session, platform, actor). actor_key is opaque and never returned by any API.
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

-- One line per (bundle, offer) holding the absolute target quantity.
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

-- Append-only parse/decision facts; never comment text, unresolved keyword, label or actor key.
-- bundle_version is the §0.1 P2(c) addition: a redelivered source must return the bundle
-- version its ACCEPTED event produced, which cannot be re-derived once later lines commit.
CREATE TABLE claims.events (
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
 bundle_version bigint CHECK (bundle_version>0),
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
 CHECK ((outcome='ACCEPTED')=(bundle_version IS NOT NULL)),
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

-- Credential table: SHA-256 of the buyer link token only; no runtime role can SELECT token_hash.
CREATE TABLE claims.links (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, bundle_id uuid NOT NULL,
 token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
 generation bigint NOT NULL CHECK (generation>0),
 issued_at timestamptz NOT NULL,     -- no DEFAULT: issue_link sets both from one clock read
 expires_at timestamptz NOT NULL,
 principal_id uuid NOT NULL,
 PRIMARY KEY (tenant_id,store_id,bundle_id),
 FOREIGN KEY (tenant_id,store_id,bundle_id) REFERENCES claims.bundles(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 -- Link TTL is enforced by the database, not only by Go (§0.1 P2(g)).
 CHECK (expires_at>issued_at AND expires_at<=issued_at+interval '72 hours'));

ALTER TABLE live.offers ENABLE ROW LEVEL SECURITY;
ALTER TABLE live.offers FORCE ROW LEVEL SECURITY;
ALTER TABLE live.claim_windows ENABLE ROW LEVEL SECURITY;
ALTER TABLE live.claim_windows FORCE ROW LEVEL SECURITY;
ALTER TABLE claims.bundles ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims.bundles FORCE ROW LEVEL SECURITY;
ALTER TABLE claims.lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims.lines FORCE ROW LEVEL SECURITY;
ALTER TABLE claims.events ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims.events FORCE ROW LEVEL SECURITY;
ALTER TABLE claims.links ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims.links FORCE ROW LEVEL SECURITY;
REVOKE ALL ON live.offers, live.claim_windows, claims.bundles, claims.lines, claims.events, claims.links FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- commerce_runtime (merchant transactions). Scope S = app.tenant_id/app.store_id GUCs.
-- No DELETE anywhere; immutable identity, keyword, SKU, owner and creation columns have no
-- UPDATE grant.
-- ---------------------------------------------------------------------------------------
CREATE POLICY offer_read ON live.offers FOR SELECT TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY offer_insert ON live.offers FOR INSERT TO commerce_runtime WITH CHECK
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
-- UPDATE also admits ingest's FOR SHARE offer fence (row locks need UPDATE privilege + USING).
CREATE POLICY offer_update ON live.offers FOR UPDATE TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT, INSERT ON live.offers TO commerce_runtime;
GRANT UPDATE(max_quantity_per_claim,active,activated_at,version,updated_at) ON live.offers TO commerce_runtime;

CREATE POLICY window_read ON live.claim_windows FOR SELECT TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY window_insert ON live.claim_windows FOR INSERT TO commerce_runtime WITH CHECK
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
-- UPDATE also admits ingest's FOR SHARE window fence; a close waits for it.
CREATE POLICY window_update ON live.claim_windows FOR UPDATE TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
GRANT SELECT, INSERT ON live.claim_windows TO commerce_runtime;
GRANT UPDATE(state,match_mode,generation,opened_at,closed_at,version,principal_id,updated_at) ON live.claim_windows TO commerce_runtime;

CREATE POLICY bundle_read ON claims.bundles FOR SELECT TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY bundle_insert ON claims.bundles FOR INSERT TO commerce_runtime WITH CHECK
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid AND platform='manual');
CREATE POLICY bundle_update ON claims.bundles FOR UPDATE TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
-- The merchant sees "bound" (bound_at) but never which buyer (owner_id is not granted).
GRANT SELECT(tenant_id,store_id,id,session_id,platform,actor_key,label,bound_at,line_count,version,created_at,updated_at)
 ON claims.bundles TO commerce_runtime;
GRANT INSERT(tenant_id,store_id,session_id,platform,actor_key,label) ON claims.bundles TO commerce_runtime;
GRANT UPDATE(line_count,version,updated_at) ON claims.bundles TO commerce_runtime;

CREATE POLICY line_read ON claims.lines FOR SELECT TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY line_insert ON claims.lines FOR INSERT TO commerce_runtime WITH CHECK
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY line_update ON claims.lines FOR UPDATE TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
-- Runtime may read applied_version (merchant list shows "applied") but never write it.
GRANT SELECT ON claims.lines TO commerce_runtime;
GRANT INSERT(tenant_id,store_id,session_id,bundle_id,offer_id,sku_id,quantity,version,updated_at) ON claims.lines TO commerce_runtime;
GRANT UPDATE(quantity,version,updated_at) ON claims.lines TO commerce_runtime;

CREATE POLICY event_read ON claims.events FOR SELECT TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY event_insert ON claims.events FOR INSERT TO commerce_runtime WITH CHECK
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND source_kind='manual' AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
GRANT SELECT, INSERT ON claims.events TO commerce_runtime;

-- Link state for ListBundles only: never token_hash or issuing principal, no write.
CREATE POLICY link_read ON claims.links FOR SELECT TO commerce_runtime USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT(tenant_id,store_id,bundle_id,generation,issued_at,expires_at) ON claims.links TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- commerce_claims_writer (definer owner). BUYER = app.principal_id unset/'' AND
-- app.buyer_id set (buyer.WithScope); MERCHANT = app.principal_id set AND app.buyer_id
-- unset/'' (platform.WithScope). Every write policy names its context in both USING and
-- WITH CHECK, so OR-ed permissive policies cannot combine a buyer USING with a merchant
-- WITH CHECK (P2(a)); the function bodies are still the primary control.
-- ---------------------------------------------------------------------------------------
CREATE POLICY link_writer_read ON claims.links FOR SELECT TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY link_issue ON claims.links FOR INSERT TO commerce_claims_writer WITH CHECK
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NOT NULL
  AND nullif(current_setting('app.buyer_id',true),'')::uuid IS NULL
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY link_rotate ON claims.links FOR UPDATE TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NOT NULL
  AND nullif(current_setting('app.buyer_id',true),'')::uuid IS NULL)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NOT NULL
  AND nullif(current_setting('app.buyer_id',true),'')::uuid IS NULL
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
GRANT SELECT(tenant_id,store_id,bundle_id,token_hash,generation,expires_at) ON claims.links TO commerce_claims_writer;
GRANT INSERT(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id) ON claims.links TO commerce_claims_writer;
GRANT UPDATE(token_hash,generation,issued_at,expires_at,principal_id) ON claims.links TO commerce_claims_writer;

GRANT SELECT(token_hash,principal_id,audience,revoked_at,expires_at) ON identity.sessions TO commerce_claims_writer;
GRANT EXECUTE ON FUNCTION identity.resolve_access(bytea,uuid,text) TO commerce_claims_writer;

CREATE POLICY bundle_writer_read ON claims.bundles FOR SELECT TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
-- bind: a buyer transaction may lock/claim an unbound bundle or its own binding only.
CREATE POLICY bundle_bind ON claims.bundles FOR UPDATE TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NULL
  AND nullif(current_setting('app.buyer_id',true),'')::uuid IS NOT NULL
  AND (owner_id IS NULL OR owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid))
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NULL
  AND owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid);
-- release: only a merchant transaction may clear a binding (issue_link also rotates the hash).
CREATE POLICY bundle_release ON claims.bundles FOR UPDATE TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NOT NULL
  AND nullif(current_setting('app.buyer_id',true),'')::uuid IS NULL)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NOT NULL
  AND nullif(current_setting('app.buyer_id',true),'')::uuid IS NULL
  AND owner_id IS NULL AND bound_at IS NULL);
GRANT SELECT(tenant_id,store_id,id,session_id,owner_id,bound_at,version) ON claims.bundles TO commerce_claims_writer;
GRANT UPDATE(owner_id,bound_at) ON claims.bundles TO commerce_claims_writer;

CREATE POLICY line_writer_read ON claims.lines FOR SELECT TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY line_writer_update ON claims.lines FOR UPDATE TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT(tenant_id,store_id,bundle_id,offer_id,sku_id,quantity,version,applied_version) ON claims.lines TO commerce_claims_writer;
GRANT UPDATE(applied_version) ON claims.lines TO commerce_claims_writer;

CREATE POLICY offer_writer_read ON live.offers FOR SELECT TO commerce_claims_writer USING
 (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT(tenant_id,store_id,id,session_id,keyword,active) ON live.offers TO commerce_claims_writer;

-- ---------------------------------------------------------------------------------------
-- Buyer definers. Shared guard (repeated inline; a helper would be a fifth function):
-- READ COMMITTED; app.tenant_id/app.store_id/app.buyer_id/app.buyer_session_id are
-- canonical lowercase UUIDs and app.principal_id is exactly '' (only buyer.WithScope sets
-- that shape); hashes are 32 bytes. Violations raise 22023. "Not found" is zero rows and
-- is identical for unknown, expired, rotated, other-store, other-tenant and other-owner.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.preview_link(p_hash bytea)
RETURNS TABLE(bundle_version bigint, bound boolean, expires_at timestamptz, offer_id uuid,
 keyword text, sku_id uuid, quantity integer, pending boolean, offer_active boolean)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_tenant uuid; v_store uuid; v_buyer uuid;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR coalesce(current_setting('app.tenant_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.store_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.buyer_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.buyer_session_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR current_setting('app.principal_id',true) IS DISTINCT FROM ''
  OR p_hash IS NULL OR octet_length(p_hash)<>32 THEN
  RAISE EXCEPTION 'invalid claim link request' USING ERRCODE='22023';
 END IF;
 v_tenant:=current_setting('app.tenant_id')::uuid;
 v_store:=current_setting('app.store_id')::uuid;
 v_buyer:=current_setting('app.buyer_id')::uuid;
 -- Read-only projection: no lock, no write. Expired links and other owners' bindings
 -- produce the same zero rows as an unknown hash.
 RETURN QUERY
 SELECT b.version, coalesce(b.owner_id=v_buyer,false), k.expires_at, l.offer_id, o.keyword,
  l.sku_id, l.quantity, l.applied_version IS DISTINCT FROM l.version, o.active
 FROM claims.links k
 JOIN claims.bundles b ON b.tenant_id=k.tenant_id AND b.store_id=k.store_id AND b.id=k.bundle_id
 JOIN claims.lines l ON l.tenant_id=b.tenant_id AND l.store_id=b.store_id AND l.bundle_id=b.id
 JOIN live.offers o ON o.tenant_id=l.tenant_id AND o.store_id=l.store_id AND o.id=l.offer_id
 WHERE k.token_hash=p_hash AND k.tenant_id=v_tenant AND k.store_id=v_store
  AND b.tenant_id=v_tenant AND b.store_id=v_store
  AND k.expires_at>clock_timestamp()
  AND (b.owner_id IS NULL OR b.owner_id=v_buyer)
 ORDER BY o.keyword;
END $$;
ALTER FUNCTION claims.preview_link(bytea) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.preview_link(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.preview_link(bytea) TO commerce_buyer_runtime;
COMMENT ON FUNCTION claims.preview_link(bytea) IS
 'internal/claims.PreviewLink only; EXECUTE: commerce_buyer_runtime. Read-only link preview; zero rows = uniform not found.';

CREATE FUNCTION claims.redeem_link(p_hash bytea, p_expected_version bigint)
RETURNS TABLE(bundle_id uuid, bundle_version bigint, offer_id uuid, sku_id uuid, quantity integer,
 line_version bigint, pending boolean, offer_active boolean)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_tenant uuid; v_store uuid; v_buyer uuid; v_bundle uuid; v_owner uuid; v_version bigint;
BEGIN
 -- Same buyer guard as claims.preview_link, plus a non-NULL expected version.
 IF current_setting('transaction_isolation')<>'read committed'
  OR coalesce(current_setting('app.tenant_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.store_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.buyer_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.buyer_session_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR current_setting('app.principal_id',true) IS DISTINCT FROM ''
  OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_expected_version IS NULL THEN
  RAISE EXCEPTION 'invalid claim link request' USING ERRCODE='22023';
 END IF;
 v_tenant:=current_setting('app.tenant_id')::uuid;
 v_store:=current_setting('app.store_id')::uuid;
 v_buyer:=current_setting('app.buyer_id')::uuid;
 -- 1. Locate the bundle without a lock (the link row itself is never locked here).
 SELECT k.bundle_id INTO v_bundle FROM claims.links k
  WHERE k.token_hash=p_hash AND k.tenant_id=v_tenant AND k.store_id=v_store
   AND k.expires_at>clock_timestamp();
 IF NOT FOUND THEN RETURN; END IF;
 -- 2. Lock the bundle. bundle_bind USING hides another owner's binding, so a concurrent
 --    second owner waits here and then sees zero rows.
 SELECT b.owner_id, b.version INTO v_owner, v_version FROM claims.bundles b
  WHERE b.tenant_id=v_tenant AND b.store_id=v_store AND b.id=v_bundle FOR NO KEY UPDATE;
 IF NOT FOUND OR (v_owner IS NOT NULL AND v_owner<>v_buyer) THEN RETURN; END IF;
 -- 3. Re-read the link under the bundle lock: a rotation or release that committed while
 --    we waited replaced the hash, so the old token is now not found.
 PERFORM 1 FROM claims.links k
  WHERE k.token_hash=p_hash AND k.tenant_id=v_tenant AND k.store_id=v_store
   AND k.bundle_id=v_bundle AND k.expires_at>clock_timestamp();
 IF NOT FOUND THEN RETURN; END IF;
 -- 4. Not-found always precedes the version check, so PT409 never proves existence.
 IF v_version<>p_expected_version THEN
  RAISE EXCEPTION 'claim bundle changed' USING ERRCODE='PT409';
 END IF;
 -- 5. First redeem binds the caller; it never marks anything applied (mark_applied does).
 IF v_owner IS NULL THEN
  UPDATE claims.bundles b SET owner_id=v_buyer, bound_at=clock_timestamp()
   WHERE b.tenant_id=v_tenant AND b.store_id=v_store AND b.id=v_bundle;
 END IF;
 RETURN QUERY
 SELECT l.bundle_id, v_version, l.offer_id, l.sku_id, l.quantity, l.version,
  l.applied_version IS DISTINCT FROM l.version, o.active
 FROM claims.lines l
 JOIN live.offers o ON o.tenant_id=l.tenant_id AND o.store_id=l.store_id AND o.id=l.offer_id
 WHERE l.tenant_id=v_tenant AND l.store_id=v_store AND l.bundle_id=v_bundle
 ORDER BY l.offer_id
 FOR NO KEY UPDATE OF l;
END $$;
ALTER FUNCTION claims.redeem_link(bytea,bigint) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.redeem_link(bytea,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.redeem_link(bytea,bigint) TO commerce_buyer_runtime;
COMMENT ON FUNCTION claims.redeem_link(bytea,bigint) IS
 'internal/claims.RedeemLink only; EXECUTE: commerce_buyer_runtime. Binds the first owner, CAS on bundle version (PT409), locks and returns lines.';

CREATE FUNCTION claims.mark_applied(p_bundle uuid, p_offers uuid[], p_versions bigint[])
RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_tenant uuid; v_store uuid; v_buyer uuid; v_count integer;
BEGIN
 -- Same buyer guard as claims.preview_link; arrays are equal-length 1..50, NULL-free,
 -- one-dimensional and without duplicate offers.
 IF current_setting('transaction_isolation')<>'read committed'
  OR coalesce(current_setting('app.tenant_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.store_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.buyer_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.buyer_session_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR current_setting('app.principal_id',true) IS DISTINCT FROM ''
  OR p_bundle IS NULL OR p_offers IS NULL OR p_versions IS NULL
  OR array_ndims(p_offers)<>1 OR array_ndims(p_versions)<>1
  OR cardinality(p_offers) NOT BETWEEN 1 AND 50 OR cardinality(p_offers)<>cardinality(p_versions)
  OR EXISTS (SELECT 1 FROM unnest(p_offers) x WHERE x IS NULL)
  OR EXISTS (SELECT 1 FROM unnest(p_versions) x WHERE x IS NULL)
  OR (SELECT count(DISTINCT x) FROM unnest(p_offers) x)<>cardinality(p_offers) THEN
  RAISE EXCEPTION 'invalid claim apply request' USING ERRCODE='22023';
 END IF;
 v_tenant:=current_setting('app.tenant_id')::uuid;
 v_store:=current_setting('app.store_id')::uuid;
 v_buyer:=current_setting('app.buyer_id')::uuid;
 -- Only the bound owner marks lines; RedeemLink already holds this lock (re-entrant).
 PERFORM 1 FROM claims.bundles b
  WHERE b.tenant_id=v_tenant AND b.store_id=v_store AND b.id=p_bundle AND b.owner_id=v_buyer
  FOR NO KEY UPDATE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'claim bundle not bound to caller' USING ERRCODE='PT409';
 END IF;
 -- Mark exactly the versions the caller applied; a newer accepted command stays pending.
 UPDATE claims.lines l SET applied_version=l.version
  FROM unnest(p_offers,p_versions) AS u(offer_id,version)
  WHERE l.tenant_id=v_tenant AND l.store_id=v_store AND l.bundle_id=p_bundle
   AND l.offer_id=u.offer_id AND l.version=u.version;
 GET DIAGNOSTICS v_count=ROW_COUNT;
 IF v_count<>cardinality(p_offers) THEN
  RAISE EXCEPTION 'claim lines changed' USING ERRCODE='PT409';
 END IF;
 RETURN v_count;
END $$;
ALTER FUNCTION claims.mark_applied(uuid,uuid[],bigint[]) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.mark_applied(uuid,uuid[],bigint[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.mark_applied(uuid,uuid[],bigint[]) TO commerce_buyer_runtime;
COMMENT ON FUNCTION claims.mark_applied(uuid,uuid[],bigint[]) IS
 'internal/claims.RedeemLink only (after storefront.SetCart); EXECUTE: commerce_buyer_runtime. Sets applied_version=version for exactly the given lines or raises PT409.';

-- ---------------------------------------------------------------------------------------
-- Merchant definer. Authority is decided in the database (0037 request_media_stop
-- pattern): resolve_access before any lock, GUC equality, and a fresh resolve_access plus
-- session-expiry read at clock_timestamp() after the last write. The only path that can
-- clear owner_id replaces token_hash in the same call, so release without rotation is
-- impossible.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.issue_link(p_auth_hash bytea, p_store uuid, p_session uuid, p_bundle uuid,
 p_expected_generation bigint, p_new_hash bytea, p_release boolean)
RETURNS TABLE(generation bigint, expires_at timestamptz, released boolean)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_access record; v_final record; v_tenant uuid; v_principal uuid; v_revision bigint;
 v_found boolean; v_owner uuid; v_current bigint; v_now timestamptz; v_expiry timestamptz;
 v_released boolean:=false;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR coalesce(current_setting('app.tenant_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.store_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.principal_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.authz_revision',true),'') !~ '^[1-9][0-9]{0,17}$'
  OR coalesce(current_setting('app.buyer_id',true),'')<>''
  OR p_auth_hash IS NULL OR octet_length(p_auth_hash)<>32
  OR p_new_hash IS NULL OR octet_length(p_new_hash)<>32
  OR p_store IS NULL OR p_session IS NULL OR p_bundle IS NULL
  OR p_store='00000000-0000-0000-0000-000000000000'::uuid
  OR p_session='00000000-0000-0000-0000-000000000000'::uuid
  OR p_bundle='00000000-0000-0000-0000-000000000000'::uuid
  OR p_expected_generation IS NULL OR p_expected_generation<0
  OR p_expected_generation>=9223372036854775807 OR p_release IS NULL THEN
  RAISE EXCEPTION 'invalid claim link issue' USING ERRCODE='22023';
 END IF;
 -- Authorize before any lock; the caller's GUCs must be exactly the resolved scope.
 SELECT * INTO v_access FROM identity.resolve_access(p_auth_hash,p_store,'live:manage');
 IF v_access.access_status='unauthorized' THEN
  RAISE EXCEPTION 'claim access unavailable' USING ERRCODE='PT401';
 END IF;
 IF v_access.access_status='not_found' THEN
  RAISE EXCEPTION 'claim access unavailable' USING ERRCODE='PT404';
 END IF;
 IF v_access.access_status<>'ok' THEN
  RAISE EXCEPTION 'claim access unavailable' USING ERRCODE='PT403';
 END IF;
 v_tenant:=v_access.tenant_id; v_principal:=v_access.principal_id; v_revision:=v_access.authz_revision;
 IF v_tenant IS DISTINCT FROM current_setting('app.tenant_id')::uuid
  OR p_store IS DISTINCT FROM current_setting('app.store_id')::uuid
  OR v_principal IS DISTINCT FROM current_setting('app.principal_id')::uuid
  OR v_revision IS DISTINCT FROM current_setting('app.authz_revision')::bigint THEN
  RAISE EXCEPTION 'claim access unavailable' USING ERRCODE='PT403';
 END IF;
 -- Lock the bundle of this session (bundle_release USING admits merchant context only).
 SELECT b.owner_id INTO v_owner FROM claims.bundles b
  WHERE b.tenant_id=v_tenant AND b.store_id=p_store AND b.session_id=p_session AND b.id=p_bundle
  FOR NO KEY UPDATE;
 v_found:=FOUND;
 IF v_found THEN
  SELECT k.generation INTO v_current FROM claims.links k
   WHERE k.tenant_id=v_tenant AND k.store_id=p_store AND k.bundle_id=p_bundle;
  IF coalesce(v_current,0)<>p_expected_generation THEN
   RAISE EXCEPTION 'claim link generation changed' USING ERRCODE='PT409';
  END IF;
  -- One clock read: issued_at and expires_at always differ by exactly 72 hours.
  v_now:=clock_timestamp();
  IF p_release AND v_owner IS NOT NULL THEN
   UPDATE claims.bundles b SET owner_id=NULL, bound_at=NULL
    WHERE b.tenant_id=v_tenant AND b.store_id=p_store AND b.id=p_bundle;
   -- The next owner receives every line again; the released owner's cart is untouched.
   UPDATE claims.lines l SET applied_version=NULL
    WHERE l.tenant_id=v_tenant AND l.store_id=p_store AND l.bundle_id=p_bundle;
   v_released:=true;
  END IF;
  -- Every issue replaces the hash (old token dies) and resets both timestamps. The
  -- DO UPDATE branch assigns the same five values from variables, not EXCLUDED.*: reading
  -- EXCLUDED columns needs SELECT on them, and the writer deliberately cannot read
  -- issued_at or principal_id (§3.2 matrix).
  INSERT INTO claims.links (tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id)
  VALUES (v_tenant,p_store,p_bundle,p_new_hash,p_expected_generation+1,v_now,v_now+interval '72 hours',v_principal)
  ON CONFLICT (tenant_id,store_id,bundle_id) DO UPDATE SET token_hash=p_new_hash,
   generation=p_expected_generation+1, issued_at=v_now, expires_at=v_now+interval '72 hours',
   principal_id=v_principal;
 END IF;
 -- Final authority after every lock wait and write (also on the not-found path).
 SELECT * INTO v_final FROM identity.resolve_access(p_auth_hash,p_store,'live:manage');
 SELECT s.expires_at INTO v_expiry FROM identity.sessions s
  WHERE s.token_hash=p_auth_hash AND s.audience='merchant' AND s.revoked_at IS NULL;
 IF v_final.access_status='unauthorized' OR v_expiry IS NULL OR v_expiry<=clock_timestamp() THEN
  RAISE EXCEPTION 'claim access unavailable' USING ERRCODE='PT401';
 END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM v_tenant
  OR v_final.principal_id IS DISTINCT FROM v_principal
  OR v_final.authz_revision IS DISTINCT FROM v_revision THEN
  RAISE EXCEPTION 'claim access unavailable' USING ERRCODE='PT403';
 END IF;
 IF v_found THEN
  RETURN QUERY SELECT p_expected_generation+1, v_now+interval '72 hours', v_released;
 END IF;
END $$;
ALTER FUNCTION claims.issue_link(bytea,uuid,uuid,uuid,bigint,bytea,boolean) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.issue_link(bytea,uuid,uuid,uuid,bigint,bytea,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.issue_link(bytea,uuid,uuid,uuid,bigint,bytea,boolean) TO commerce_runtime;
COMMENT ON FUNCTION claims.issue_link(bytea,uuid,uuid,uuid,bigint,bytea,boolean) IS
 'internal/claims.IssueLink only; EXECUTE: commerce_runtime. Re-resolves live:manage in the DB, CAS on link generation, rotates the token hash and optionally releases the binding (never one without the other). Zero rows = not found.';

COMMENT ON TABLE live.offers IS
 'internal/claims: keyword->SKU offer per live session (keyword/SKU immutable). RW: commerce_runtime; R: commerce_claims_writer.';
COMMENT ON TABLE live.claim_windows IS
 'internal/claims: per-session claim window (CLOSED/OPEN, match mode, generation); FK anchor of claims.*. RW: commerce_runtime.';
COMMENT ON TABLE claims.bundles IS
 'internal/claims: one claim bundle per (session, platform, actor). RW: commerce_runtime (no owner_id); binding: commerce_claims_writer definers only.';
COMMENT ON TABLE claims.lines IS
 'internal/claims: absolute target quantity per (bundle, offer). RW: commerce_runtime; applied_version: commerce_claims_writer definers only.';
COMMENT ON TABLE claims.events IS
 'internal/claims: append-only parse/decision facts (no text, unresolved keyword, label or actor key). Insert/read: commerce_runtime.';
COMMENT ON TABLE claims.links IS
 'internal/claims: SHA-256 of the buyer claim-link token (72 h TTL). Write/hash read: commerce_claims_writer definers only; runtime reads link state columns.';
COMMENT ON COLUMN claims.bundles.actor_key IS 'Pseudonymous personal data (arch §14.4): opaque 64-hex; never returned by any API or copied into events. Retention class: actor keys (T14).';
COMMENT ON COLUMN claims.bundles.label IS 'Merchant-entered personal data (manual only): merchant live:read only; never in receipts, audit, logs, URLs or buyer projections. Retention class: manual labels (T14).';
COMMENT ON COLUMN claims.bundles.owner_id IS 'Buyer binding: written only by claims.redeem_link (bind) and claims.issue_link (release+rotate); not readable by commerce_runtime. Retention class: bindings (T14).';
COMMENT ON COLUMN claims.links.token_hash IS 'SHA-256 of the link token; the token itself is never stored. Readable only by commerce_claims_writer. Retention class: link hashes (T14).';
COMMENT ON COLUMN claims.events.bundle_version IS 'Bundle version produced by this ACCEPTED event, so a duplicate ingest returns a fully reconstructible result (contract §0.1 P2(c)).';
