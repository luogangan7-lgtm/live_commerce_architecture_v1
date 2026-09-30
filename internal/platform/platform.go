// Package platform owns the narrow HTTP and database foundation shared by the API process: pool
// opening per DB role, WithScope (token to tenant/store scope inside one transaction) and permission
// checks. Domain packages receive a scoped transaction, never a pool. It never holds a domain rule,
// never widens a role's grants, and never accepts a tenant or store id that did not come from a
// verified session.
package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"livecommerce/internal/httperror"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnauthorized = errors.New("unauthorized")
var ErrForbidden = errors.New("forbidden")
var ErrScopeNotFound = errors.New("scope not found")

const (
	storeReadPermission = "store:read"
	auditReadPermission = "audit:read"
	requestTimeout      = 5 * time.Second
	startupTimeout      = 2 * time.Second
	lockTimeout         = time.Second
)

// Scope is resolved by identity.resolve_access and is not derived from HTTP input.
type Scope struct {
	TenantID    string
	StoreID     string
	PrincipalID string
	Revision    int64
}

// OpenPool opens the runtime pool and rejects privileged or schema-owning logins.
// A login granted commerce_runtime is valid when it is itself not privileged.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openPool(ctx, dsn, "runtime")
}

// OpenIdentityPool is only for the trusted login/onboarding service. Never pass
// this pool into business handlers: identity issuance is a different authority.
func OpenIdentityPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openPool(ctx, dsn, "identity")
}

// OpenBuyerPool cannot read merchant tables, even when a buyer has the same
// tenant/store context. Buyer resource grants are separate from merchant RBAC.
func OpenBuyerPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openPool(ctx, dsn, "buyer_runtime")
}

// OpenCheckoutPool admits only the dedicated internal checkout login. It is
// never a public buyer SQL credential or a merchant authority.
func OpenCheckoutPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openPool(ctx, dsn, "checkout_runtime")
}

// OpenHostedPool is the trusted payment signing authority. Its LOGIN inherits
// commerce_hosted_runtime (GRANT ... WITH INHERIT TRUE, SET FALSE), which in
// turn inherits checkout runtime grants. Admission checks both options.
func OpenHostedPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openPool(ctx, dsn, "hosted_runtime")
}

// OpenBuyerIssuerPool is an internal capability authority, never a public
// store-ID-to-token endpoint. See contracts/buyer-capability-v1.md.
func OpenBuyerIssuerPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openPool(ctx, dsn, "buyer_issuer")
}

// OpenWorkerPool is a separate non-HTTP authority. River's lifecycle grants
// must never be inherited by a merchant, buyer, or identity service login.
func OpenWorkerPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return openPool(ctx, dsn, "worker")
}

// ValidateMetaIngressPool admits only a dedicated webhook producer. The borrowed
// pool remains owned by the caller; no merchant or worker authority may coexist.
func ValidateMetaIngressPool(ctx context.Context, pool *pgxpool.Pool) error {
	if ctx == nil || pool == nil {
		return errors.New("meta ingress database unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := validatePoolAuthority(bounded, pool, "meta_ingress"); err != nil {
		return errors.New("meta ingress database unavailable")
	}
	return nil
}

// ValidateMetaConsumerPool borrows a dedicated projection-only pool. River's
// lifecycle pool is separate, so a consumer cannot grant itself a running job.
func ValidateMetaConsumerPool(ctx context.Context, pool *pgxpool.Pool) error {
	if ctx == nil || pool == nil {
		return errors.New("meta consumer database unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := validatePoolAuthority(bounded, pool, "meta_consumer"); err != nil {
		return errors.New("meta consumer database unavailable")
	}
	return nil
}

func openPool(ctx context.Context, dsn string, authority string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("database url required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	config.MaxConns = 8

	startup, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(startup, config)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(startup); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unavailable: %w", err)
	}
	if err := validatePoolAuthority(startup, pool, authority); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// ValidateWorkerPool reuses the startup authority gate when an internal worker
// receives an existing pool. The caller retains ownership of that pool; failure
// never closes it. A nil pool and an owner/mixed-role connection fail closed.
func ValidateWorkerPool(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("worker database pool required")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	return validatePoolAuthority(bounded, pool, "worker")
}

// ValidateCheckoutPool checks an existing pool without taking ownership of it.
func ValidateCheckoutPool(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("checkout database pool required")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	return validatePoolAuthority(bounded, pool, "checkout_runtime")
}

func ValidateHostedPool(ctx context.Context, pool *pgxpool.Pool) error {
	if ctx == nil || pool == nil {
		return errors.New("hosted context and database pool required")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	return validatePoolAuthority(bounded, pool, "hosted_runtime")
}

// ValidateBuyerPool checks a borrowed buyer pool before it enters buyer.WithScope.
// It must not inherit issuer, merchant, owner, or checkout authority.
func ValidateBuyerPool(ctx context.Context, pool *pgxpool.Pool) error {
	if ctx == nil || pool == nil {
		return errors.New("buyer context and runtime pool required")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	return validatePoolAuthority(bounded, pool, "buyer_runtime")
}

// ValidateBuyerIssuerPool checks an existing issuer pool without taking ownership.
func ValidateBuyerIssuerPool(ctx context.Context, pool *pgxpool.Pool) error {
	if ctx == nil {
		return errors.New("buyer issuer context required")
	}
	if pool == nil {
		return errors.New("buyer issuer database pool required")
	}
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	return validatePoolAuthority(bounded, pool, "buyer_issuer")
}

func validatePoolAuthority(ctx context.Context, pool *pgxpool.Pool, authority string) error {
	var sameLogin, dsnUserMatch, superuser, bypassRLS, roleAdmin, databaseCreator, replication, objectOwner, runtimeMember, authMember, identityMember, buyerRuntimeMember, buyerIssuerMember, workerMember, checkoutMember, hostedMember, hostedUsage, hostedSet, checkoutWriterMember, canReachPrivileged bool
	var metaIngress, metaRegistrar, metaCurator, metaConsumer, metaWriter, metaUsage, metaSet, consumerUsage, consumerSet, systemAuthority bool
	var metaWorker, metaWorkerUsage, metaWorkerSet bool
	var mediaRegistrar, mediaRegistrarUsage, mediaRegistrarSet, mediaWriter, mediaWriterUsage, mediaWriterSet bool
	var mediaWorker, mediaWorkerUsage, mediaWorkerSet, mediaExecutor, mediaExecutorUsage, mediaExecutorSet bool
	var mediaRecovery, mediaRecoveryUsage, mediaRecoverySet bool
	var stripeIngress, stripeIngressUsage, stripeIngressSet, stripeRegistrar, stripeRegistrarUsage, stripeRegistrarSet, stripeRegistryWriter, integrationWriter bool
	err := pool.QueryRow(ctx, `
		SELECT coalesce(pg_has_role(session_user, to_regrole('commerce_stripe_ingress'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_stripe_ingress'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_stripe_ingress'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_payment_registrar'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_payment_registrar'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_payment_registrar'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_payment_registry_writer'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_integration_writer'), 'MEMBER'),false),
		       session_user=current_user, session_user=$1, r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolcreatedb, r.rolreplication,
		       (EXISTS (
			   SELECT 1 FROM pg_namespace n
			   WHERE n.nspowner = r.oid
			     AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		       ) OR EXISTS (
			   SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			   WHERE c.relowner=r.oid AND n.nspname NOT IN ('pg_catalog','information_schema')
		       ) OR EXISTS (
			   SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			   WHERE p.proowner=r.oid AND n.nspname NOT IN ('pg_catalog','information_schema')
		       )),
		       pg_has_role(session_user, 'commerce_runtime', 'MEMBER'),
		       pg_has_role(session_user, 'commerce_auth', 'MEMBER'),
		       pg_has_role(session_user, 'commerce_identity', 'MEMBER'),
		       pg_has_role(session_user, 'commerce_buyer_runtime', 'MEMBER'),
		       pg_has_role(session_user, 'commerce_buyer_issuer', 'MEMBER'),
		       pg_has_role(session_user, 'commerce_worker', 'MEMBER'),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_checkout_runtime'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_hosted_runtime'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_hosted_runtime'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_hosted_runtime'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_checkout_writer'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_ingress'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_registrar'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_curator'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_consumer'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_writer'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_ingress'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_ingress'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_consumer'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_consumer'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_worker'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_worker'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_meta_worker'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_registrar'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_registrar'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_registrar'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_writer'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_writer'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_writer'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_worker'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_worker'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_worker'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_executor'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_executor'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_executor'), 'SET'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_recovery'), 'MEMBER'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_recovery'), 'USAGE'),false),
		       coalesce(pg_has_role(session_user, to_regrole('commerce_media_recovery'), 'SET'),false),
		       EXISTS (SELECT 1 FROM pg_roles predefined WHERE predefined.rolname LIKE 'pg\_%' ESCAPE '\'
			   AND pg_has_role(session_user, predefined.oid, 'MEMBER')),
		       EXISTS (
			   SELECT 1 FROM pg_roles candidate
			   WHERE (candidate.rolsuper OR candidate.rolbypassrls OR candidate.rolcreaterole OR candidate.rolcreatedb OR candidate.rolreplication
			       OR EXISTS (
				   SELECT 1 FROM pg_namespace n
				   WHERE n.nspowner = candidate.oid
				     AND n.nspname NOT IN ('pg_catalog', 'information_schema')
			       ) OR EXISTS (
				   SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
				   WHERE c.relowner=candidate.oid AND n.nspname NOT IN ('pg_catalog','information_schema')
			       ) OR EXISTS (
				   SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
				   WHERE p.proowner=candidate.oid AND n.nspname NOT IN ('pg_catalog','information_schema')
			       ))
			     AND (pg_has_role(session_user, candidate.oid, 'SET') OR pg_has_role(session_user, candidate.oid, 'USAGE'))
		       ) OR EXISTS (
			   -- ADMIN OPTION lets the login GRANT any authority it is a member of
			   -- to itself or others; no runtime pool, old or new, may hold it.
			   SELECT 1 FROM pg_auth_members am WHERE am.member=r.oid AND am.admin_option
		       )
		FROM pg_roles r WHERE r.rolname = session_user`, pool.Config().ConnConfig.User).
		Scan(&stripeIngress, &stripeIngressUsage, &stripeIngressSet, &stripeRegistrar, &stripeRegistrarUsage, &stripeRegistrarSet, &stripeRegistryWriter, &integrationWriter, &sameLogin, &dsnUserMatch, &superuser, &bypassRLS, &roleAdmin, &databaseCreator, &replication, &objectOwner, &runtimeMember, &authMember, &identityMember, &buyerRuntimeMember, &buyerIssuerMember, &workerMember, &checkoutMember, &hostedMember, &hostedUsage, &hostedSet, &checkoutWriterMember, &metaIngress, &metaRegistrar, &metaCurator, &metaConsumer, &metaWriter, &metaUsage, &metaSet, &consumerUsage, &consumerSet, &metaWorker, &metaWorkerUsage, &metaWorkerSet, &mediaRegistrar, &mediaRegistrarUsage, &mediaRegistrarSet, &mediaWriter, &mediaWriterUsage, &mediaWriterSet, &mediaWorker, &mediaWorkerUsage, &mediaWorkerSet, &mediaExecutor, &mediaExecutorUsage, &mediaExecutorSet, &mediaRecovery, &mediaRecoveryUsage, &mediaRecoverySet, &systemAuthority, &canReachPrivileged)
	if err != nil {
		return fmt.Errorf("validate runtime role: %w", err)
	}
	// The claims intake authority (meta-claims-intake-v1 §4.1) has its own probe so that every
	// other authority also rejects a login that can reach commerce_claims_intake, and the intake
	// login rejects every other authority (both directions, via the exactly-one rule below).
	var claimsIntake, claimsIntakeUsage, claimsIntakeSet bool
	if err := pool.QueryRow(ctx, `SELECT coalesce(pg_has_role(session_user, to_regrole('commerce_claims_intake'), 'MEMBER'),false),
		coalesce(pg_has_role(session_user, to_regrole('commerce_claims_intake'), 'USAGE'),false),
		coalesce(pg_has_role(session_user, to_regrole('commerce_claims_intake'), 'SET'),false)`).
		Scan(&claimsIntake, &claimsIntakeUsage, &claimsIntakeSet); err != nil {
		return fmt.Errorf("validate runtime role: %w", err)
	}
	// U08 retention logins (claims-retention-purge-v1 §4): same shape as the claims intake probe. The two roles
	// join the exactly-one rule below, so every other authority rejects a login that can reach either of them,
	// and both reject any other authority. Their definer owner commerce_retention_writer owns functions and is
	// therefore already caught by the canReachPrivileged owner scan.
	var retentionJob, retentionJobUsage, retentionJobSet, retentionOperator, retentionOperatorUsage, retentionOperatorSet bool
	if err := pool.QueryRow(ctx, `SELECT coalesce(pg_has_role(session_user, to_regrole('commerce_retention_job'), 'MEMBER'),false),
		coalesce(pg_has_role(session_user, to_regrole('commerce_retention_job'), 'USAGE'),false),
		coalesce(pg_has_role(session_user, to_regrole('commerce_retention_job'), 'SET'),false),
		coalesce(pg_has_role(session_user, to_regrole('commerce_retention_operator'), 'MEMBER'),false),
		coalesce(pg_has_role(session_user, to_regrole('commerce_retention_operator'), 'USAGE'),false),
		coalesce(pg_has_role(session_user, to_regrole('commerce_retention_operator'), 'SET'),false)`).
		Scan(&retentionJob, &retentionJobUsage, &retentionJobSet, &retentionOperator, &retentionOperatorUsage, &retentionOperatorSet); err != nil {
		return fmt.Errorf("validate runtime role: %w", err)
	}
	// Exactly one authority, including indirect grants. Checking only the desired
	// role would let a mixed login smuggle merchant privileges into buyer code.
	memberships := map[string]bool{"runtime": runtimeMember, "identity": identityMember,
		"buyer_runtime": buyerRuntimeMember, "buyer_issuer": buyerIssuerMember, "worker": workerMember,
		"checkout_runtime": checkoutMember, "meta_ingress": metaIngress,
		"meta_registrar": metaRegistrar, "meta_curator": metaCurator, "meta_consumer": metaConsumer,
		"meta_worker": metaWorker, "media_registrar": mediaRegistrar,
		"media_worker": mediaWorker, "media_executor": mediaExecutor, "media_recovery": mediaRecovery,
		"stripe_ingress": stripeIngress, "stripe_registrar": stripeRegistrar, "claims_intake": claimsIntake,
		"retention_job": retentionJob, "retention_operator": retentionOperator}
	roleCount := 0
	for _, member := range memberships {
		if member {
			roleCount++
		}
	}
	roleValid := memberships[authority] && roleCount == 1 && !hostedMember
	if authority == "hosted_runtime" {
		roleValid = checkoutMember && hostedMember && hostedUsage && !hostedSet && roleCount == 1
	}
	if authority == "meta_ingress" {
		// Predefined data/file/server roles need not own an object or have
		// BYPASSRLS to exceed this producer's deliberately narrow authority.
		roleValid = roleValid && metaUsage && !metaSet && !systemAuthority
	}
	if authority == "meta_consumer" {
		roleValid = roleValid && consumerUsage && !consumerSet && !systemAuthority
	}
	if authority == "meta_worker" {
		roleValid = roleValid && metaWorkerUsage && !metaWorkerSet && !systemAuthority
	}
	if authority == "claims_intake" {
		roleValid = roleValid && claimsIntakeUsage && !claimsIntakeSet && !systemAuthority
	}
	if authority == "retention_job" {
		roleValid = roleValid && retentionJobUsage && !retentionJobSet && !systemAuthority
	}
	if authority == "retention_operator" {
		roleValid = roleValid && retentionOperatorUsage && !retentionOperatorSet && !systemAuthority
	}
	if authority == "media_worker" {
		roleValid = roleValid && mediaWorkerUsage && !mediaWorkerSet && !systemAuthority
	}
	if authority == "media_executor" {
		roleValid = roleValid && mediaExecutorUsage && !mediaExecutorSet && !systemAuthority
	}
	if authority == "media_recovery" {
		roleValid = roleValid && mediaRecoveryUsage && !mediaRecoverySet && !systemAuthority
	}
	if authority == "stripe_ingress" {
		roleValid = roleValid && stripeIngressUsage && !stripeIngressSet && !systemAuthority && !stripeRegistryWriter && !integrationWriter
	}
	if authority == "stripe_registrar" {
		roleValid = roleValid && stripeRegistrarUsage && !stripeRegistrarSet && !systemAuthority && !stripeRegistryWriter && !integrationWriter
	}
	// A privileged login cannot launder its authority with startup SET ROLE:
	// RESET ROLE would recover the session_user's capabilities after admission.
	// Inherited owner authority also permits DDL without SET ROLE; SET FALSE is
	// not a safe substitute for withholding that membership.
	if !sameLogin || !dsnUserMatch || superuser || bypassRLS || roleAdmin || databaseCreator || replication || objectOwner || !roleValid || authMember || checkoutWriterMember || metaWriter || mediaRegistrarUsage || mediaRegistrarSet || mediaWriter || mediaWriterUsage || mediaWriterSet || canReachPrivileged {
		return errors.New("unsafe runtime database role")
	}
	if err := validateStripeAuthority(ctx, pool, authority); err != nil {
		return err
	}
	// The registrar owns no objects; inspect effective direct, PUBLIC and
	// reachable-role EXECUTE on its two fixed definer functions as well.
	var mediaExecute bool
	err = pool.QueryRow(ctx, `
		WITH reachable AS (
			SELECT oid FROM pg_roles WHERE rolname=session_user
			   OR pg_has_role(session_user, oid, 'USAGE') OR pg_has_role(session_user, oid, 'SET')
		), fixed AS (
			SELECT p.oid FROM pg_catalog.pg_proc p
			JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
			WHERE n.nspname='live' AND (
				(p.proname='register_prepared_media' AND p.pronargs=3
				 AND p.proargtypes[0]='pg_catalog.jsonb'::regtype
				 AND p.proargtypes[1]='pg_catalog.bytea'::regtype
				 AND p.proargtypes[2]='pg_catalog.bytea'::regtype)
				OR (p.proname='revoke_prepared_media' AND p.pronargs=4
				 AND p.proargtypes[0]='pg_catalog.uuid'::regtype
				 AND p.proargtypes[1]='pg_catalog.uuid'::regtype
				 AND p.proargtypes[2]='pg_catalog.uuid'::regtype
				 AND p.proargtypes[3]='pg_catalog.text'::regtype)
				OR p.oid IN (SELECT x.oid FROM pg_catalog.pg_proc x WHERE x.pronamespace=n.oid
					AND x.proname IN ('claim_media_operation','load_media_material','reserve_media_start',
					'record_media_observation','finish_media_uncertain',
					'begin_media_recovery_episode','claim_recovery_observation',
					'record_recovery_observation','finish_recovery_observation',
					'read_media_recovery_episode','witness_media_recovery_episode',
					'timeout_media_recovery_episode','media_recovery_ready',
					'media_recovery_begin_replay','media_recovery_native_eligible',
					'qualify_media_recovery_observation'))
			)
		)
		SELECT EXISTS (
			SELECT 1 FROM reachable r CROSS JOIN fixed f
			WHERE has_function_privilege(r.oid, f.oid, 'EXECUTE')
		)`).Scan(&mediaExecute)
	if err != nil || (mediaExecute && authority != "media_executor" && authority != "media_recovery") {
		return errors.New("unsafe runtime database role")
	}
	if authority == "media_executor" || authority == "media_worker" || authority == "media_recovery" {
		if err := validateMediaAuthority(ctx, pool, authority); err != nil {
			return err
		}
	}
	if authority == "meta_worker" {
		// Membership checks cannot see direct/PUBLIC/column grants or a custom
		// non-owner role. Check the effective ACLs of this login and every role it
		// can inherit or SET, in the shared path used by both open and validate.
		// River maintenance must never reach another lane or alter its ledger.
		var forbidden bool
		err := pool.QueryRow(ctx, `
			WITH reachable AS (
				SELECT oid FROM pg_roles
				WHERE pg_has_role(session_user, oid, 'USAGE') OR pg_has_role(session_user, oid, 'SET')
			)
			SELECT EXISTS (
				SELECT 1 FROM reachable r CROSS JOIN pg_class c
				JOIN pg_namespace n ON n.oid=c.relnamespace
				WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				AND CASE
					WHEN n.nspname <> 'river_meta' AND c.relkind='S'
						THEN has_sequence_privilege(r.oid,c.oid,'USAGE,SELECT,UPDATE')
					WHEN n.nspname <> 'river_meta' AND c.relkind IN ('r','p','v','m','f')
						THEN has_table_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
							OR has_any_column_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,REFERENCES')
					WHEN n.nspname='river_meta' AND c.relname='river_migration' AND c.relkind='r'
						THEN has_table_privilege(r.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
							OR has_any_column_privilege(r.oid,c.oid,'INSERT,UPDATE,REFERENCES')
					ELSE false
				END
			) OR EXISTS (
				SELECT 1 FROM reachable r CROSS JOIN pg_namespace n
				WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'
				AND has_schema_privilege(r.oid,n.oid,'CREATE')
			) OR EXISTS (
				SELECT 1 FROM reachable r WHERE has_database_privilege(r.oid,current_database(),'CREATE')
			)`).Scan(&forbidden)
		if err != nil || forbidden {
			return errors.New("unsafe meta worker database privileges")
		}
	}
	return nil
}

// WithScope resolves an opaque session inside a transaction, sets transaction-local
// RLS context, and runs fn. Every non-success path rolls back with an independent,
// bounded cleanup context.
func WithScope(ctx context.Context, pool *pgxpool.Pool, token, storeID, permission string, fn func(pgx.Tx, Scope) error) (err error) {
	if fn == nil {
		return ErrUnauthorized
	}
	return withScopeContext(ctx, pool, token, storeID, permission, func(_ context.Context, tx pgx.Tx, scope Scope) error {
		return fn(tx, scope)
	})
}

// RequirePermission checks a second fixed route permission inside the existing
// scoped transaction. It cannot change scope or open a separate auth snapshot.
func RequirePermission(ctx context.Context, tx pgx.Tx, scope Scope, token, permission string) error {
	if tx == nil || len(token) < 32 || len(token) > 512 {
		return ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(token))
	var status, tenant, principal string
	var revision int64
	err := tx.QueryRow(ctx, `SELECT access_status,coalesce(tenant_id::text,''),coalesce(principal_id::text,''),coalesce(authz_revision,0) FROM identity.resolve_access($1,$2::uuid,$3)`, hash[:], scope.StoreID, permission).Scan(&status, &tenant, &principal, &revision)
	if err != nil {
		return err
	}
	switch status {
	case "unauthorized":
		return ErrUnauthorized
	case "not_found":
		return ErrScopeNotFound
	case "forbidden":
		return ErrForbidden
	case "ok":
		if tenant == scope.TenantID && principal == scope.PrincipalID && revision == scope.Revision {
			return nil
		}
		return ErrForbidden
	default:
		return errors.New("invalid access result")
	}
}

func withScopeContext(ctx context.Context, pool *pgxpool.Pool, token, storeID, permission string, fn func(context.Context, pgx.Tx, Scope) error) (err error) {
	if pool == nil || fn == nil || len(token) < 32 || len(token) > 512 || !isCanonicalUUID(storeID) {
		return ErrUnauthorized
	}
	scopeCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	transaction, err := pool.BeginTx(scopeCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			rollback(transaction)
			panic(recovered)
		}
		if err != nil {
			rollback(transaction)
		}
	}()

	hash := sha256.Sum256([]byte(token))
	var scope Scope
	_, err = transaction.Exec(scopeCtx, `SELECT
		set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true),
		set_config('idle_in_transaction_session_timeout', $3, true)`, requestTimeout.String(), lockTimeout.String(), requestTimeout.String())
	if err != nil {
		return err
	}
	var access string
	err = transaction.QueryRow(scopeCtx, `SELECT access_status, coalesce(tenant_id::text,''),
        coalesce(principal_id::text,''), coalesce(authz_revision,0)
		FROM identity.resolve_access($1, $2::uuid, $3)`, hash[:], storeID, permission).
		Scan(&access, &scope.TenantID, &scope.PrincipalID, &scope.Revision)
	if err != nil {
		return err
	}
	switch access {
	case "unauthorized":
		return ErrUnauthorized
	case "not_found":
		return ErrScopeNotFound
	case "forbidden":
		return ErrForbidden
	case "ok":
	default:
		return errors.New("invalid access result")
	}
	scope.StoreID = storeID
	_, err = transaction.Exec(scopeCtx, `SELECT
		set_config('app.tenant_id', $1, true),
		set_config('app.store_id', $2, true),
		set_config('app.principal_id', $3, true)`, scope.TenantID, scope.StoreID, scope.PrincipalID)
	if err != nil {
		return err
	}
	if err = fn(scopeCtx, transaction, scope); err != nil {
		return err
	}
	return transaction.Commit(scopeCtx)
}

func rollback(transaction pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = transaction.Rollback(cleanup)
}

func isCanonicalUUID(value string) bool {
	if len(value) != 36 || value != strings.ToLower(value) {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
				return false
			}
		}
	}
	return true
}

// NewHandler returns the small API surface. Authorization permissions are fixed
// constants selected by the route, never supplied by a caller.
// HandlerOptions enables only explicitly wired authentication projections.
// The ordinary fixture/business handler must not expand its discovery surface.
type HandlerOptions struct{ SessionStoreList bool }

func NewHandler(pool *pgxpool.Pool, options ...HandlerOptions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), startupTimeout)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if len(options) > 0 && options[0].SessionStoreList {
		mux.HandleFunc("GET /v1/admin/stores", sessionStoresHandler(pool))
	}
	mux.HandleFunc("GET /v1/admin/stores/{store_id}", storeHandler(pool))
	mux.HandleFunc("GET /v1/admin/stores/{store_id}/audit-events", auditHandler(pool))
	return httperror.Middleware(mux)
}

func storeHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		storeID := r.PathValue("store_id")
		var store struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Currency string `json:"currency"`
		}
		err := withRequestScope(r, pool, storeID, storeReadPermission, func(ctx context.Context, tx pgx.Tx, _ Scope) error {
			return tx.QueryRow(ctx, `SELECT id::text, name, currency FROM control.stores WHERE id = $1::uuid`, storeID).
				Scan(&store.ID, &store.Name, &store.Currency)
		})
		switch {
		case errors.Is(err, ErrScopeNotFound):
			writeError(w, http.StatusNotFound, "not_found")
		case errors.Is(err, ErrForbidden):
			writeError(w, http.StatusForbidden, "forbidden")
		case errors.Is(err, ErrUnauthorized):
			writeError(w, http.StatusUnauthorized, "unauthorized")
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "not_found")
		case err != nil:
			writeError(w, http.StatusInternalServerError, "internal")
		default:
			writeJSON(w, http.StatusOK, store)
		}
	}
}

func auditHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		storeID := r.PathValue("store_id")
		events := make([]struct {
			ID        string    `json:"id"`
			Action    string    `json:"action"`
			CreatedAt time.Time `json:"created_at"`
		}, 0)
		err := withRequestScope(r, pool, storeID, auditReadPermission, func(ctx context.Context, tx pgx.Tx, _ Scope) error {
			rows, err := tx.Query(ctx, `SELECT id::text, action, created_at
				FROM ops.audit_events WHERE store_id = $1::uuid ORDER BY created_at DESC LIMIT 50`, storeID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var event struct {
					ID        string    `json:"id"`
					Action    string    `json:"action"`
					CreatedAt time.Time `json:"created_at"`
				}
				if err := rows.Scan(&event.ID, &event.Action, &event.CreatedAt); err != nil {
					return err
				}
				events = append(events, event)
			}
			return rows.Err()
		})
		if errors.Is(err, ErrScopeNotFound) {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		if errors.Is(err, ErrForbidden) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		if errors.Is(err, ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		writeJSON(w, http.StatusOK, events)
	}
}

func withRequestScope(r *http.Request, pool *pgxpool.Pool, storeID, permission string, fn func(context.Context, pgx.Tx, Scope) error) error {
	token, ok := bearerToken(r)
	if !ok {
		return ErrUnauthorized
	}
	requestCtx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	return withScopeContext(requestCtx, pool, token, storeID, permission, fn)
}

func bearerToken(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	return token, token != ""
}

func writeError(w http.ResponseWriter, status int, code string) {
	httperror.Write(w, status, code)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
