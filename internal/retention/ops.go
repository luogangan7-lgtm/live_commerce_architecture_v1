// ops.go holds the database calls of package retention: RunOnce (job and operator), Erase,
// SetPolicy, GetStatus and Replay (operator). Each is exactly one call of a 0071 definer, i.e.
// one transaction; the definer picks the rows, this file only validates shapes, maps SQLSTATEs
// to the fixed errors and returns numbers. Non-goals: no retries (busy means the next hour or the
// operator retries), no logging, no row selection, no network.

package retention

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hex64Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	assetPattern   = regexp.MustCompile(`^[0-9]{1,40}$`)
	commentPattern = regexp.MustCompile(`^[0-9_]{1,80}$`)
)

// mapError turns a definer failure into one of the fixed errors. SQLSTATEs are the contract's:
// 22023 usage, PT404 not found, PT409 conflict, 55P03 lock_timeout (busy). Everything else,
// including connection and driver errors (which can echo a DSN), collapses to one fixed error.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "22023":
			return ErrUsage
		case "PT404":
			return ErrNotFound
		case "PT409":
			return ErrConflict
		case "55P03":
			// busy: another run or erasure holds hashtextextended('claims-retention',0) or a row for over 2 s.
			return ErrBusy
		}
	}
	return errFailed
}

// parseCounts decodes a definer's jsonb result. Non-numeric values are an error; keys outside
// allowedCounts are dropped. The raw map is also returned for fields that are not counts (held).
func parseCounts(raw []byte) (Counts, map[string]int64, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, errFailed
	}
	all := make(map[string]int64, len(m))
	out := make(Counts, len(m))
	for k, v := range m {
		// A quoted number, boolean or fraction is not a count: the definers emit integers only.
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return nil, nil, errFailed
		}
		all[k] = n
		if allowedCounts[k] {
			out[k] = n
		}
	}
	return out, all, nil
}

// RunOnce runs one batch of claims.run_retention (limit 1..1000). Busy (another run holds the
// advisory key) is Counts{"busy":1}, nil, not an error. One statement = one transaction, so a
// crash between batches loses nothing.
func RunOnce(ctx context.Context, pool *pgxpool.Pool, limit int) (Counts, error) {
	if pool == nil || limit < 1 || limit > 1000 {
		return nil, ErrUsage
	}
	var raw []byte
	// claims.run_retention: RD3 platform-level batch, rows chosen by the definer's class rules.
	if err := pool.QueryRow(ctx, `SELECT claims.run_retention($1::integer)`, limit).Scan(&raw); err != nil {
		return nil, mapError(err)
	}
	c, _, err := parseCounts(raw)
	return c, err
}

// validSelector mirrors claims.erase_actor's shape check so a bad call fails before the network.
func validSelector(s Selector) bool {
	a, b, c := s.ActorKey != "", s.CommentRef != "", s.Bundle != ""
	n := 0
	for _, x := range []bool{a, b, c} {
		if x {
			n++
		}
	}
	if !uuidPattern.MatchString(s.Request) || n != 1 {
		return false
	}
	if c {
		return uuidPattern.MatchString(s.Bundle) && uuidPattern.MatchString(s.Tenant) && uuidPattern.MatchString(s.Store) &&
			s.Object == "" && s.Asset == "" && len(s.PeerKeys) == 0
	}
	if (s.Object != "page" && s.Object != "instagram") || !assetPattern.MatchString(s.Asset) || s.Tenant != "" || s.Store != "" {
		return false
	}
	if b {
		return commentPattern.MatchString(s.CommentRef) && len(s.PeerKeys) == 0
	}
	if !hex64Pattern.MatchString(s.ActorKey) || len(s.PeerKeys) > 8 {
		return false
	}
	for _, k := range s.PeerKeys {
		if !hex64Pattern.MatchString(k) {
			return false
		}
	}
	return true
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Erase erases one actor synchronously (RD4). Held means the RD5 hold refused it and nothing was
// written; a repeat of the same request id and selector returns the stored counts with replayed=1.
func Erase(ctx context.Context, pool *pgxpool.Pool, s Selector) (Counts, Held, error) {
	if pool == nil || !validSelector(s) {
		return nil, Held{}, ErrUsage
	}
	var peers any
	if len(s.PeerKeys) > 0 {
		peers = s.PeerKeys
	}
	var raw []byte
	// claims.erase_actor: RD4 one tx, RD5 hold inside the lock.
	err := pool.QueryRow(ctx, `SELECT claims.erase_actor($1::uuid,$2::text,$3::text,$4::text,$5::text,$6::uuid,$7::uuid,$8::uuid,$9::text[])`,
		s.Request, nz(s.Object), nz(s.Asset), nz(s.ActorKey), nz(s.CommentRef), nz(s.Tenant), nz(s.Store), nz(s.Bundle), peers).Scan(&raw)
	if err != nil {
		return nil, Held{}, mapError(err)
	}
	c, all, err := parseCounts(raw)
	if err != nil {
		return nil, Held{}, err
	}
	if all["held"] == 1 {
		return nil, Held{RetryAfter: time.Unix(all["retry_after"], 0).UTC()}, nil
	}
	return c, Held{}, nil
}

// SetPolicy changes the retention policy by CAS on expectedVersion and returns the new version.
func SetPolicy(ctx context.Context, pool *pgxpool.Pool, expectedVersion int64, p Policy) (int64, error) {
	if pool == nil || expectedVersion <= 0 {
		return 0, ErrUsage
	}
	var v int64
	// claims.set_retention_policy: CAS on version, audited as a policy_set log row.
	err := pool.QueryRow(ctx, `SELECT claims.set_retention_policy($1::bigint,$2::boolean,$3::integer,$4::integer,$5::integer,$6::integer)`,
		expectedVersion, p.Enforced, p.LinkDays, p.IntakeDays, p.ClaimsDays, p.SocialDays).Scan(&v)
	if err != nil {
		return 0, mapError(err)
	}
	return v, nil
}

// GetStatus reads the policy and the latest run (claims.retention_status, numbers only).
func GetStatus(ctx context.Context, pool *pgxpool.Pool) (Status, error) {
	if pool == nil {
		return Status{}, ErrUsage
	}
	var raw []byte
	// claims.retention_status: read-only; the job login may call it (deploy smoke).
	if err := pool.QueryRow(ctx, `SELECT claims.retention_status()`).Scan(&raw); err != nil {
		return Status{}, mapError(err)
	}
	_, m, err := parseCounts(raw)
	if err != nil {
		return Status{}, err
	}
	return Status{
		Policy: Policy{Enforced: m["enforced"] == 1, LinkDays: int(m["link_days"]), IntakeDays: int(m["intake_days"]),
			ClaimsDays: int(m["claims_days"]), SocialDays: int(m["social_days"])},
		Version: m["version"], LastRunUnix: m["last_run_unix"], LastRunMore: m["last_run_more"] == 1,
	}, nil
}

type tombstoneJSON struct {
	RequestID      string  `json:"request_id"`
	SelectorDigest string  `json:"selector_digest"`
	ActorDigest    *string `json:"actor_digest"`
	BundleTenant   *string `json:"bundle_tenant"`
	BundleStore    *string `json:"bundle_store"`
	BundleRef      *string `json:"bundle_ref"`
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optHex(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	h := hex.EncodeToString(b)
	return &h
}

// Replay applies restore-time erasure tombstones (架构 §21.4). nil replays every actor_erased row
// of the log; a non-nil list is inserted first (missing tuples only) and then applied, without the
// RD5 hold. It returns the number of tombstones applied.
func Replay(ctx context.Context, pool *pgxpool.Pool, tombstones []Tombstone) (int64, error) {
	if pool == nil || len(tombstones) > 10000 {
		return 0, ErrUsage
	}
	var payload any // NULL = from the log
	if tombstones != nil {
		list := make([]tombstoneJSON, 0, len(tombstones))
		for _, t := range tombstones {
			list = append(list, tombstoneJSON{RequestID: t.RequestID, SelectorDigest: hex.EncodeToString(t.SelectorDigest),
				ActorDigest: optHex(t.ActorDigest), BundleTenant: optString(t.BundleTenant), BundleStore: optString(t.BundleStore),
				BundleRef: optString(t.BundleRef)})
		}
		b, err := json.Marshal(list)
		if err != nil {
			return 0, errFailed
		}
		payload = string(b)
	}
	var n int64
	// claims.replay_actor_erasures: full tuples only (never bare digests); validated again in the definer.
	err := pool.QueryRow(ctx, `SELECT claims.replay_actor_erasures(CASE WHEN $1::jsonb IS NULL THEN NULL ELSE ARRAY(
		SELECT ROW(x.request_id,decode(x.selector_digest,'hex'),decode(x.actor_digest,'hex'),x.bundle_tenant,x.bundle_store,x.bundle_ref)::claims.erasure_tombstone
		FROM jsonb_to_recordset($1::jsonb) AS x(request_id uuid,selector_digest text,actor_digest text,bundle_tenant uuid,bundle_store uuid,bundle_ref uuid)) END)::bigint`,
		payload).Scan(&n)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
