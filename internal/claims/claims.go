// Package claims owns live keyword offers, claim windows, claim bundles and claim-link redemption.
// It never reserves inventory, sends messages, reads Meta storage or creates orders.
//
// Contract: contracts/live-keyword-claims-v1.md (FROZEN, with the §0.1 integrator rulings)
// and contracts/meta-claims-intake-v1.md (FROZEN; §4.4 amends the former). Two ingress
// paths share one unexported core (ingest.go): RecordManualClaim is the only production
// caller of Ingest/IngestParsed; IngestMetaIntake (meta_intake.go) is called only by the
// claims intake worker (internal/claimsintake, commerce_claims_intake login).
//
// Files: claims.go (shared types, authority, errors, locks), grammar/ (pure kw-v1),
// merchant.go (board, window, offers, bundle list), manual.go (RecordManualClaim),
// ingest.go (shared ingest core, Ingest/IngestParsed), meta_intake.go (IngestMetaIntake),
// link.go (IssueLink), credentials.go (LabelKey, LinkToken, redaction), buyer.go
// (PreviewLink/RedeemLink).
//
// Depends on (and only on): command (receipts, audit, sentinel errors), platform (merchant
// scope and RequirePermission), buyer (buyer scope, RunCommand), storefront (the only cart
// writer), pagination (claim-bundles cursors), claims/grammar, pgx and the standard
// library. Only HTTP adapters and internal/claimsintake import this package. It never
// imports inventory, checkout, integrations/meta or River (Meta facts reach it only as rows
// of claims.meta_intake staged by the consumer's SQL edge meta_inbox.stage_claim_intake).
//
// Tables and roles: live.offers, live.claim_windows and claims.{bundles,lines,events} are
// written by commerce_runtime under RLS + column grants (migrations/0060_live_claims.sql).
// Buyer binding, applied state and link hashes are written only by the NOLOGIN
// commerce_claims_writer through claims.issue_link (merchant) and claims.redeem_link /
// claims.mark_applied (buyer); claims.preview_link is the read-only buyer projection.
//
// Transactions: every exported function takes a caller-owned pgx.Tx (platform.WithScope
// for merchant functions, buyer.WithScope for buyer functions, both READ COMMITTED) and
// the caller must roll back on every returned error. No function returns a partial value
// together with an error. Grammar/offer/window rejections are data (Outcome=REJECTED),
// never errors.
package claims

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// Claims reuse the live permissions in this slice (contract §0.1).
const (
	readPermission   = "live:read"
	managePermission = "live:manage"
)

// Bounds fixed by the contract (§1, §5.7).
const (
	maxOffersPerSession = 200
	maxLinesPerBundle   = 50
	maxIngestTextBytes  = 65536
)

// MatchMode is the claim window's quantity rule.
type MatchMode string // "EXACT" | "KEYWORD_QTY_ONLY"

const (
	MatchExact          MatchMode = "EXACT"
	MatchKeywordQtyOnly MatchMode = "KEYWORD_QTY_ONLY"
)

// Reason explains a REJECTED command. All but WINDOW_CLOSED are persisted in claims.events;
// WINDOW_CLOSED is reported but never persisted. RATE_LIMITED exists for source_kind='meta'
// only (meta-claims-intake-v1 §4.2).
type Reason string

const (
	ReasonNoMatch          Reason = "NO_MATCH"
	ReasonUnknownKeyword   Reason = "UNKNOWN_KEYWORD"
	ReasonOfferInactive    Reason = "OFFER_INACTIVE"
	ReasonInvalidQuantity  Reason = "INVALID_QUANTITY"
	ReasonQuantityRequired Reason = "QUANTITY_REQUIRED"
	ReasonQuantityOverMax  Reason = "QUANTITY_OVER_MAX"
	ReasonBundleLimit      Reason = "BUNDLE_LIMIT"
	ReasonRateLimited      Reason = "RATE_LIMITED"
	ReasonWindowClosed     Reason = "WINDOW_CLOSED"
)

// persistedReasons lists every reason claims.events can store (boardReasons is the reported subset).
var persistedReasons = []Reason{ReasonNoMatch, ReasonUnknownKeyword, ReasonOfferInactive,
	ReasonInvalidQuantity, ReasonQuantityRequired, ReasonQuantityOverMax, ReasonBundleLimit, ReasonRateLimited}

// boardReasons is the closed 7-key set the merchant board's Stats.Rejected reports (the Studio
// parser apps/admin/lib/claims-model.ts requires exactly these keys). RATE_LIMITED is persisted
// but not reported until the claims-board UI unit adds it in all locales; readStats skips it.
var boardReasons = []Reason{ReasonNoMatch, ReasonUnknownKeyword, ReasonOfferInactive,
	ReasonInvalidQuantity, ReasonQuantityRequired, ReasonQuantityOverMax, ReasonBundleLimit}

// Outcomes and window states as stored in the database.
const (
	OutcomeAccepted = "ACCEPTED"
	OutcomeRejected = "REJECTED"
	WindowOpen      = "OPEN"
	WindowClosed    = "CLOSED"
)

// Source kinds. "manual" is RecordManualClaim's kind and platform; "meta" (platform facebook
// or instagram) is accepted only through IngestMetaIntake.
const (
	manualSource = "manual"
	metaSource   = "meta"
)

// authorize mirrors live.authorize (internal/live/draft.go): canonical scope, READ
// COMMITTED, exact tenant/store/principal GUC equality with the platform.Scope, then
// platform.RequirePermission against identity.resolve_access. Merchant functions call it
// before work, inside command.Run after lock waits, and after command.Run returns
// (including a replay), so a revoked token is denied at every point.
func authorize(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, permission string) error {
	if ctx == nil || tx == nil || !command.ValidID(scope.TenantID) || !command.ValidID(scope.StoreID) ||
		!command.ValidID(scope.PrincipalID) || scope.Revision < 1 {
		return command.ErrInvalid
	}
	var isolation, tenantID, storeID, principalID string
	err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'),
		coalesce(current_setting('app.tenant_id',true),''),
		coalesce(current_setting('app.store_id',true),''),
		coalesce(current_setting('app.principal_id',true),'')`).
		Scan(&isolation, &tenantID, &storeID, &principalID)
	if err != nil {
		return err
	}
	if isolation != "read committed" || tenantID != scope.TenantID || storeID != scope.StoreID || principalID != scope.PrincipalID {
		return command.ErrInvalid
	}
	return platform.RequirePermission(ctx, tx, scope, token, permission)
}

// requireSession proves the live session is in the transaction's store. It is a plain
// read of live.sessions (owned by package live): no row lock, so UpdateDraft's session
// FOR UPDATE and media FOR SHARE never wait on claims (contract §3, §4.2 CreateOffer).
func requireSession(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID string) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM live.sessions WHERE tenant_id=$1 AND store_id=$2 AND id=$3`,
		scope.TenantID, scope.StoreID, sessionID).Scan(&one)
	return mapError(err)
}

// waitAdvisory takes a transaction-scoped advisory lock with lock_timeout lifted only for
// this wait (the command.Run pattern): a valid duplicate or concurrent writer may hold it
// longer than the 1 s row-lock budget, and the 5 s statement timeout still bounds it.
// Keys are stable scope strings, never payload. Used for the claim-source dedup lock
// (Ingest) and the per-session offer cap (CreateOffer); see the §5.6 global lock order.
func waitAdvisory(ctx context.Context, tx pgx.Tx, key string) error {
	var timeout string
	if err := tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&timeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout','0',true)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout',$1,true)`, timeout)
	return err
}

// ErrBillingRestricted is SQLSTATE PT412 raised by the 0079 trigger billing_guard_window_open when a store
// in RESTRICTED billing standing opens a NEW claim window (customers-billing-v1 BD5, C-3). PT402 is not
// reused: it already means insufficient stock. httpapi maps it to 402 billing_restricted.
var ErrBillingRestricted = errors.New("billing restricted")

// mapError is the contract §4.5 table. Constraint violations whose DETAIL can echo row
// data (23505, 23514, 23503) become bare sentinels, never a wrapped *pgconn.PgError, so a
// label cannot reach callers, responses or application logs (§0.1 P2(b)); labels are also
// validated in Go first so the database never has to reject one. Lock, deadlock and
// timeout errors (40P01, 55P03, 57014) and anything unknown are returned unchanged for
// the HTTP layer's 503 mapping.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "22023", "23514", "22P02":
			return command.ErrInvalid
		case "23505", "PT409":
			return command.ErrConflict
		case "23503":
			return command.ErrNotFound
		case "PT412":
			return ErrBillingRestricted
		case "PT401":
			return platform.ErrUnauthorized
		case "PT403":
			return platform.ErrForbidden
		case "PT404":
			return platform.ErrScopeNotFound
		}
	}
	return err
}
