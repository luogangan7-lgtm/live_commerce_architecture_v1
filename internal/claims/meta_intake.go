// meta_intake.go owns IngestMetaIntake, the meta source of the shared ingest core
// (ingest.go): it applies one leased claims.meta_intake row as one claims.events row and,
// when accepted, an absolute line target (meta-claims-intake-v1 §5.3, live-keyword-claims-v1
// §4.4 clauses 6-7).
//
// Non-goals: it never changes intake state (the poller writes APPLIED/DROPPED/FAILED after
// this returns), never plans or sends the private reply (integration.plan_claim_reply, called
// by internal/claimsintake), never reads Meta storage or comment text (the row is text-free),
// never opens a transaction or touches River.
//
// KC15 guard: IngestMetaIntake is called only from the intake worker; Ingest/IngestParsed only from
// RecordManualClaim.

package claims

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
)

// Abuse bounds (meta-claims-intake-v1 §4.2, IR-6): at most metaActorLimit ACCEPTED commands per
// (session, actor) whose occurred_at lies in (occurred_at-metaActorWindow, occurred_at], and at
// most metaSessionBundleCap non-manual bundles per session. Event time, not receipt time, so a
// backlog replay of legitimately spaced comments is not rate-limited.
const (
	metaActorLimit       = 10
	metaActorWindow      = 60 // seconds
	metaSessionBundleCap = 5000
)

// metaIngest carries the facts of the leased claims.meta_intake row that the shared core needs
// beyond IngestInput/grammar.Result. offerID is the offer the staged keyword resolved to;
// unknownKeyword means it resolved to none (and no keyword was ever stored).
type metaIngest struct {
	sourceID       string
	receivedAt     time.Time
	offerID        string
	unknownKeyword bool
}

func validMetaIngest(m *metaIngest) bool {
	return m != nil && command.ValidID(m.sourceID) && !m.receivedAt.IsZero() &&
		(m.offerID == "" || (command.ValidID(m.offerID) && !m.unknownKeyword))
}

// IngestMetaIntake applies the claims.meta_intake row intakeID in the caller's READ COMMITTED
// transaction of the commerce_claims_intake login. Preconditions, else command.ErrInvalid with
// nothing written: intakeID is leased in THIS transaction by claims.lease_meta_intake() and still
// PENDING; app.tenant_id/app.store_id are set local to its scope; app.principal_id and
// app.buyer_id are unset. It never changes intake state. A missing window interval, an inactive
// source or an occurred_at more than 120 s in the future is {REJECTED, WINDOW_CLOSED, EventID ""}
// with a nil error (claims §4.3 step 3: not persisted). DB errors stay unwrappable
// (errors.As(err, **pgconn.PgError)) except the bare sentinels of mapError; the poller classifies
// them (§5.3). Caller: internal/claimsintake Poller.
func IngestMetaIntake(ctx context.Context, tx pgx.Tx, intakeID string) (IngestResult, error) {
	if ctx == nil || tx == nil || !command.ValidID(intakeID) {
		return IngestResult{}, command.ErrInvalid
	}
	var (
		in                        IngestInput
		m                         metaIngest
		kind                      string
		quantity                  *int32
		explicit                  *bool
		occurredAt, receivedAtRaw time.Time
	)
	err := tx.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,session_id::text,source_id::text,inbox_event_id::text,
		platform,actor_key,occurred_at,received_at,grammar_kind,coalesce(offer_id::text,''),unknown_keyword,quantity,explicit_quantity
		FROM claims.meta_intake WHERE id=$1 AND state='PENDING' AND lease_xid=pg_current_xact_id()`, intakeID).
		Scan(&in.TenantID, &in.StoreID, &in.SessionID, &m.sourceID, &in.SourceEventID, &in.Platform, &in.ActorKey,
			&occurredAt, &receivedAtRaw, &kind, &m.offerID, &m.unknownKeyword, &quantity, &explicit)
	if errors.Is(err, pgx.ErrNoRows) {
		return IngestResult{}, command.ErrInvalid // not leased in this transaction (RLS hides every other row)
	}
	if err != nil {
		return IngestResult{}, mapError(err)
	}
	in.SourceKind = metaSource
	in.OccurredAt = occurredAt.UTC()
	m.receivedAt = receivedAtRaw.UTC()

	p := grammar.Result{Version: grammar.Version, Kind: grammar.Kind(kind)}
	if p.Kind != grammar.NoMatch && !m.unknownKeyword {
		// Offer keywords are immutable, so this plain read is stable; the core re-reads and locks
		// the offer by id. Only the offer's own keyword is used (an unresolved head is never stored).
		err = tx.QueryRow(ctx, `SELECT keyword FROM live.offers WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND id=$4::uuid`,
			in.TenantID, in.StoreID, in.SessionID, m.offerID).Scan(&p.Keyword)
		if err != nil {
			return IngestResult{}, mapError(err)
		}
		if p.Kind == grammar.Match && quantity != nil && explicit != nil {
			p.Quantity, p.Explicit = int64(*quantity), *explicit
		}
	}
	return ingest(ctx, tx, in, p, &m)
}

// matchMetaWindow is the meta window step (§5.3 step 2): lock the session's window row FOR SHARE
// in any state (a concurrent close or reopen waits for this transaction), THEN read the source
// and select the interval with opened_at <= occurred_at and (still open, or occurred_at <
// closed_at and received within 60 s after the close). ok=false is WINDOW_CLOSED: no window row,
// inactive source, future occurred_at (>120 s), or no covering interval. The returned window
// carries the matched interval's generation (the event's fence) and the window row's current
// match_mode (intervals store no mode: a late webhook for an earlier generation uses the
// current mode, documented limit).
func matchMetaWindow(ctx context.Context, tx pgx.Tx, in IngestInput, m *metaIngest, inClock bool) (window, bool, error) {
	var w window
	err := tx.QueryRow(ctx, `SELECT match_mode FROM live.claim_windows WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 FOR SHARE`,
		in.TenantID, in.StoreID, in.SessionID).Scan(&w.mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return window{}, false, nil
	}
	if err != nil {
		return window{}, false, mapError(err)
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT active FROM live.claim_sources WHERE tenant_id=$1 AND store_id=$2 AND id=$3::uuid`,
		in.TenantID, in.StoreID, m.sourceID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return window{}, false, nil
	}
	if err != nil {
		return window{}, false, mapError(err)
	}
	if !active || !inClock {
		return window{}, false, nil
	}
	err = tx.QueryRow(ctx, `SELECT generation,opened_at FROM live.claim_window_intervals
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND opened_at<=$4
		 AND (closed_at IS NULL OR ($4<closed_at AND $5<=closed_at+interval '60 seconds'))
		ORDER BY generation DESC LIMIT 1`,
		in.TenantID, in.StoreID, in.SessionID, in.OccurredAt, m.receivedAt).Scan(&w.generation, &w.openedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return window{}, false, nil
	}
	if err != nil {
		return window{}, false, mapError(err)
	}
	return w, true, nil
}

// metaRateLimited applies the §4.2 bounds under the actor and session-cap advisories the caller
// already holds: the actor has metaActorLimit ACCEPTED meta commands in the metaActorWindow
// seconds up to this command's occurred_at, or the session already has metaSessionBundleCap
// non-manual bundles and this actor has none yet (an existing bundle adds no bundle).
func metaRateLimited(ctx context.Context, tx pgx.Tx, in IngestInput) (bool, error) {
	var recent int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM claims.events e
		JOIN claims.bundles b ON b.tenant_id=e.tenant_id AND b.store_id=e.store_id AND b.id=e.bundle_id
		WHERE e.tenant_id=$1 AND e.store_id=$2 AND e.session_id=$3 AND e.source_kind='meta' AND e.outcome='ACCEPTED'
		 AND b.actor_key=$4 AND e.occurred_at>$5::timestamptz-make_interval(secs=>$6) AND e.occurred_at<=$5::timestamptz`,
		in.TenantID, in.StoreID, in.SessionID, in.ActorKey, in.OccurredAt, float64(metaActorWindow)).Scan(&recent)
	if err != nil {
		return false, mapError(err)
	}
	if recent >= metaActorLimit {
		return true, nil
	}
	var bundles int64
	var actorHasBundle bool
	err = tx.QueryRow(ctx, `SELECT count(*),coalesce(bool_or(actor_key=$4),false) FROM claims.bundles
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND platform<>'manual'`,
		in.TenantID, in.StoreID, in.SessionID, in.ActorKey).Scan(&bundles, &actorHasBundle)
	if err != nil {
		return false, mapError(err)
	}
	return !actorHasBundle && bundles >= metaSessionBundleCap, nil
}
