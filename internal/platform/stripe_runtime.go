// stripe_runtime.go owns Stripe database-role admission, reusing the shared gate.
// It never reads payment credentials, verifies webhooks or moves payment state.

package platform

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/psp/stripe"
)

// LoadStripeLiveApproval reads COMMERCE_STRIPE_LIVE_ENABLED and COMMERCE_STRIPE_LIVE_APPROVAL_REF, the
// owner's LIVE flag+reference pair (contracts/stripe-live-enable-v1.md §5.1). "" and "0" mean the flag is
// off; both-or-neither: a flag without a reference or a reference without the flag is an error, as is a
// malformed reference. Neither set returns the zero LiveApproval (no LIVE). The returned error is fixed
// text and never carries the reference. Callers (cmd/api, cmd/payment-worker, cmd/stripe-admin) still
// decide whether the profile they run may use it; stripe.admit re-validates the pair on every client.
func LoadStripeLiveApproval(getenv func(string) string) (stripe.LiveApproval, error) {
	if getenv == nil {
		return stripe.LiveApproval{}, errors.New("stripe live approval invalid")
	}
	var live stripe.LiveApproval
	switch getenv("COMMERCE_STRIPE_LIVE_ENABLED") {
	case "", "0":
	case "1":
		live.Enabled = true
	default:
		return stripe.LiveApproval{}, errors.New("stripe live approval invalid")
	}
	live.Reference = getenv("COMMERCE_STRIPE_LIVE_APPROVAL_REF")
	if !live.Enabled && live.Reference == "" {
		return stripe.LiveApproval{}, nil
	}
	if !live.Valid() { // flag without ref, ref without flag, or a malformed ref
		return stripe.LiveApproval{}, errors.New("stripe live approval invalid")
	}
	return live, nil
}

// OpenStripeIngressPool admits the insert-only webhook authority. Neither the
// merchant nor payment-worker pool may be substituted when this opener fails.
func OpenStripeIngressPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if ctx == nil || len(dsn) > 8192 {
		return nil, errors.New("stripe ingress database unavailable")
	}
	pool, err := openPool(ctx, dsn, "stripe_ingress")
	if err != nil {
		return nil, errors.New("stripe ingress database unavailable")
	}
	return pool, nil
}

// ValidateStripeIngressPool borrows the caller's pool; failure never closes it.
func ValidateStripeIngressPool(ctx context.Context, pool *pgxpool.Pool) error {
	if ctx == nil || pool == nil {
		return errors.New("stripe ingress database unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := validatePoolAuthority(bounded, pool, "stripe_ingress"); err != nil {
		return errors.New("stripe ingress database unavailable")
	}
	return nil
}

// OpenStripeRegistrarPool admits only the operator's scoped registry definers.
// Connection/parser errors are masked because they can contain DSN credentials.
func OpenStripeRegistrarPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if ctx == nil || len(dsn) > 8192 {
		return nil, errors.New("stripe registrar database unavailable")
	}
	pool, err := openPool(ctx, dsn, "stripe_registrar")
	if err != nil {
		return nil, errors.New("stripe registrar database unavailable")
	}
	return pool, nil
}

// These are the frozen 0061/post-River0012 ABIs, not broad schema/name grants.
// The shared validator checks them for every pool so a direct or PUBLIC grant
// cannot smuggle registrar/ingress authority into an otherwise ordinary login.
var stripeIngressFunctions = []string{
	"payments.stripe_webhook_material(uuid)",
	// stripe-refund-v1 D2: 0062 replaces the 16-argument prepare with 16 + payment_intent + lc_refund.
	"payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint,text,text)",
	"payments.stripe_webhook_commit(uuid,uuid,bigint)",
	// customers-billing-v1 C-4 (0079): the platform webhook shares this login and executes exactly this ABI.
	// billing.platform_account_conflict(text) is granted to BOTH this login and commerce_runtime (startup
	// check), so it is not in this list (a runtime pool would fail the fixed-function scan); see
	// stripeIngressExtraFunctions below.
	"billing.apply_subscription(text,text,text,text,text,timestamp with time zone,timestamp with time zone,boolean,timestamp with time zone,timestamp with time zone,uuid,boolean)",
}

// stripeIngressExtraFunctions are functions the ingress login may execute in addition to
// stripeIngressFunctions but that other pools legitimately hold too, so they cannot join the fixed list
// (which every non-ingress pool must NOT hold). Resolved by regprocedure, ingress only.
var stripeIngressExtraFunctions = []string{"billing.platform_account_conflict(text)"}
var stripeRegistrarFunctions = []string{
	"integration.register_stripe_account(uuid,uuid,uuid,uuid,uuid,text,text,text,bytea,bytea)",
	"integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea)",
	"payments.set_stripe_webhook_endpoint(uuid,uuid,uuid,uuid,uuid,text,bigint,boolean,text,bytea,bytea)",
	// Read-only: the registered account of an in-scope connection, sealed into the webhook AAD (§0.2).
	"payments.stripe_endpoint_account(uuid,uuid,uuid,uuid)",
	// Read-only: the sealed API credential envelope at the head version, for the SANDBOX qualify probe (S4).
	"payments.stripe_registrar_credential(uuid,uuid,uuid,uuid,bigint)",
	"payments.qualify_stripe_method(uuid,uuid,uuid,uuid,uuid,bigint,text,text,timestamp with time zone,timestamp with time zone)",
	"payments.set_stripe_method(uuid,uuid,uuid,uuid,text,uuid,uuid,bigint,boolean,boolean,integer,bigint,bigint,text,text,text)",
	// stripe-live-enable-v1 §3.4 (migration 0077): LIVE approval, canary verification and the per-store kill switch.
	"payments.approve_stripe_live(uuid,uuid,uuid,uuid,uuid,text,text,timestamp with time zone,bigint,bigint,text[],jsonb)",
	"payments.record_stripe_live_canary(uuid,uuid,uuid,uuid,uuid,uuid)",
	"payments.revoke_stripe_live(uuid,uuid,uuid,uuid,text)",
}

func validateStripeAuthority(ctx context.Context, pool *pgxpool.Pool, authority string) error {
	if authority == "runtime" {
		if err := validateRuntimeRiverPrivileges(ctx, pool); err != nil {
			return err
		}
	}
	var allowed []string
	all := append(append([]string{}, stripeIngressFunctions...), stripeRegistrarFunctions...)
	if authority == "stripe_ingress" {
		allowed = stripeIngressFunctions
	} else if authority == "stripe_registrar" {
		allowed = stripeRegistrarFunctions
	}
	if allowed == nil {
		allowed = []string{}
	}
	var forbidden, missing bool
	var allowedOIDs []uint32
	// pg_proc OIDs avoid schema USAGE checks on catalog lookup. has_*_privilege
	// includes PUBLIC; reachable covers inherited and SET-only custom roles.
	err := pool.QueryRow(ctx, `WITH reachable AS (
	 SELECT oid FROM pg_roles WHERE rolname=session_user
	  OR pg_has_role(session_user,oid,'USAGE') OR pg_has_role(session_user,oid,'SET')
	), fixed AS (
	 -- ABI match by namespace name, proname and pg_catalog type OIDs (oidvector
	 -- has no cast from oid[] and its array bounds start at 0, so compare text). Never by
	 -- format_type/oidvectortypes text: those qualify or hide names by the
	 -- caller's search_path, so a shadowing type could hide a fixed function
	 -- from the forbidden-EXECUTE scan below (fail-open).
	 SELECT p.oid,w.signature FROM unnest($1::text[]) w(signature)
	 LEFT JOIN pg_catalog.pg_namespace n ON n.nspname=split_part(w.signature,'.',1)
	 LEFT JOIN pg_catalog.pg_proc p ON p.pronamespace=n.oid
	  AND p.proname=split_part(split_part(w.signature,'(',1),'.',2)
	  AND p.proargtypes::pg_catalog.text=pg_catalog.array_to_string(ARRAY(
	   SELECT t.oid FROM unnest(string_to_array(substring(w.signature from '\((.*)\)$'),',')) WITH ORDINALITY a(name,ord)
	   JOIN pg_catalog.pg_type t ON t.typnamespace='pg_catalog'::regnamespace
	    AND t.typname=CASE a.name WHEN 'bigint' THEN 'int8' WHEN 'integer' THEN 'int4'
	     WHEN 'boolean' THEN 'bool' WHEN 'timestamp with time zone' THEN 'timestamptz'
	     WHEN 'text[]' THEN '_text' ELSE a.name END
	   ORDER BY a.ord),' ')
	)
	SELECT EXISTS(SELECT 1 FROM reachable r CROSS JOIN fixed f
	 WHERE f.oid IS NOT NULL AND NOT(f.signature=ANY($2::text[]))
	  AND has_function_privilege(r.oid,f.oid,'EXECUTE')),
	 EXISTS(SELECT 1 FROM fixed f WHERE f.signature=ANY($2::text[])
	  AND (f.oid IS NULL OR NOT coalesce(has_function_privilege(session_user,f.oid,'EXECUTE'),false))),
	 (SELECT coalesce(array_agg(f.oid),'{}'::oid[]) FROM fixed f
	  WHERE f.signature=ANY($2::text[]) AND f.oid IS NOT NULL)`,
		all, allowed).Scan(&forbidden, &missing, &allowedOIDs)
	if err != nil || forbidden || missing {
		return errors.New("unsafe stripe database privileges")
	}
	if len(allowed) == 0 {
		return nil
	}
	if authority == "stripe_ingress" {
		for _, signature := range stripeIngressExtraFunctions {
			var oid uint32
			if err := pool.QueryRow(ctx, `SELECT coalesce(to_regprocedure($1)::oid::bigint,0)::oid`, signature).Scan(&oid); err != nil {
				return errors.New("unsafe stripe database privileges")
			}
			if oid != 0 {
				allowedOIDs = append(allowedOIDs, oid)
			}
		}
	}
	// New Stripe roles cannot read any application table. The only exception is
	// the ingress role's river_payment.river_job INSERT/SELECT plus column-level
	// UPDATE(kind), which River's JobInsertFastMany needs for its
	// ON CONFLICT (unique_key) DO UPDATE SET kind=EXCLUDED.kind privilege check
	// (a kind no-op: integration.guard_payment_job_family rejects any kind/args/
	// identity rewrite), plus river_job_id_seq. Never any other lifecycle mutation.
	err = pool.QueryRow(ctx, `WITH reachable AS (
	 SELECT oid FROM pg_roles WHERE rolname=session_user
	  OR pg_has_role(session_user,oid,'USAGE') OR pg_has_role(session_user,oid,'SET')
	), allowed AS (
	 SELECT unnest($2::oid[]) AS oid
	)
	SELECT EXISTS(SELECT 1 FROM reachable r CROSS JOIN pg_class c
	 JOIN pg_namespace n ON n.oid=c.relnamespace
	 WHERE n.nspname<>'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'
	 AND CASE
	  WHEN c.relkind='S' AND $1='stripe_ingress' AND n.nspname='river_payment' AND c.relname='river_job_id_seq'
	   THEN has_sequence_privilege(r.oid,c.oid,'SELECT,UPDATE')
	  WHEN c.relkind='S' THEN has_sequence_privilege(r.oid,c.oid,'USAGE,SELECT,UPDATE')
	  WHEN c.relkind IN ('r','p','v','m','f') AND $1='stripe_ingress' AND n.nspname='river_payment' AND c.relname='river_job'
	   THEN has_table_privilege(r.oid,c.oid,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
	    OR has_any_column_privilege(r.oid,c.oid,'REFERENCES')
	    OR EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
	     AND a.attname<>'kind' AND has_column_privilege(r.oid,c.oid,a.attnum,'UPDATE'))
	  WHEN c.relkind IN ('r','p','v','m','f') THEN
	   has_table_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
	    OR has_any_column_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,REFERENCES')
	  ELSE false END)
	 OR EXISTS(SELECT 1 FROM reachable r CROSS JOIN pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
	  WHERE n.nspname<>'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'
	   AND NOT EXISTS(SELECT 1 FROM allowed a WHERE a.oid=p.oid)
	   -- River v0.40 creates river_job_state_in_bitmask(bit,river_job_state) in each
	   -- River schema with PUBLIC EXECUTE, and every River insert statement
	   -- (JobInsertFastMany's ON CONFLICT predicate) calls it, so it cannot be
	   -- revoked from PUBLIC without breaking every River producer, nor withheld
	   -- from stripe_ingress. Exempt exactly that inert function: fixed schema
	   -- names, IMMUTABLE, not SECURITY DEFINER, signature (bit, same-schema
	   -- river_job_state), owned by the schema's river_job owner. Nothing else.
	   AND NOT (p.proname='river_job_state_in_bitmask'
	    AND n.nspname IN ('river','river_meta','river_payment','river_expiry','river_media')
	    AND p.provolatile='i' AND NOT p.prosecdef AND p.pronargs=2
	    AND p.proargtypes[0]='pg_catalog.bit'::regtype
	    AND p.proargtypes[1]=(SELECT t.oid FROM pg_catalog.pg_type t
	     WHERE t.typnamespace=n.oid AND t.typname='river_job_state')
	    AND EXISTS(SELECT 1 FROM pg_catalog.pg_class j WHERE j.relnamespace=n.oid
	     AND j.relname='river_job' AND j.relowner=p.proowner))
	   AND has_function_privilege(r.oid,p.oid,'EXECUTE'))
	 OR EXISTS(SELECT 1 FROM reachable r CROSS JOIN pg_namespace n
	  WHERE n.nspname<>'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'
	   AND has_schema_privilege(r.oid,n.oid,'CREATE'))
	 OR EXISTS(SELECT 1 FROM reachable r WHERE has_database_privilege(r.oid,current_database(),'CREATE'))`,
		authority, allowedOIDs).Scan(&forbidden)
	if err != nil || forbidden {
		return errors.New("unsafe stripe database privileges")
	}
	return nil
}

// validateRuntimeRiverPrivileges bounds what the merchant API login (commerce_runtime) may do to any River
// schema (stripe-refund-v1 §4.5, ruling on the river_payment grants). The runtime pool inserts jobs through
// River's InsertTx, which needs exactly SELECT, INSERT and column UPDATE(kind) on river_job plus USAGE on
// river_job_id_seq, in the three schemas whose families the merchant API may produce (river, river_media and
// river_payment). It must never hold DELETE, TRUNCATE, TRIGGER, any other column UPDATE, any privilege on
// another River table, or any privilege at all in river_meta and river_expiry: guard_payment_job_family and
// the deferred queue router only protect job identity, not job lifecycle.
func validateRuntimeRiverPrivileges(ctx context.Context, pool *pgxpool.Pool) error {
	var forbidden bool
	err := pool.QueryRow(ctx, `WITH reachable AS (
	 SELECT oid FROM pg_roles WHERE rolname=session_user
	  OR pg_has_role(session_user,oid,'USAGE') OR pg_has_role(session_user,oid,'SET')
	)
	SELECT EXISTS(SELECT 1 FROM reachable r CROSS JOIN pg_class c
	 JOIN pg_namespace n ON n.oid=c.relnamespace
	 WHERE n.nspname IN ('river','river_meta','river_payment','river_expiry','river_media')
	 AND CASE
	  WHEN c.relkind='S' AND n.nspname IN ('river','river_media','river_payment') AND c.relname='river_job_id_seq'
	   THEN has_sequence_privilege(r.oid,c.oid,'SELECT,UPDATE')
	  WHEN c.relkind='S' THEN has_sequence_privilege(r.oid,c.oid,'USAGE,SELECT,UPDATE')
	  WHEN c.relkind IN ('r','p','v','m','f') AND n.nspname IN ('river','river_media','river_payment') AND c.relname='river_job'
	   THEN has_table_privilege(r.oid,c.oid,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
	    OR has_any_column_privilege(r.oid,c.oid,'REFERENCES')
	    OR EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
	     AND a.attname<>'kind' AND has_column_privilege(r.oid,c.oid,a.attnum,'UPDATE'))
	  WHEN c.relkind IN ('r','p','v','m','f') THEN
	   has_table_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
	    OR has_any_column_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,REFERENCES')
	  ELSE false END)`).Scan(&forbidden)
	if err != nil || forbidden {
		return errors.New("unsafe runtime river privileges")
	}
	return nil
}
