// main.go parses the retention-admin command line (flags, strict stdin JSON, tombstone file),
// derives keys for a Meta sender and maps retention errors to the exit codes documented in
// doc.go. Facts it relies on: internal/retention (one definer per call), meta.ClaimActorKey /
// meta.SocialPeerKey (key derivation), platform.OpenRetention*Pool (admission of the login).
// Non-goals: no SQL, no logging, no retry; a failed call is reported once with a fixed code.

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/platform"
	"livecommerce/internal/retention"
)

const (
	envOperatorDSN = "COMMERCE_RETENTION_OPERATOR_DATABASE_URL"
	envJobDSN      = "COMMERCE_RETENTION_JOB_DATABASE_URL"

	maxStdin      = 4 << 10 // D8: stdin <= 4 KiB
	maxTombstones = 10000   // D7
	maxFile       = 4 << 20 // D7: 4 MiB cap
	maxDSN        = 8192
	defaultLimit  = 500
)

// Fixed stderr codes and exit codes (contract §5). Nothing else ever reaches stderr.
const (
	exitOK, exitOther, exitUsage, exitHeld, exitNotFound, exitConflict = 0, 1, 2, 3, 4, 5

	codeUsage    = "retention_admin_usage"
	codeConfig   = "retention_admin_config"
	codeDatabase = "retention_admin_database"
	codeFailed   = "retention_admin_failed"
	codeHeld     = "retention_admin_held"
	codeNotFound = "retention_admin_not_found"
	codeConflict = "retention_admin_conflict"
	codeBusy     = "retention_admin_busy"
	codeOutput   = "retention_admin_output"
)

var (
	idPattern      = regexp.MustCompile(`^[0-9]{1,32}$`)
	commentPattern = regexp.MustCompile(`^[0-9_]{1,80}$`)
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hex64Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Seams replaced by unit tests; production values are the real pools and retention calls.
var (
	openOperator = platform.OpenRetentionOperatorPool
	openJob      = platform.OpenRetentionJobPool
	doStatus     = retention.GetStatus
	doSetPolicy  = retention.SetPolicy
	doRunOnce    = retention.RunOnce
	doErase      = retention.Erase
	doReplay     = retention.Replay
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	cancel()
	os.Exit(code)
}

// run executes one subcommand and returns the process exit code. All usage validation happens
// before any connection opens.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if ctx == nil || stdin == nil || stdout == nil || stderr == nil || getenv == nil || len(args) < 1 {
		return fail(stderr, exitUsage, codeUsage)
	}
	sub, rest := args[0], args[1:]
	var plan func(context.Context, *pgxpool.Pool, io.Writer) (int, string)
	var err error
	switch sub {
	case "status":
		plan, err = planStatus(rest)
	case "policy-set":
		plan, err = planPolicySet(rest)
	case "run":
		plan, err = planRun(rest)
	case "erase":
		plan, err = planErase(rest, stdin, getenv)
	case "replay":
		plan, err = planReplay(rest)
	default:
		err = errUsage
	}
	if err != nil {
		return failErr(stderr, err)
	}
	operator, job := getenv(envOperatorDSN), getenv(envJobDSN)
	if len(operator) > maxDSN || len(job) > maxDSN {
		return fail(stderr, exitOther, codeConfig)
	}
	var pool *pgxpool.Pool
	switch {
	case strings.TrimSpace(operator) != "":
		pool, err = openOperator(ctx, operator)
	case sub == "status" && strings.TrimSpace(job) != "":
		// status is the only subcommand the job login may run (deploy smoke; contract §10(5)).
		pool, err = openJob(ctx, job)
	case sub != "status" && strings.TrimSpace(job) != "":
		return fail(stderr, exitUsage, codeUsage) // the job DSN is refused for every other subcommand
	default:
		return fail(stderr, exitOther, codeConfig)
	}
	if err != nil {
		return fail(stderr, exitOther, codeDatabase) // Open* errors are already fixed text; never echo them
	}
	if pool != nil {
		defer pool.Close()
	}
	code, msg := plan(ctx, pool, stdout)
	if code != exitOK {
		return fail(stderr, code, msg)
	}
	return exitOK
}

var (
	errUsage  = errors.New(codeUsage)
	errConfig = errors.New(codeConfig)
)

func fail(stderr io.Writer, code int, msg string) int {
	_, _ = io.WriteString(stderr, msg+"\n")
	return code
}

func failErr(stderr io.Writer, err error) int {
	if errors.Is(err, errConfig) {
		return fail(stderr, exitOther, codeConfig)
	}
	return fail(stderr, exitUsage, codeUsage)
}

// exitFor maps a retention error to (exit code, fixed stderr code).
func exitFor(err error) (int, string) {
	switch {
	case err == nil:
		return exitOK, ""
	case errors.Is(err, retention.ErrUsage):
		return exitUsage, codeUsage
	case errors.Is(err, retention.ErrNotFound):
		return exitNotFound, codeNotFound
	case errors.Is(err, retention.ErrConflict):
		return exitConflict, codeConflict
	case errors.Is(err, retention.ErrBusy):
		// busy: another run or erasure holds the retention advisory key; the operator re-runs (idempotent by request id).
		return exitConflict, codeBusy
	default:
		return exitOther, codeFailed
	}
}

// emit writes lines to stdout; a write failure is a fixed error.
func emit(w io.Writer, lines ...string) (int, string) {
	if _, err := io.WriteString(w, strings.Join(lines, "\n")+"\n"); err != nil {
		return exitOther, codeOutput
	}
	return exitOK, ""
}

// countLines renders numeric-only sorted key=value lines.
func countLines(c retention.Counts) []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+strconv.FormatInt(c[k], 10))
	}
	return out
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag errors echo the offending value; usage errors are fixed instead
	return fs
}

func boolInt(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func planStatus(args []string) (func(context.Context, *pgxpool.Pool, io.Writer) (int, string), error) {
	fs := newFlags("status")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return nil, errUsage
	}
	return func(ctx context.Context, pool *pgxpool.Pool, out io.Writer) (int, string) {
		s, err := doStatus(ctx, pool)
		if err != nil {
			return exitFor(err)
		}
		return emit(out,
			"enforced="+boolInt(s.Enforced), "version="+strconv.FormatInt(s.Version, 10),
			"link_days="+strconv.Itoa(s.LinkDays), "intake_days="+strconv.Itoa(s.IntakeDays),
			"claims_days="+strconv.Itoa(s.ClaimsDays), "social_days="+strconv.Itoa(s.SocialDays),
			"last_run_unix="+strconv.FormatInt(s.LastRunUnix, 10), "last_run_more="+boolInt(s.LastRunMore))
	}, nil
}

func planPolicySet(args []string) (func(context.Context, *pgxpool.Pool, io.Writer) (int, string), error) {
	fs := newFlags("policy-set")
	var version int64
	var enforced bool
	var p retention.Policy
	fs.Int64Var(&version, "expected-version", 0, "")
	fs.BoolVar(&enforced, "enforced", false, "")
	fs.IntVar(&p.LinkDays, "link-days", 0, "")
	fs.IntVar(&p.IntakeDays, "intake-days", 0, "")
	fs.IntVar(&p.ClaimsDays, "claims-days", 0, "")
	fs.IntVar(&p.SocialDays, "social-days", 0, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return nil, errUsage
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	for _, name := range []string{"expected-version", "enforced", "link-days", "intake-days", "claims-days", "social-days"} {
		if !seen[name] {
			return nil, errUsage // every value is explicit: a policy change is never a default
		}
	}
	p.Enforced = enforced
	if version <= 0 || p.LinkDays < 1 || p.LinkDays > 365 || p.IntakeDays < 8 || p.IntakeDays > 3650 ||
		p.ClaimsDays < 8 || p.ClaimsDays > 3650 || p.SocialDays < 8 || p.SocialDays > 3650 || p.SocialDays > p.IntakeDays {
		return nil, errUsage
	}
	return func(ctx context.Context, pool *pgxpool.Pool, out io.Writer) (int, string) {
		v, err := doSetPolicy(ctx, pool, version, p)
		if err != nil {
			return exitFor(err)
		}
		return emit(out, "version="+strconv.FormatInt(v, 10))
	}, nil
}

func planRun(args []string) (func(context.Context, *pgxpool.Pool, io.Writer) (int, string), error) {
	fs := newFlags("run")
	limit := defaultLimit
	fs.IntVar(&limit, "limit", defaultLimit, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || limit < 1 || limit > 1000 {
		return nil, errUsage
	}
	return func(ctx context.Context, pool *pgxpool.Pool, out io.Writer) (int, string) {
		c, err := doRunOnce(ctx, pool, limit)
		if err != nil {
			return exitFor(err)
		}
		return emit(out, countLines(c)...)
	}, nil
}

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string     { return "" }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseStdinSelector reads at most maxStdin bytes and decodes one strict JSON object. Empty
// (whitespace-only) input returns ("", "", nil). Unknown fields, both fields, a null value or
// trailing data are usage errors.
func parseStdinSelector(stdin io.Reader) (sender, comment string, err error) {
	raw, rerr := io.ReadAll(io.LimitReader(stdin, maxStdin+1))
	if rerr != nil || len(raw) > maxStdin {
		return "", "", errUsage
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", "", nil
	}
	var in struct {
		SenderID   *string `json:"sender_id"`
		CommentRef *string `json:"comment_ref"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil {
		return "", "", errUsage
	}
	if _, more := dec.Token(); more != io.EOF { // trailing data of any kind
		return "", "", errUsage
	}
	if (in.SenderID == nil) == (in.CommentRef == nil) {
		return "", "", errUsage
	}
	if in.SenderID != nil {
		if !idPattern.MatchString(*in.SenderID) {
			return "", "", errUsage
		}
		return *in.SenderID, "", nil
	}
	if !commentPattern.MatchString(*in.CommentRef) {
		return "", "", errUsage
	}
	return "", *in.CommentRef, nil
}

// buildSelector validates the erase flags and stdin and derives the keys of selector (a). The
// sender id exists only in this function's locals; the Selector carries derived keys.
func buildSelector(args []string, stdin io.Reader, getenv func(string) string) (retention.Selector, error) {
	fs := newFlags("erase")
	var request, object, asset, tenant, store, bundle string
	var apps multiFlag
	fs.StringVar(&request, "request", "", "")
	fs.StringVar(&object, "object", "", "")
	fs.StringVar(&asset, "asset", "", "")
	fs.Var(&apps, "app", "")
	fs.StringVar(&tenant, "tenant", "", "")
	fs.StringVar(&store, "store", "", "")
	fs.StringVar(&bundle, "bundle", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || !uuidPattern.MatchString(request) {
		return retention.Selector{}, errUsage
	}
	sender, comment, err := parseStdinSelector(stdin)
	if err != nil {
		return retention.Selector{}, err
	}
	if bundle != "" {
		if !uuidPattern.MatchString(bundle) || !uuidPattern.MatchString(tenant) || !uuidPattern.MatchString(store) ||
			object != "" || asset != "" || len(apps) > 0 || sender != "" || comment != "" {
			return retention.Selector{}, errUsage
		}
		return retention.Selector{Request: request, Tenant: tenant, Store: store, Bundle: bundle}, nil
	}
	if tenant != "" || store != "" || (sender == "") == (comment == "") ||
		(object != "page" && object != "instagram") || !idPattern.MatchString(asset) || len(apps) > 8 {
		return retention.Selector{}, errUsage
	}
	if comment != "" {
		if len(apps) > 0 {
			return retention.Selector{}, errUsage
		}
		return retention.Selector{Request: request, Object: object, Asset: asset, CommentRef: comment}, nil
	}
	for _, a := range apps {
		if !idPattern.MatchString(a) {
			return retention.Selector{}, errUsage
		}
	}
	// COMMERCE_CLAIMS_ACTOR_KEY: K_actor, read only here (clause 4); missing or malformed is a config error, never a guess.
	k, ok, kerr := meta.LoadClaimsActorKey(getenv)
	if kerr != nil || !ok {
		return retention.Selector{}, errConfig
	}
	sel := retention.Selector{Request: request, Object: object, Asset: asset, ActorKey: meta.ClaimActorKey(k, object, asset, sender)}
	for _, a := range apps {
		sel.PeerKeys = append(sel.PeerKeys, meta.SocialPeerKey(a, object, asset, sender))
	}
	return sel, nil
}

func planErase(args []string, stdin io.Reader, getenv func(string) string) (func(context.Context, *pgxpool.Pool, io.Writer) (int, string), error) {
	sel, err := buildSelector(args, stdin, getenv)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, pool *pgxpool.Pool, out io.Writer) (int, string) {
		c, held, err := doErase(ctx, pool, sel)
		if err != nil {
			return exitFor(err)
		}
		if held.IsHeld() {
			// held: the RD5 hold refused it and nothing was written; the operator retries after retry_after.
			if code, msg := emit(out, "request="+sel.Request, "retry_after="+held.RetryAfter.UTC().Format(time.RFC3339)); code != exitOK {
				return code, msg
			}
			return exitHeld, codeHeld
		}
		return emit(out, append([]string{"request=" + sel.Request}, countLines(c)...)...)
	}, nil
}

type tombstoneEntry struct {
	RequestID      string  `json:"request_id"`
	SelectorDigest string  `json:"selector_digest"`
	ActorDigest    *string `json:"actor_digest"`
	BundleTenant   *string `json:"bundle_tenant"`
	BundleStore    *string `json:"bundle_store"`
	BundleRef      *string `json:"bundle_ref"`
}

// parseTombstones decodes the D7 file: a JSON array (<= 10 000) of objects with exactly the six
// keys, no unknown key, no trailing data. Values are validated like the actor_erased CHECKs.
func parseTombstones(raw []byte) ([]retention.Tombstone, error) {
	var items []map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&items) != nil || items == nil {
		return nil, errUsage
	}
	if _, more := dec.Token(); more != io.EOF || len(items) > maxTombstones {
		return nil, errUsage
	}
	out := make([]retention.Tombstone, 0, len(items))
	for _, it := range items {
		if len(it) != 6 {
			return nil, errUsage
		}
		strict, err := json.Marshal(it)
		if err != nil {
			return nil, errUsage
		}
		var e tombstoneEntry
		d := json.NewDecoder(bytes.NewReader(strict))
		d.DisallowUnknownFields()
		if d.Decode(&e) != nil {
			return nil, errUsage
		}
		for _, k := range []string{"request_id", "selector_digest", "actor_digest", "bundle_tenant", "bundle_store", "bundle_ref"} {
			if _, ok := it[k]; !ok {
				return nil, errUsage
			}
		}
		t, err := e.toTombstone()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (e tombstoneEntry) toTombstone() (retention.Tombstone, error) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if !uuidPattern.MatchString(e.RequestID) || !hex64Pattern.MatchString(e.SelectorDigest) {
		return retention.Tombstone{}, errUsage
	}
	t := retention.Tombstone{RequestID: e.RequestID, BundleTenant: str(e.BundleTenant), BundleStore: str(e.BundleStore), BundleRef: str(e.BundleRef)}
	t.SelectorDigest, _ = hex.DecodeString(e.SelectorDigest)
	if e.ActorDigest != nil {
		if !hex64Pattern.MatchString(*e.ActorDigest) {
			return retention.Tombstone{}, errUsage
		}
		t.ActorDigest, _ = hex.DecodeString(*e.ActorDigest)
	}
	// Exactly one of actor_digest / bundle_ref; the bundle triple is all or none (same as the log CHECKs).
	if (e.ActorDigest == nil) == (e.BundleRef == nil) || (e.BundleRef == nil) != (e.BundleTenant == nil) || (e.BundleRef == nil) != (e.BundleStore == nil) {
		return retention.Tombstone{}, errUsage
	}
	for _, id := range []string{t.BundleTenant, t.BundleStore, t.BundleRef} {
		if id != "" && !uuidPattern.MatchString(id) {
			return retention.Tombstone{}, errUsage
		}
	}
	return t, nil
}

func planReplay(args []string) (func(context.Context, *pgxpool.Pool, io.Writer) (int, string), error) {
	fs := newFlags("replay")
	var file string
	fs.StringVar(&file, "tombstones-file", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return nil, errUsage
	}
	var list []retention.Tombstone // nil = replay the log
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return nil, errUsage
		}
		raw, rerr := io.ReadAll(io.LimitReader(f, maxFile+1))
		_ = f.Close()
		if rerr != nil || len(raw) > maxFile {
			return nil, errUsage
		}
		if list, err = parseTombstones(raw); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context, pool *pgxpool.Pool, out io.Writer) (int, string) {
		n, err := doReplay(ctx, pool, list)
		if err != nil {
			return exitFor(err)
		}
		return emit(out, "tombstones="+strconv.FormatInt(n, 10))
	}, nil
}
