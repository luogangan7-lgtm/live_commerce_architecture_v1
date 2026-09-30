-- 0070 merchant password auth (contracts/merchant-password-auth-v1.md §4, v1 FROZEN 2026-09-30).
-- Owning package: internal/identity (password.go, challenge.go, throttle.go). Only the
-- commerce_identity login (internal/platform.OpenIdentityPool) may EXECUTE the definers below.
-- Non-goals: no tenant/store data, no RLS (ruling R-3: identity tables are not tenant rows,
-- isolation is by privilege exactly as in 0004), no plaintext code or password anywhere.
--
-- The four tables are owned by the migration role (no ALTER TABLE .. OWNER, verified against
-- 0004); commerce_identity_writer receives only the explicit column-level grants at the end.
-- Lock order everywhere: email advisory lock -> principal -> credential -> challenge -> sessions.

CREATE TABLE identity.password_credentials (
    principal_id uuid PRIMARY KEY REFERENCES identity.principals(id),
    email text NOT NULL UNIQUE CHECK (length(email) BETWEEN 3 AND 254 AND email ~ '^[!-~]+$'
        AND email = lower(email) AND email ~ '^[^@]+@[^@]+$'),
    password_hash text NOT NULL CHECK (password_hash ~ '^\$argon2id\$v=19\$m=[0-9]{4,7},t=[0-9]{1,2},p=[0-9]{1,2}\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$'),
    password_version bigint NOT NULL DEFAULT 1 CHECK (password_version > 0),
    failed_count int NOT NULL DEFAULT 0 CHECK (failed_count BETWEEN 0 AND 100),
    disabled_at timestamptz,
    email_verified_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    password_changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE identity.email_challenges (
    id uuid PRIMARY KEY,
    purpose text NOT NULL CHECK (purpose IN ('signup','login','reset')),
    email text NOT NULL CHECK (length(email) BETWEEN 3 AND 254 AND email ~ '^[!-~]+$'
        AND email = lower(email) AND email ~ '^[^@]+@[^@]+$'),
    locale text NOT NULL CHECK (locale IN ('zh-CN','zh-TW','en')),
    principal_id uuid REFERENCES identity.principals(id),
    password_version bigint,
    pending_password_hash text CHECK (pending_password_hash IS NULL OR pending_password_hash ~
        '^\$argon2id\$v=19\$m=[0-9]{4,7},t=[0-9]{1,2},p=[0-9]{1,2}\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$'),
    binding_hash bytea NOT NULL UNIQUE CHECK (octet_length(binding_hash) = 32),
    code_hmac bytea NOT NULL CHECK (octet_length(code_hmac) = 32),
    ip_hmac bytea NOT NULL CHECK (octet_length(ip_hmac) = 32),
    attempts smallint NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    consumed_reason text CHECK (consumed_reason IN ('verified','superseded','exhausted')),
    mail_state text NOT NULL DEFAULT 'PENDING' CHECK (mail_state IN ('PENDING','SENT','FAILED','UNKNOWN')),
    provider_message_id text CHECK (length(provider_message_id) <= 128),
    -- Purpose shape (§4.1). A consumed signup row has its pending hash nulled (§4.1 last bullet),
    -- so "pending NOT NULL" is required only while the challenge is still open.
    CONSTRAINT email_challenges_shape CHECK (
        (purpose = 'signup' AND principal_id IS NULL AND password_version IS NULL
            AND (pending_password_hash IS NOT NULL OR consumed_at IS NOT NULL))
        OR (purpose = 'login' AND principal_id IS NOT NULL AND password_version IS NOT NULL AND pending_password_hash IS NULL)
        OR (purpose = 'reset' AND principal_id IS NOT NULL AND password_version IS NULL AND pending_password_hash IS NULL)),
    CONSTRAINT email_challenges_consumed CHECK ((consumed_at IS NULL) = (consumed_reason IS NULL))
);
CREATE INDEX email_challenges_open ON identity.email_challenges(email, purpose) WHERE consumed_at IS NULL;
CREATE INDEX email_challenges_expiry ON identity.email_challenges(expires_at);
CREATE INDEX email_challenges_reset_budget ON identity.email_challenges(principal_id, created_at) WHERE purpose = 'reset';

CREATE TABLE identity.auth_throttle (
    bucket bytea NOT NULL CHECK (octet_length(bucket) = 32),
    window_start timestamptz NOT NULL,
    hits int NOT NULL CHECK (hits > 0),
    PRIMARY KEY (bucket, window_start)
);
CREATE INDEX auth_throttle_window ON identity.auth_throttle(window_start);

CREATE TABLE identity.auth_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid REFERENCES identity.principals(id),
    email_hmac bytea CHECK (octet_length(email_hmac) = 32),
    ip_hmac bytea CHECK (octet_length(ip_hmac) = 32),
    challenge_id uuid,
    action text NOT NULL CHECK (action IN ('signup.requested','signup.exists_notified','signup.verified',
        'login.password_failed','login.password_ok','login.disabled','login.verified','reset.requested',
        'reset.completed','code.failed','code.exhausted','throttled','mail.sent','mail.failed','mail.unknown',
        'password.breach_check_unavailable','operator.unlocked')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX auth_events_created ON identity.auth_events(created_at);

-- ---------------------------------------------------------------------------------------------
-- Definers. All SECURITY DEFINER, owner commerce_identity_writer, search_path = pg_catalog.
-- `#variable_conflict use_column`: RETURNS TABLE column names (outcome, expires_at) would
-- otherwise shadow same-named table columns inside the bodies.
-- ---------------------------------------------------------------------------------------------

CREATE FUNCTION identity.auth_throttle_hit(p_bucket bytea, p_window_seconds int, p_offset_seconds int)
RETURNS int LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
DECLARE v_now timestamptz := clock_timestamp(); v_start timestamptz; v_hits int; v_key bytea;
BEGIN
    IF p_bucket IS NULL OR octet_length(p_bucket) <> 32 OR p_window_seconds IS NULL OR p_window_seconds NOT BETWEEN 1 AND 86400
       OR p_offset_seconds IS NULL OR p_offset_seconds NOT BETWEEN 0 AND 86399 THEN
        RAISE EXCEPTION 'invalid throttle request' USING ERRCODE = 'PT400';
    END IF;
    -- Fixed window. p_offset_seconds is the zone offset east of UTC (0, or 28800 for the UTC+8 finance
    -- day), so a 86400 s window with 28800 starts at 16:00 UTC = 00:00 UTC+8 (ruling Q11).
    v_start := to_timestamp((floor((extract(epoch FROM v_now) + p_offset_seconds) / p_window_seconds) * p_window_seconds - p_offset_seconds)::double precision);
    -- F1 (2026-09-30): the stored key is sha256(p_bucket || window length || offset), so two windows of one
    -- bucket that start at the same instant (60 s + 1 h in the first minute of an hour; 1 h + day in the
    -- first hour of a UTC+8 day) never share a row and each counts its own hits. Still 32 bytes; callers
    -- keep passing the bucket HMAC and never see the derived key.
    v_key := sha256(p_bucket || int4send(p_window_seconds) || int4send(p_offset_seconds));
    INSERT INTO identity.auth_throttle(bucket, window_start, hits) VALUES (v_key, v_start, 1)
    ON CONFLICT (bucket, window_start) DO UPDATE SET hits = identity.auth_throttle.hits + 1
    RETURNING identity.auth_throttle.hits INTO v_hits;
    -- I23 bounded purge: <= 100 rows per table per call, deterministic order so concurrent purges
    -- lock rows in the same order and cannot deadlock.
    DELETE FROM identity.auth_throttle t USING (
        SELECT a.bucket, a.window_start FROM identity.auth_throttle a
        WHERE a.window_start < v_now - interval '2 days' ORDER BY a.window_start, a.bucket LIMIT 100) d
    WHERE t.bucket = d.bucket AND t.window_start = d.window_start;
    DELETE FROM identity.email_challenges e USING (
        SELECT a.id FROM identity.email_challenges a
        WHERE a.expires_at < v_now - interval '1 day' ORDER BY a.expires_at, a.id LIMIT 100) d
    WHERE e.id = d.id;
    DELETE FROM identity.auth_events e USING (
        SELECT a.id FROM identity.auth_events a
        WHERE a.created_at < v_now - interval '180 days' ORDER BY a.created_at, a.id LIMIT 100) d
    WHERE e.id = d.id;
    RETURN v_hits;
END $$;

CREATE FUNCTION identity.start_signup_challenge(p_id uuid, p_email text, p_hash text, p_binding bytea, p_code bytea, p_locale text, p_ip bytea)
RETURNS TABLE(outcome text, expires_at timestamptz) LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
DECLARE v_expiry timestamptz; v_existing uuid;
BEGIN
    IF p_id IS NULL OR p_email IS NULL OR p_hash IS NULL OR p_binding IS NULL OR octet_length(p_binding) <> 32
       OR p_code IS NULL OR octet_length(p_code) <> 32 OR p_ip IS NULL OR octet_length(p_ip) <> 32
       OR p_locale IS NULL OR p_locale NOT IN ('zh-CN','zh-TW','en') THEN
        RAISE EXCEPTION 'invalid challenge request' USING ERRCODE = 'PT400';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('lc:pw-email:' || p_email, 0));
    SELECT c.principal_id INTO v_existing FROM identity.password_credentials c WHERE c.email = p_email;
    IF FOUND THEN
        -- The email is taken: no challenge row, only an audit row; Go sends the "account exists" notice.
        INSERT INTO identity.auth_events(principal_id, ip_hmac, action) VALUES (v_existing, p_ip, 'signup.exists_notified');
        RETURN QUERY SELECT 'EXISTS'::text, NULL::timestamptz;
        RETURN;
    END IF;
    -- PD4: only the same source supersedes; another source cannot kill the victim's open code.
    UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'superseded', pending_password_hash = NULL
     WHERE c.email = p_email AND c.purpose = 'signup' AND c.consumed_at IS NULL AND c.expires_at > clock_timestamp() AND c.ip_hmac = p_ip;
    LOOP
        EXIT WHEN (SELECT count(*) FROM identity.email_challenges c WHERE c.email = p_email AND c.purpose = 'signup'
                     AND c.consumed_at IS NULL AND c.expires_at > clock_timestamp()) < 3;
        UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'superseded', pending_password_hash = NULL
         WHERE c.id = (SELECT o.id FROM identity.email_challenges o WHERE o.email = p_email AND o.purpose = 'signup'
                         AND o.consumed_at IS NULL AND o.expires_at > clock_timestamp() ORDER BY o.created_at, o.id LIMIT 1);
    END LOOP;
    INSERT INTO identity.email_challenges(id, purpose, email, locale, pending_password_hash, binding_hash, code_hmac, ip_hmac, expires_at)
    VALUES (p_id, 'signup', p_email, p_locale, p_hash, p_binding, p_code, p_ip, clock_timestamp() + interval '10 minutes')
    RETURNING identity.email_challenges.expires_at INTO v_expiry;
    INSERT INTO identity.auth_events(ip_hmac, challenge_id, action) VALUES (p_ip, p_id, 'signup.requested');
    RETURN QUERY SELECT 'CHALLENGE'::text, v_expiry;
END $$;

CREATE FUNCTION identity.password_login_material(p_email text)
RETURNS TABLE(password_hash text, password_version bigint, disabled boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT c.password_hash, c.password_version, c.disabled_at IS NOT NULL
      FROM identity.password_credentials c JOIN identity.principals p ON p.id = c.principal_id
     WHERE c.email = p_email AND p.active
$$;

CREATE FUNCTION identity.record_password_failure(p_email text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
DECLARE v_principal uuid; v_count int; v_disabled timestamptz; v_newly boolean;
BEGIN
    SELECT c.principal_id, c.failed_count, c.disabled_at INTO v_principal, v_count, v_disabled
      FROM identity.password_credentials c WHERE c.email = p_email FOR UPDATE;
    IF NOT FOUND THEN RETURN; END IF;  -- unknown email: no-op, never raises (PD6)
    -- R2-1: the counter saturates at 100 so the 101st wrong password never violates the CHECK.
    v_newly := v_disabled IS NULL AND v_count + 1 >= 100;
    UPDATE identity.password_credentials c SET failed_count = LEAST(v_count + 1, 100),
           disabled_at = CASE WHEN v_newly THEN clock_timestamp() ELSE v_disabled END
     WHERE c.principal_id = v_principal;
    INSERT INTO identity.auth_events(principal_id, action) VALUES (v_principal, 'login.password_failed');
    IF v_newly THEN
        INSERT INTO identity.auth_events(principal_id, action) VALUES (v_principal, 'login.disabled');
    END IF;
END $$;

CREATE FUNCTION identity.start_login_challenge(p_id uuid, p_email text, p_version bigint, p_binding bytea, p_code bytea, p_locale text, p_ip bytea)
RETURNS TABLE(expires_at timestamptz, has_membership boolean) LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
DECLARE v_principal uuid; v_active boolean; v_version bigint; v_disabled timestamptz; v_expiry timestamptz; v_member boolean;
BEGIN
    IF p_id IS NULL OR p_email IS NULL OR p_version IS NULL OR p_binding IS NULL OR octet_length(p_binding) <> 32
       OR p_code IS NULL OR octet_length(p_code) <> 32 OR p_ip IS NULL OR octet_length(p_ip) <> 32
       OR p_locale IS NULL OR p_locale NOT IN ('zh-CN','zh-TW','en') THEN
        RAISE EXCEPTION 'invalid challenge request' USING ERRCODE = 'PT400';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('lc:pw-email:' || p_email, 0));
    SELECT c.principal_id INTO v_principal FROM identity.password_credentials c WHERE c.email = p_email;
    IF NOT FOUND THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE = 'PT401'; END IF;
    SELECT p.active INTO v_active FROM identity.principals p WHERE p.id = v_principal FOR UPDATE;
    SELECT c.password_version, c.disabled_at INTO v_version, v_disabled
      FROM identity.password_credentials c WHERE c.principal_id = v_principal FOR UPDATE;
    IF NOT FOUND OR NOT v_active OR v_disabled IS NOT NULL OR v_version <> p_version THEN
        RAISE EXCEPTION 'unauthorized' USING ERRCODE = 'PT401';
    END IF;
    -- PD4: the caller proved the password, so every older open login challenge is superseded.
    UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'superseded'
     WHERE c.principal_id = v_principal AND c.purpose = 'login' AND c.consumed_at IS NULL AND c.expires_at > clock_timestamp();
    INSERT INTO identity.email_challenges(id, purpose, email, locale, principal_id, password_version, binding_hash, code_hmac, ip_hmac, expires_at)
    VALUES (p_id, 'login', p_email, p_locale, v_principal, p_version, p_binding, p_code, p_ip, clock_timestamp() + interval '10 minutes')
    RETURNING identity.email_challenges.expires_at INTO v_expiry;
    -- §4.2: an active membership of the principal in an active tenant (both readable by the writer, no RLS).
    v_member := EXISTS (SELECT 1 FROM identity.memberships m JOIN control.tenants t ON t.id = m.tenant_id
                         WHERE m.principal_id = v_principal AND m.active AND t.active);
    INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action) VALUES (v_principal, p_ip, p_id, 'login.password_ok');
    RETURN QUERY SELECT v_expiry, v_member;
END $$;

CREATE FUNCTION identity.start_reset_challenge(p_id uuid, p_email text, p_binding bytea, p_code bytea, p_locale text, p_ip bytea)
RETURNS TABLE(outcome text, expires_at timestamptz) LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
DECLARE v_principal uuid; v_active boolean; v_expiry timestamptz;
BEGIN
    IF p_id IS NULL OR p_email IS NULL OR p_binding IS NULL OR octet_length(p_binding) <> 32
       OR p_code IS NULL OR octet_length(p_code) <> 32 OR p_ip IS NULL OR octet_length(p_ip) <> 32
       OR p_locale IS NULL OR p_locale NOT IN ('zh-CN','zh-TW','en') THEN
        RAISE EXCEPTION 'invalid challenge request' USING ERRCODE = 'PT400';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('lc:pw-email:' || p_email, 0));
    SELECT c.principal_id INTO v_principal FROM identity.password_credentials c WHERE c.email = p_email;
    IF NOT FOUND THEN RETURN QUERY SELECT 'NONE'::text, NULL::timestamptz; RETURN; END IF;
    SELECT p.active INTO v_active FROM identity.principals p WHERE p.id = v_principal FOR UPDATE;
    IF NOT v_active THEN RETURN QUERY SELECT 'NONE'::text, NULL::timestamptz; RETURN; END IF;
    -- PD4: same-source supersede only, then at most 3 open (oldest superseded).
    UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'superseded'
     WHERE c.principal_id = v_principal AND c.purpose = 'reset' AND c.consumed_at IS NULL AND c.expires_at > clock_timestamp() AND c.ip_hmac = p_ip;
    LOOP
        EXIT WHEN (SELECT count(*) FROM identity.email_challenges c WHERE c.principal_id = v_principal AND c.purpose = 'reset'
                     AND c.consumed_at IS NULL AND c.expires_at > clock_timestamp()) < 3;
        UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'superseded'
         WHERE c.id = (SELECT o.id FROM identity.email_challenges o WHERE o.principal_id = v_principal AND o.purpose = 'reset'
                         AND o.consumed_at IS NULL AND o.expires_at > clock_timestamp() ORDER BY o.created_at, o.id LIMIT 1);
    END LOOP;
    INSERT INTO identity.email_challenges(id, purpose, email, locale, principal_id, binding_hash, code_hmac, ip_hmac, expires_at)
    VALUES (p_id, 'reset', p_email, p_locale, v_principal, p_binding, p_code, p_ip, clock_timestamp() + interval '10 minutes')
    RETURNING identity.email_challenges.expires_at INTO v_expiry;
    INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action) VALUES (v_principal, p_ip, p_id, 'reset.requested');
    RETURN QUERY SELECT 'CHALLENGE'::text, v_expiry;
END $$;

CREATE FUNCTION identity.record_challenge_mail(p_id uuid, p_state text, p_message_id text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
BEGIN
    IF p_id IS NULL OR p_state IS NULL OR p_state NOT IN ('SENT','FAILED','UNKNOWN') THEN
        RAISE EXCEPTION 'invalid mail state' USING ERRCODE = 'PT400';
    END IF;
    -- PD7/§8: the state is set once, from PENDING; a repeat is a no-op (no second audit row).
    WITH upd AS (
        UPDATE identity.email_challenges c SET mail_state = p_state, provider_message_id = left(p_message_id, 128)
         WHERE c.id = p_id AND c.mail_state = 'PENDING' RETURNING c.id, c.principal_id, c.ip_hmac)
    INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action)
    SELECT u.principal_id, u.ip_hmac, u.id, 'mail.' || lower(p_state) FROM upd u;
END $$;

CREATE FUNCTION identity.complete_email_challenge(p_binding bytea, p_purpose text, p_code bytea, p_new_hash text, p_session bytea, p_ttl bigint)
RETURNS TABLE(outcome text, expires_at timestamptz) LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
DECLARE
    v_id uuid; v_email text; v_principal uuid; v_ch identity.email_challenges%ROWTYPE;
    v_active boolean; v_cred identity.password_credentials%ROWTYPE; v_cred_found boolean := false;
    v_same bigint; v_all bigint; v_attempts smallint; v_session uuid; v_expiry timestamptz;
BEGIN
    IF p_binding IS NULL OR octet_length(p_binding) <> 32 OR p_purpose IS NULL OR p_purpose NOT IN ('signup','login','reset')
       OR p_code IS NULL OR octet_length(p_code) <> 32 OR p_session IS NULL OR octet_length(p_session) <> 32
       OR p_ttl IS NULL OR p_ttl NOT BETWEEN 300 AND 86400
       OR (p_purpose = 'reset' AND p_new_hash IS NULL) OR (p_purpose <> 'reset' AND p_new_hash IS NOT NULL) THEN
        RAISE EXCEPTION 'invalid challenge request' USING ERRCODE = 'PT400';
    END IF;
    -- Unlocked lookup only to learn the email/principal for the lock order; everything is rechecked after locking.
    SELECT c.id, c.email, c.principal_id INTO v_id, v_email, v_principal
      FROM identity.email_challenges c WHERE c.binding_hash = p_binding AND c.purpose = p_purpose;
    IF NOT FOUND THEN RETURN QUERY SELECT 'INVALID'::text, NULL::timestamptz; RETURN; END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('lc:pw-email:' || v_email, 0));
    IF v_principal IS NOT NULL THEN
        SELECT p.active INTO v_active FROM identity.principals p WHERE p.id = v_principal FOR UPDATE;
        SELECT * INTO v_cred FROM identity.password_credentials c WHERE c.principal_id = v_principal FOR UPDATE;
        v_cred_found := FOUND;
    END IF;
    SELECT * INTO v_ch FROM identity.email_challenges c WHERE c.id = v_id FOR UPDATE;
    IF NOT FOUND OR v_ch.consumed_at IS NOT NULL OR v_ch.expires_at <= clock_timestamp() THEN
        RETURN QUERY SELECT 'INVALID'::text, NULL::timestamptz; RETURN;
    END IF;
    IF p_purpose = 'reset' THEN
        -- PD5: budget per (principal, source ip bucket) 10 and per principal 50 over 24 h. The principal
        -- row lock above serializes concurrent completes; over budget = INVALID with no compare and no attempts change.
        SELECT COALESCE(sum(c.attempts) FILTER (WHERE c.ip_hmac = v_ch.ip_hmac), 0), COALESCE(sum(c.attempts), 0)
          INTO v_same, v_all
          FROM identity.email_challenges c
         WHERE c.principal_id = v_principal AND c.purpose = 'reset' AND c.created_at > clock_timestamp() - interval '24 hours';
        IF v_same >= 10 OR v_all >= 50 THEN RETURN QUERY SELECT 'INVALID'::text, NULL::timestamptz; RETURN; END IF;
    END IF;
    -- Both sides are HMACs of server-side secrets, so plain <> leaks nothing useful (§4.3 step 3).
    IF v_ch.code_hmac <> p_code THEN
        v_attempts := v_ch.attempts + 1;
        IF v_attempts >= 5 THEN
            UPDATE identity.email_challenges c SET attempts = v_attempts, consumed_at = clock_timestamp(),
                   consumed_reason = 'exhausted', pending_password_hash = NULL WHERE c.id = v_id;
            INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action) VALUES (v_principal, v_ch.ip_hmac, v_id, 'code.exhausted');
        ELSE
            UPDATE identity.email_challenges c SET attempts = v_attempts WHERE c.id = v_id;
        END IF;
        INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action) VALUES (v_principal, v_ch.ip_hmac, v_id, 'code.failed');
        RETURN QUERY SELECT 'INVALID'::text, NULL::timestamptz;
        RETURN;
    END IF;

    IF p_purpose = 'signup' THEN
        UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'verified', pending_password_hash = NULL WHERE c.id = v_id;
        IF EXISTS (SELECT 1 FROM identity.password_credentials c WHERE c.email = v_email) THEN
            RETURN QUERY SELECT 'EXISTS'::text, NULL::timestamptz; RETURN;  -- lost a race with another sign-up
        END IF;
        INSERT INTO identity.principals(id) VALUES (gen_random_uuid()) RETURNING id INTO v_principal;
        INSERT INTO identity.password_credentials(principal_id, email, password_hash, email_verified_at)
        VALUES (v_principal, v_email, v_ch.pending_password_hash, clock_timestamp());
        INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action) VALUES (v_principal, v_ch.ip_hmac, v_id, 'signup.verified');
    ELSIF p_purpose = 'login' THEN
        IF NOT v_cred_found OR NOT v_active OR v_cred.disabled_at IS NOT NULL OR v_cred.password_version <> v_ch.password_version THEN
            RETURN QUERY SELECT 'INVALID'::text, NULL::timestamptz; RETURN;
        END IF;
        UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'verified' WHERE c.id = v_id;
        UPDATE identity.password_credentials c SET failed_count = 0 WHERE c.principal_id = v_principal;
        INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action) VALUES (v_principal, v_ch.ip_hmac, v_id, 'login.verified');
    ELSE
        IF NOT v_cred_found OR NOT v_active THEN
            RETURN QUERY SELECT 'INVALID'::text, NULL::timestamptz; RETURN;
        END IF;
        UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'verified' WHERE c.id = v_id;
        -- PD10: new hash, version+1, counter reset, re-enabled.
        UPDATE identity.password_credentials c SET password_hash = p_new_hash, password_version = c.password_version + 1,
               failed_count = 0, disabled_at = NULL, password_changed_at = clock_timestamp()
         WHERE c.principal_id = v_principal;
        -- Revoke every unrevoked merchant session of the principal (other audiences untouched), with events.
        WITH revoked AS (UPDATE identity.sessions s SET revoked_at = clock_timestamp()
                          WHERE s.principal_id = v_principal AND s.audience = 'merchant' AND s.revoked_at IS NULL RETURNING s.id)
        INSERT INTO identity.session_events(principal_id, session_id, action) SELECT v_principal, r.id, 'session.revoked' FROM revoked r;
        -- Older open login challenges were bound to the old password version; retire them.
        UPDATE identity.email_challenges c SET consumed_at = clock_timestamp(), consumed_reason = 'superseded'
         WHERE c.principal_id = v_principal AND c.purpose = 'login' AND c.consumed_at IS NULL;
        INSERT INTO identity.auth_events(principal_id, ip_hmac, challenge_id, action) VALUES (v_principal, v_ch.ip_hmac, v_id, 'reset.completed');
    END IF;

    -- PD8: a session is issued only here, in the definer that consumed a correct code.
    INSERT INTO identity.sessions(id, token_hash, principal_id, audience, expires_at)
    VALUES (gen_random_uuid(), p_session, v_principal, 'merchant', clock_timestamp() + p_ttl * interval '1 second')
    RETURNING identity.sessions.id, identity.sessions.expires_at INTO v_session, v_expiry;
    INSERT INTO identity.session_events(principal_id, session_id, action) VALUES (v_principal, v_session, 'session.issued');
    RETURN QUERY SELECT 'SESSION'::text, v_expiry;
END $$;

ALTER FUNCTION identity.auth_throttle_hit(bytea,int,int) OWNER TO commerce_identity_writer;
ALTER FUNCTION identity.start_signup_challenge(uuid,text,text,bytea,bytea,text,bytea) OWNER TO commerce_identity_writer;
ALTER FUNCTION identity.password_login_material(text) OWNER TO commerce_identity_writer;
ALTER FUNCTION identity.record_password_failure(text) OWNER TO commerce_identity_writer;
ALTER FUNCTION identity.start_login_challenge(uuid,text,bigint,bytea,bytea,text,bytea) OWNER TO commerce_identity_writer;
ALTER FUNCTION identity.start_reset_challenge(uuid,text,bytea,bytea,text,bytea) OWNER TO commerce_identity_writer;
ALTER FUNCTION identity.record_challenge_mail(uuid,text,text) OWNER TO commerce_identity_writer;
ALTER FUNCTION identity.complete_email_challenge(bytea,text,bytea,text,bytea,bigint) OWNER TO commerce_identity_writer;

REVOKE ALL ON FUNCTION identity.auth_throttle_hit(bytea,int,int),
    identity.start_signup_challenge(uuid,text,text,bytea,bytea,text,bytea),
    identity.password_login_material(text), identity.record_password_failure(text),
    identity.start_login_challenge(uuid,text,bigint,bytea,bytea,text,bytea),
    identity.start_reset_challenge(uuid,text,bytea,bytea,text,bytea),
    identity.record_challenge_mail(uuid,text,text),
    identity.complete_email_challenge(bytea,text,bytea,text,bytea,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.auth_throttle_hit(bytea,int,int),
    identity.start_signup_challenge(uuid,text,text,bytea,bytea,text,bytea),
    identity.password_login_material(text), identity.record_password_failure(text),
    identity.start_login_challenge(uuid,text,bigint,bytea,bytea,text,bytea),
    identity.start_reset_challenge(uuid,text,bytea,bytea,text,bytea),
    identity.record_challenge_mail(uuid,text,text),
    identity.complete_email_challenge(bytea,text,bytea,text,bytea,bigint) TO commerce_identity;

-- §4.4 grants. commerce_identity_writer owns none of the four tables; the 0004 grants (SELECT/INSERT on
-- principals, sessions, session_events, memberships, tenants; UPDATE (id) on principals; UPDATE (revoked_at)
-- on sessions) already cover session issue, principal lock and revoke-all. No TRUNCATE, no UPDATE on auth_events.
GRANT SELECT, INSERT ON identity.password_credentials, identity.email_challenges,
    identity.auth_throttle, identity.auth_events TO commerce_identity_writer;
GRANT UPDATE (password_hash, password_version, failed_count, disabled_at, password_changed_at)
    ON identity.password_credentials TO commerce_identity_writer;
GRANT UPDATE (attempts, consumed_at, consumed_reason, mail_state, provider_message_id, pending_password_hash)
    ON identity.email_challenges TO commerce_identity_writer;
GRANT UPDATE (hits) ON identity.auth_throttle TO commerce_identity_writer;
GRANT DELETE ON identity.auth_throttle, identity.email_challenges, identity.auth_events TO commerce_identity_writer;

-- ---------------------------------------------------------------------------------------------
-- COMMENT ON (PROCESS §5): owning package internal/identity; allowed role commerce_identity via the
-- definers only; non-goals stated per object.
-- ---------------------------------------------------------------------------------------------
COMMENT ON TABLE identity.password_credentials IS 'Owner: internal/identity. One Argon2id credential per password principal (PD1, PD9: never merged with OIDC identities by email). Written only by the complete_email_challenge/record_password_failure definers. Non-goal: no plaintext password, no pepper, no rehash-on-login.';
COMMENT ON COLUMN identity.password_credentials.principal_id IS 'PK and FK to identity.principals; one credential per principal.';
COMMENT ON COLUMN identity.password_credentials.email IS 'Verified login email, ASCII-only lower case (collation-independent lower()); UNIQUE.';
COMMENT ON COLUMN identity.password_credentials.password_hash IS 'PHC string $argon2id$v=19$m=19456,t=2,p=1$salt$hash (PD1). Never returned outside password_login_material.';
COMMENT ON COLUMN identity.password_credentials.password_version IS 'Incremented by reset (PD10); login challenges are bound to the version they were issued for.';
COMMENT ON COLUMN identity.password_credentials.failed_count IS 'Consecutive wrong passwords, saturating at 100 (PD12, R2-1); reset by a verified login or reset.';
COMMENT ON COLUMN identity.password_credentials.disabled_at IS 'Set at 100 consecutive failures (PD12); cleared only by a successful reset or the owner-approved operator unlock runbook.';
COMMENT ON COLUMN identity.password_credentials.email_verified_at IS 'When the sign-up code was verified; a credential exists only for verified emails.';
COMMENT ON COLUMN identity.password_credentials.created_at IS 'Row creation time.';
COMMENT ON COLUMN identity.password_credentials.password_changed_at IS 'Last reset time (or creation).';

COMMENT ON TABLE identity.email_challenges IS 'Owner: internal/identity. One emailed 6-digit challenge (PD4): stores only HMAC(auth_pepper, binding_hash||code), never the plaintext code. Written only by the start_*/record_challenge_mail/complete_email_challenge definers. Non-goal: not a queue, not a mail outbox (PD7: one send after commit, never retried).';
COMMENT ON COLUMN identity.email_challenges.id IS 'Go-generated challenge id; used as the mail-state handle.';
COMMENT ON COLUMN identity.email_challenges.purpose IS 'signup | login | reset; the purpose shape CHECK ties it to principal/version/pending hash.';
COMMENT ON COLUMN identity.email_challenges.email IS 'Recipient, normalized (ASCII lower case).';
COMMENT ON COLUMN identity.email_challenges.locale IS 'Mail language chosen by the step-1 caller (zh-CN/zh-TW/en).';
COMMENT ON COLUMN identity.email_challenges.principal_id IS 'NULL for signup (no principal until verified); set for login/reset.';
COMMENT ON COLUMN identity.email_challenges.password_version IS 'Login only: credential version the challenge is bound to.';
COMMENT ON COLUMN identity.email_challenges.pending_password_hash IS 'Signup only: Argon2id hash awaiting verification; NULLed on consume, supersede and exhaustion.';
COMMENT ON COLUMN identity.email_challenges.binding_hash IS 'sha256 of the browser binding held in the HttpOnly challenge cookie; UNIQUE lookup key.';
COMMENT ON COLUMN identity.email_challenges.code_hmac IS 'HMAC-SHA256(auth_pepper, binding_hash||code); a 6-digit code is never stored plain or as plain SHA-256.';
COMMENT ON COLUMN identity.email_challenges.ip_hmac IS 'HMAC of the creating source ip bucket (PD14); drives same-source supersede (PD4) and the reset budget (PD5).';
COMMENT ON COLUMN identity.email_challenges.attempts IS 'Wrong codes entered, 0..5; 5 exhausts the challenge.';
COMMENT ON COLUMN identity.email_challenges.created_at IS 'Creation time; 24 h window of the PD5 reset budget.';
COMMENT ON COLUMN identity.email_challenges.expires_at IS 'created_at + 10 minutes, set in SQL (NIST OTP lifetime).';
COMMENT ON COLUMN identity.email_challenges.consumed_at IS 'Set once when verified, superseded or exhausted; single use.';
COMMENT ON COLUMN identity.email_challenges.consumed_reason IS 'verified | superseded | exhausted.';
COMMENT ON COLUMN identity.email_challenges.mail_state IS 'PENDING -> SENT | FAILED | UNKNOWN, set once by record_challenge_mail; UNKNOWN is never retried (I06).';
COMMENT ON COLUMN identity.email_challenges.provider_message_id IS 'SMTP 2xx reply text, at most 128 bytes.';

COMMENT ON TABLE identity.auth_throttle IS 'Owner: internal/identity. Fixed-window counters for pre-authentication rate limits (§6); bucket = HMAC(auth_pepper, kind:value), so no raw ip/email at rest. Purged 2 days after the window (Q8). Non-goal: not a general rate limiter, not per tenant (tenant is unknown before authentication, PD13).';
COMMENT ON COLUMN identity.auth_throttle.bucket IS '32-byte sha256(bucket HMAC || window length || offset): one key per window of a bucket (F1), so windows that start at the same instant never share a row.';
COMMENT ON COLUMN identity.auth_throttle.window_start IS 'Aligned window start (UTC, or UTC+8 day for daily buckets).';
COMMENT ON COLUMN identity.auth_throttle.hits IS 'Hits in the window, incremented by auth_throttle_hit.';

COMMENT ON TABLE identity.auth_events IS 'Owner: internal/identity. Append-only audit of password-auth events (no tenant exists yet, so ops.audit_events cannot hold it). No raw email, ip, code or password. Retention 180 days (Q8), purged by auth_throttle_hit. operator.unlocked is written only by the migration-owner runbook SQL.';
COMMENT ON COLUMN identity.auth_events.id IS 'Random row id.';
COMMENT ON COLUMN identity.auth_events.principal_id IS 'The principal concerned when known.';
COMMENT ON COLUMN identity.auth_events.email_hmac IS 'Reserved for an email HMAC; the frozen definer signatures carry no pepper, so definers leave it NULL.';
COMMENT ON COLUMN identity.auth_events.ip_hmac IS 'HMAC of the source ip bucket when the definer receives it.';
COMMENT ON COLUMN identity.auth_events.challenge_id IS 'The email_challenges.id the event concerns (no FK: challenges are purged earlier).';
COMMENT ON COLUMN identity.auth_events.action IS 'Closed event vocabulary (CHECK).';
COMMENT ON COLUMN identity.auth_events.created_at IS 'Event time.';

COMMENT ON FUNCTION identity.auth_throttle_hit(bytea,int,int) IS 'Owner: internal/identity (throttle.go). Role: commerce_identity only. Upserts +1 in the fixed window (p_offset_seconds = zone offset east of UTC, 28800 for the UTC+8 day) and returns hits; also purges <=100 old rows per table (I23). Non-goal: does not know or enforce the limit.';
COMMENT ON FUNCTION identity.start_signup_challenge(uuid,text,text,bytea,bytea,text,bytea) IS 'Owner: internal/identity (challenge.go). Role: commerce_identity only. EXISTS when a credential has the email (no row), else supersedes same-source open sign-up challenges (PD4) and inserts one. Non-goal: never creates a principal.';
COMMENT ON FUNCTION identity.password_login_material(text) IS 'Owner: internal/identity. Role: commerce_identity only. Returns the stored PHC, version and disabled flag for an active principal; zero rows otherwise. Never returns a principal id.';
COMMENT ON FUNCTION identity.record_password_failure(text) IS 'Owner: internal/identity. Role: commerce_identity only. failed_count = LEAST(+1,100), disables at 100; a no-op for unknown emails; never raises (PD6, R2-1).';
COMMENT ON FUNCTION identity.start_login_challenge(uuid,text,bigint,bytea,bytea,text,bytea) IS 'Owner: internal/identity. Role: commerce_identity only. After Go verified the password: locks credential, requires the version, supersedes older login challenges, inserts one, returns has_membership for the mail budget split (PD13). PT401 when the credential changed or is unusable.';
COMMENT ON FUNCTION identity.start_reset_challenge(uuid,text,bytea,bytea,text,bytea) IS 'Owner: internal/identity. Role: commerce_identity only. NONE for unknown/inactive emails; else same-source supersede (PD4) and insert.';
COMMENT ON FUNCTION identity.record_challenge_mail(uuid,text,text) IS 'Owner: internal/identity (sender.go). Role: commerce_identity only. Sets mail_state once from PENDING to SENT/FAILED/UNKNOWN and audits it. Non-goal: never triggers a resend.';
COMMENT ON FUNCTION identity.complete_email_challenge(bytea,text,bytea,text,bytea,bigint) IS 'Owner: internal/identity (challenge.go). Role: commerce_identity only. The only place a password-principal session is issued (PD8): checks binding, expiry, reset budget (PD5) and code, then signup/login/reset per §4.3. Returns SESSION, INVALID or EXISTS; INVALID never distinguishes wrong, exhausted, over-budget or unknown.';
