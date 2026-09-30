package meta

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

const consumerTimeout = 5 * time.Second

var (
	ErrConsumerJob     = errors.New("meta: invalid consumer job")
	ErrConsumerPolicy  = errors.New("meta: consumer policy unavailable")
	ErrConsumerStorage = errors.New("meta: consumer storage unavailable")
)

// ConsumerWorker borrows a dedicated consumer pool. River job lifecycle runs
// under a separately validated ordinary worker pool.
type ConsumerWorker struct {
	river.WorkerDefaults[inboxJobArgs]
	pool  *pgxpool.Pool
	keys  *PayloadKeyring
	actor ClaimsActorKey // zero = claim staging off (MC01-07 bytes)
}

var _ river.Worker[inboxJobArgs] = (*ConsumerWorker)(nil)

func (ConsumerWorker) String() string     { return "meta.ConsumerWorker{redacted}" }
func (w ConsumerWorker) GoString() string { return w.String() }
func (ConsumerWorker) MarshalJSON() ([]byte, error) {
	return []byte(`"meta.ConsumerWorker{redacted}"`), nil
}

// NewConsumerWorker never stages claims: it equals NewConsumerWorkerWithClaims with the zero key.
func NewConsumerWorker(ctx context.Context, pool *pgxpool.Pool, keys *PayloadKeyring) (*ConsumerWorker, error) {
	return NewConsumerWorkerWithClaims(ctx, pool, keys, ClaimsActorKey{})
}

// NewConsumerWorkerWithClaims additionally stages one text-free claims.meta_intake row per
// qualifying comment on a bound object, inside the consumer transaction, when actor holds a key
// (meta-claims-intake-v1 §5.1). A zero actor never stages.
func NewConsumerWorkerWithClaims(ctx context.Context, pool *pgxpool.Pool, keys *PayloadKeyring, actor ClaimsActorKey) (*ConsumerWorker, error) {
	if ctx == nil || pool == nil || keys == nil || !validPayloadKeyID(keys.activeID) ||
		len(keys.keys) < 1 || len(keys.keys) > 16 {
		return nil, ErrConfig
	}
	if active, ok := keys.keys[keys.activeID]; !ok || active == ([32]byte{}) {
		return nil, ErrConfig
	}
	if err := platform.ValidateMetaConsumerPool(ctx, pool); err != nil {
		return nil, ErrConfig
	}
	return &ConsumerWorker{pool: pool, keys: keys, actor: actor}, nil
}

func (*ConsumerWorker) Timeout(*river.Job[inboxJobArgs]) time.Duration { return consumerTimeout }

type socialLoaded struct {
	outcome     string
	appID       *string
	object      *string
	assetID     *string
	kind        *string
	eventKey    *string
	payloadHash *string
	tenantID    *string
	storeID     *string
	routeID     *string
	routeEpoch  *int64
	keyID       *string
	nonce       []byte
	ciphertext  []byte
}

func (r socialLoaded) empty() bool {
	return r.appID == nil && r.object == nil && r.assetID == nil && r.kind == nil &&
		r.eventKey == nil && r.payloadHash == nil && r.tenantID == nil && r.storeID == nil &&
		r.routeID == nil && r.routeEpoch == nil && r.keyID == nil && r.nonce == nil && r.ciphertext == nil
}

func (r socialLoaded) ready() bool {
	return r.appID != nil && r.object != nil && r.assetID != nil && r.kind != nil &&
		r.eventKey != nil && r.payloadHash != nil && r.tenantID != nil && r.storeID != nil &&
		r.routeID != nil && r.routeEpoch != nil && r.keyID != nil && len(r.nonce) > 0 && len(r.ciphertext) > 0
}

func consumerDatabaseError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22023" {
		return river.JobCancel(ErrConsumerJob)
	}
	return ErrConsumerStorage
}

func (w *ConsumerWorker) Work(ctx context.Context, job *river.Job[inboxJobArgs]) error {
	if job == nil || job.JobRow == nil || job.ID <= 0 || job.Attempt <= 0 ||
		job.Args.Version != 1 || !command.ValidID(job.Args.EventID) ||
		job.Kind != (inboxJobArgs{}).Kind() || job.Queue != inboxQueue {
		return river.JobCancel(ErrConsumerJob)
	}
	if ctx == nil || w == nil || w.pool == nil || w.keys == nil {
		return ErrConsumerStorage
	}
	bounded, cancel := context.WithTimeout(ctx, consumerTimeout)
	defer cancel()
	tx, err := w.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ErrConsumerStorage
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(bounded, `SELECT set_config('statement_timeout','5s',true),
		set_config('lock_timeout','1s',true),set_config('idle_in_transaction_session_timeout','5s',true)`); err != nil {
		return consumerDatabaseError(err)
	}
	var loaded socialLoaded
	err = tx.QueryRow(bounded, `SELECT outcome,app_id,object,asset_id,kind,event_key,payload_hash,
		tenant_id::text,store_id::text,route_id::text,route_epoch,key_id,nonce,ciphertext
		FROM meta_inbox.load_social_event($1::uuid,$2::bigint,$3::integer)`,
		job.Args.EventID, job.ID, job.Attempt).Scan(
		&loaded.outcome, &loaded.appID, &loaded.object, &loaded.assetID, &loaded.kind,
		&loaded.eventKey, &loaded.payloadHash, &loaded.tenantID, &loaded.storeID,
		&loaded.routeID, &loaded.routeEpoch, &loaded.keyID, &loaded.nonce, &loaded.ciphertext)
	if err != nil {
		return consumerDatabaseError(err)
	}
	switch loaded.outcome {
	case "ALREADY", "REVIEWED", "STALE":
		if !loaded.empty() {
			return ErrConsumerStorage
		}
		if err = tx.Commit(bounded); err != nil {
			return consumerDatabaseError(err)
		}
		if loaded.outcome == "ALREADY" {
			return nil
		}
		return river.JobCancel(ErrConsumerPolicy)
	case "READY":
		if !loaded.ready() {
			return ErrConsumerStorage
		}
	default:
		return ErrConsumerStorage
	}
	scope := payloadContext{Class: "event", ID: job.Args.EventID, AppID: *loaded.appID,
		Object: *loaded.object, EventKey: *loaded.eventKey, PayloadHash: *loaded.payloadHash,
		TenantID: *loaded.tenantID, StoreID: *loaded.storeID, RouteID: *loaded.routeID,
		RouteEpoch: *loaded.routeEpoch}
	plaintext, err := w.keys.open(scope, sealedPayload{KeyID: *loaded.keyID,
		Nonce: loaded.nonce, Ciphertext: loaded.ciphertext})
	if err != nil {
		return ErrConsumerPayload
	}
	projection, err := projectSocial(scope, *loaded.assetID, *loaded.kind, plaintext)
	if err != nil {
		return ErrConsumerPayload
	}
	if _, err = tx.Exec(bounded, `SELECT meta_inbox.finish_social_event($1::uuid,$2::bigint,$3::integer,$4::text,$5::text)`,
		job.Args.EventID, job.ID, job.Attempt, projection.family, projection.subjectKey); err != nil {
		return consumerDatabaseError(err)
	}
	if w.actor.set && projection.family == "comment" {
		if err = w.stageClaim(bounded, tx, job, loaded, plaintext); err != nil {
			return err
		}
	}
	if err = tx.Commit(bounded); err != nil {
		return consumerDatabaseError(err)
	}
	return nil
}

// stageClaim is the one sanctioned Meta -> claims edge (§5.1): after the comment fact is
// written and before COMMIT it hands meta_inbox.stage_claim_intake the qualification of the
// already projected unit. An unqualified comment stages nothing (the social fact still commits).
// Any staging error rolls back the whole transaction, so no fact commits without its intake
// decision; the job is retried by River (never cancelled: cancelling would drop the social fact
// for a transient or configuration fault). No network, no River, no claims table access.
// meta_inbox.stage_claim_intake: owner commerce_meta_writer, EXECUTE commerce_meta_consumer only.
func (w *ConsumerWorker) stageClaim(ctx context.Context, tx pgx.Tx, job *river.Job[inboxJobArgs], loaded socialLoaded, plaintext []byte) error {
	candidate, ok := qualifyClaim(*loaded.object, *loaded.assetID, *loaded.kind, plaintext)
	if !ok {
		return nil
	}
	var keyword *string
	var quantity *int32
	var explicit *bool
	if candidate.Parsed.Keyword != "" {
		keyword = &candidate.Parsed.Keyword
	}
	if candidate.Parsed.Quantity > 0 {
		q := int32(candidate.Parsed.Quantity)
		quantity = &q
	}
	if candidate.Parsed.Kind == grammar.Match {
		explicit = &candidate.Parsed.Explicit
	}
	actor := ClaimActorKey(w.actor, *loaded.object, *loaded.assetID, candidate.FromID)
	var staged *string
	if err := tx.QueryRow(ctx, `SELECT meta_inbox.stage_claim_intake($1::uuid,$2::bigint,$3::integer,$4::text,$5::text,$6::text,
		NULL::timestamptz,$7::text,$8::text,$9::integer,$10::boolean)::text`,
		job.Args.EventID, job.ID, job.Attempt, candidate.ObjectID, candidate.CommentRef, actor,
		string(candidate.Parsed.Kind), keyword, quantity, explicit).Scan(&staged); err != nil {
		return ErrConsumerStorage
	}
	return nil
}
