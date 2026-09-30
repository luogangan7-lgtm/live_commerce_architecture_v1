// types.go holds the value types of package retention (Counts, Held, Selector, Policy, Status,
// Tombstone) and the fixed errors. Non-goals: no SQL (ops.go), no River (job.go). Counts and
// Selector are redacted on every formatting path so a log line or %v cannot leak a person's id.

package retention

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Fixed errors. The SQLSTATE mapping is in mapError (ops.go); none carries a driver message,
// DSN, key or selector value.
var (
	ErrUsage    = errors.New("retention_usage")     // 22023
	ErrNotFound = errors.New("retention_not_found") // PT404
	ErrConflict = errors.New("retention_conflict")  // PT409
	ErrBusy     = errors.New("retention_busy")      // 55P03
	errFailed   = errors.New("retention_failed")    // anything else
)

// allowedCounts is the closed set of keys a Counts may carry (D5: run, erasure and replay rows,
// plus busy for a skipped batch and replayed for an idempotent repeat). Keys returned by the
// database that are not listed are dropped, so a future definer field cannot reach a log line.
var allowedCounts = map[string]bool{
	"enforced": true, "links": true, "bundles": true, "intake": true, "operations": true,
	"comment_events": true, "messages": true, "conversations": true, "more": true, "busy": true,
	"lines": true, "replayed": true, "tombstones": true, "inserted": true, "social_deferred": true,
}

// Counts is a set of row counts (booleans as 0/1). Every formatting path prints numbers only.
type Counts map[string]int64

// String renders sorted key=value pairs separated by spaces.
func (c Counts) String() string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k + "=" + strconv.FormatInt(c[k], 10))
	}
	return b.String()
}

// GoString, Format and MarshalJSON keep %#v, %v, %+v and json.Marshal on the same numeric-only text.
func (c Counts) GoString() string             { return c.String() }
func (c Counts) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(c.String())) }
func (c Counts) MarshalJSON() ([]byte, error) { return json.Marshal(map[string]int64(c)) }

// Held reports that erasure was refused by the RD5 hold; nothing was written. The zero value means not held.
type Held struct{ RetryAfter time.Time }

// IsHeld reports whether RetryAfter is set.
func (h Held) IsHeld() bool { return !h.RetryAfter.IsZero() }

// Selector names one actor. Exactly one of ActorKey (a), CommentRef (b) or Bundle (c) is set.
// The caller derives ActorKey/PeerKeys; this package never sees a sender id.
type Selector struct {
	Request               string   // uuid: operator ticket = confirmation code
	Object, Asset         string   // selectors a, b
	ActorKey              string   // a: 64 hex, derived by the caller (meta.ClaimActorKey)
	PeerKeys              []string // a: 0..8 x 64 hex (meta.SocialPeerKey per --app)
	CommentRef            string   // b
	Tenant, Store, Bundle string   // c
}

const redactedSelector = "retention.Selector{redacted}"

// String, GoString, Format and MarshalJSON never print a key, ref or id.
func (Selector) String() string               { return redactedSelector }
func (Selector) GoString() string             { return redactedSelector }
func (Selector) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redactedSelector)) }
func (Selector) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

// Policy is claims.retention_policy without its bookkeeping columns.
type Policy struct {
	Enforced                                     bool
	LinkDays, IntakeDays, ClaimsDays, SocialDays int
}

// Status is claims.retention_status(): the policy, its CAS version and the latest run.
type Status struct {
	Policy
	Version, LastRunUnix int64
	LastRunMore          bool
}

// Tombstone is the full actor_erased tuple replayed after a restore ("" = NULL).
type Tombstone struct {
	RequestID                            string
	SelectorDigest, ActorDigest          []byte
	BundleTenant, BundleStore, BundleRef string
}
