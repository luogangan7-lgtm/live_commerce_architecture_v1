// Package claimsintake owns the claims intake poll loop (meta-claims-intake-v1 §5.3): it leases one
// claims.meta_intake row per transaction, applies it as a claim (claims.IngestMetaIntake) and, when
// that created a bundle on a private_reply source, plans the first private reply in the same
// transaction (integration.claim_reply_plannable, the River job, integration.plan_claim_reply).
//
// It never talks to Meta, never sends the reply (the dispatcher route in internal/integrations/
// metareply does, from another pool and process role), never reads comment text (the intake row is
// text-free), never holds a Page token, and never retries by looping inside one transaction: a
// failed apply is rolled back and recorded once by claims.fail_meta_intake, which owns the backoff
// and the 10-attempt cap.
package claimsintake

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
)

// ErrConfig is the only error New returns for a bad Config, pool or key (no driver text).
var ErrConfig = errors.New("claimsintake: invalid configuration")

// Config sizes the poller. Zero values mean the defaults (2 workers, 1 s idle sleep).
type Config struct {
	Workers   int           // 1..8
	IdleSleep time.Duration // >= 10 ms
}

// applyTimeout bounds one apply transaction: statement_timeout 5 s and lock_timeout 1 s (§5.3);
// the context deadline is the backstop for a stalled connection.
const (
	statementTimeout = "5s"
	lockTimeout      = "1s"
	applyDeadline    = 15 * time.Second
	failDeadline     = 5 * time.Second
)

// Poller applies claims.meta_intake rows. Create it with New; Run and ApplyOne are safe for
// concurrent use (each call owns its transaction).
type Poller struct {
	pool    *pgxpool.Pool
	jobs    *river.Client[pgx.Tx] // insert-only client on the main river schema, intake login
	linkKey claims.ReplyLinkKey
	cfg     Config
}

// New validates the pool (dedicated commerce_claims_intake login), the link key and cfg. The pool
// stays owned by the caller.
func New(ctx context.Context, intakePool *pgxpool.Pool, linkKey claims.ReplyLinkKey, cfg Config) (*Poller, error) {
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	if cfg.IdleSleep == 0 {
		cfg.IdleSleep = time.Second
	}
	if ctx == nil || intakePool == nil || linkKey.ID() == "" || cfg.Workers < 1 || cfg.Workers > 8 || cfg.IdleSleep < 10*time.Millisecond {
		return nil, ErrConfig
	}
	if err := platform.ValidateClaimsIntakePool(ctx, intakePool); err != nil {
		return nil, ErrConfig
	}
	// No Workers and no Queues: an insert-only client (River lifecycle stays in cmd/claims-worker's
	// commerce_worker client). River's log output is discarded because job args carry operation ids.
	jobs, err := river.NewClient(riverpgxv5.New(intakePool), &river.Config{
		Schema: "river", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return nil, ErrConfig
	}
	return &Poller{pool: intakePool, jobs: jobs, linkKey: linkKey, cfg: cfg}, nil
}

// Run starts cfg.Workers goroutines of ApplyOne, sleeping IdleSleep after an idle poll or a
// failure, until ctx is done, then waits for them and returns nil.
func (p *Poller) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrConfig
	}
	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				leased, err := p.ApplyOne(ctx)
				if err != nil && ctx.Err() == nil { // shutdown cancellation is not a failure
					// Fixed fields only: an error value can carry row data through a driver message.
					slog.Warn("claims_intake_apply_error", "sqlstate", sqlState(err), "leased", leased)
				}
				if leased && err == nil {
					continue // backlog: no sleep between rows
				}
				select {
				case <-ctx.Done():
				case <-time.After(p.cfg.IdleSleep):
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

// ApplyOne leases and applies at most one intake row. It returns (false, nil) when nothing is due,
// (true, nil) when a row was applied or its failure was recorded, and (true, err) when the apply
// failed and the failure could not be recorded (the row stays leased-free PENDING with no attempt
// counted; the next poll retries it), or (false, err) when leasing itself failed. A cancelled ctx
// rolls back and records nothing (shutdown is not an attempt).
func (p *Poller) ApplyOne(ctx context.Context) (leased bool, err error) {
	if ctx == nil {
		return false, ErrConfig
	}
	txCtx, cancel := context.WithTimeout(ctx, applyDeadline)
	defer cancel()
	tx, err := p.pool.BeginTx(txCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(txCtx)) }()
	if _, err = tx.Exec(txCtx, `SELECT set_config('statement_timeout',$1,true),set_config('lock_timeout',$2,true)`, statementTimeout, lockTimeout); err != nil {
		return false, err
	}
	// claims.lease_meta_intake: definer commerce_claims_writer; needs unset app.* GUCs, so it runs first.
	var id, tenant, store, source string
	err = tx.QueryRow(txCtx, `SELECT id::text,tenant_id::text,store_id::text,source_id::text FROM claims.lease_meta_intake()`).
		Scan(&id, &tenant, &store, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(txCtx)
	}
	if err != nil {
		return false, err
	}
	if err = p.apply(txCtx, tx, id, tenant, store, source); err == nil {
		if err = tx.Commit(txCtx); err == nil {
			return true, nil
		}
	}
	// Rolled back (deferred) or commit unacknowledged: record the failure in a fresh short tx.
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	_ = tx.Rollback(context.WithoutCancel(txCtx))
	code, final := classify(err)
	failCtx, failCancel := context.WithTimeout(context.WithoutCancel(ctx), failDeadline)
	defer failCancel()
	// claims.fail_meta_intake: definer commerce_claims_writer; bumps attempts and backoff, FAILED when final or at 10.
	if _, ferr := p.pool.Exec(failCtx, `SELECT claims.fail_meta_intake($1::uuid,$2,$3)`, id, code, final); ferr != nil {
		return true, ferr
	}
	return true, nil
}

// apply is the body of one leased transaction (§5.3 steps 2-4).
func (p *Poller) apply(ctx context.Context, tx pgx.Tx, id, tenant, store, source string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true)`, tenant, store); err != nil {
		return err
	}
	res, err := claims.IngestMetaIntake(ctx, tx, id)
	if err != nil {
		return err
	}
	if res.EventID == "" {
		// WINDOW_CLOSED is not a claims event (claims §4.3 step 3); the intake row records the drop.
		return finish(ctx, tx, `UPDATE claims.meta_intake SET state='DROPPED',drop_reason='window_closed',lease_xid=NULL,updated_at=clock_timestamp()`)
	}
	if res.Outcome == claims.OutcomeAccepted && res.BundleCreated {
		var privateReply bool
		if err = tx.QueryRow(ctx, `SELECT private_reply FROM live.claim_sources WHERE tenant_id=$1 AND store_id=$2 AND id=$3`,
			tenant, store, source).Scan(&privateReply); err != nil {
			return err
		}
		if privateReply {
			if err = p.planReply(ctx, tx, id, tenant, store, res.BundleID); err != nil {
				return err
			}
		}
	}
	// The update has no WHERE on purpose: the intake_update policy exposes only the row leased by this
	// transaction, and finish requires exactly one affected row.
	return finish(ctx, tx, `UPDATE claims.meta_intake SET state='APPLIED',applied_event_id=$1::uuid,lease_xid=NULL,updated_at=clock_timestamp()`, res.EventID)
}

func finish(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return command.ErrInvalid
	}
	return nil
}

// planReply is §6.2: a reply precondition that fails is an audited skip inside claim_reply_plannable
// (the claim still commits); only OK inserts the River job and plans the operation.
func (p *Poller) planReply(ctx context.Context, tx pgx.Tx, intake, tenant, store, bundle string) error {
	var code string
	// integration.claim_reply_plannable: definer commerce_integration_writer; locks binding then source FOR SHARE.
	if err := tx.QueryRow(ctx, `SELECT integration.claim_reply_plannable($1::uuid)`, intake).Scan(&code); err != nil {
		return err
	}
	if code != "OK" {
		return nil
	}
	var operation string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&operation); err != nil {
		return err
	}
	job, err := core.InsertOperationJob(ctx, p.jobs, tx, operation)
	if err != nil {
		return err
	}
	token, err := claims.SystemLinkToken(p.linkKey, tenant, store, bundle, operation)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	// integration.plan_claim_reply: definer commerce_integration_writer; writes the system link, the READY
	// meta.private_reply operation, its event and audit; verifies the job is this transaction's row.
	var planned string
	return tx.QueryRow(ctx, `SELECT integration.plan_claim_reply($1::uuid,$2::uuid,$3::bytea,$4,$5)::text`,
		intake, operation, hash[:], p.linkKey.ID(), job).Scan(&planned)
}

// classify maps an apply error to the fail_meta_intake code and finality (§5.3). Final: invalid
// (command.ErrInvalid / 22023), reply_key_conflict (23505 from the plan), not_found (a vanished row,
// e.g. 23503 through claims.mapError) and raw 23514/23503/42501 as sqlstate_<code>. Everything else
// (deadlock, lock or statement timeout, connection loss, context timeout, unknown) retries under the
// 10-attempt cap with the backoff owned by claims.fail_meta_intake.
func classify(err error) (code string, final bool) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "22023":
			return "invalid", true
		case "23505":
			return "reply_key_conflict", true
		case "23514", "23503", "42501":
			return "sqlstate_" + strings.ToLower(pg.Code), true
		}
		return "sqlstate_" + strings.ToLower(pg.Code), false
	}
	switch {
	case errors.Is(err, command.ErrInvalid):
		return "invalid", true
	case errors.Is(err, command.ErrNotFound):
		return "not_found", true
	case errors.Is(err, command.ErrConflict):
		return "conflict", false
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", false
	}
	return "error", false
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}
