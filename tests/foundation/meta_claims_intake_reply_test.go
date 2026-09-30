// T10c meta claims intake gates MCI07 (first private reply) and MCI08 (privacy), contracts/
// meta-claims-intake-v1.md §6, §7, §8 and §12, written by the independent test_worker from the
// FROZEN contract and the frozen Go/SQL signatures of docs/delivery/units/meta-intake-{core,reply}.md,
// not from the implementation.
//
// Owns: the fake Graph API (an httptest server on 127.0.0.1 that counts POSTs per comment_id and
// records path, query, headers and body) and the in-process dispatcher harness that runs the
// real metareply routes through core.NewDispatcher on a private River queue. The signed-webhook
// half of the MOCK harness (mciEnv) lives in meta_claims_intake_flow_test.go.
//
// Non-goals: no Meta token, no send, no non-loopback network: the Page token is a synthetic
// sentinel that may appear only in the fake Graph's received request (MCI08 scans everything
// else). Graph error codes are unknown until probe U3, so every non-2xx result must be UNKNOWN.
//
// Evidence label: MOCK (REAL_PG + River + fake Graph). LIVE probes are MCI11/MCI12, NOT_RUN.
package foundation_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/claims"
	integration "livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/metareply"
)

// ---------------------------------------------------------------------------------------
// Fake Graph
// ---------------------------------------------------------------------------------------

type mciGraphReq struct {
	method, path, rawQuery, comment, text, bodyToken string
	header                                           http.Header
	body                                             []byte
}

type mciGraph struct {
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []mciGraphReq
	mode     string // ok | 400 | 5xx | 429 | garbled | nomsgid | hang
	received chan struct{}
	seq      int
}

func newMciGraph(t *testing.T) *mciGraph {
	t.Helper()
	g := &mciGraph{mode: "ok", received: make(chan struct{}, 256)}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var in struct {
			Recipient struct {
				CommentID string `json:"comment_id"`
			} `json:"recipient"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(body, &in)
		g.mu.Lock()
		g.seq++
		n := g.seq
		mode := g.mode
		g.reqs = append(g.reqs, mciGraphReq{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, comment: in.Recipient.CommentID,
			text: in.Message.Text, bodyToken: in.AccessToken, header: r.Header.Clone(), body: body})
		g.mu.Unlock()
		select {
		case g.received <- struct{}{}:
		default:
		}
		switch mode {
		case "400":
			http.Error(w, `{"error":{"message":"synthetic","type":"OAuthException","code":190}}`, http.StatusBadRequest)
		case "5xx":
			http.Error(w, `{"error":{"message":"synthetic outage"}}`, http.StatusServiceUnavailable)
		case "429":
			http.Error(w, `{"error":{"message":"synthetic throttle","code":4}}`, http.StatusTooManyRequests)
		case "garbled":
			_, _ = w.Write([]byte("<html>not json"))
		case "nomsgid":
			_, _ = w.Write([]byte(`{"recipient_id":"1"}`))
		case "hang":
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte(fmt.Sprintf(`{"recipient_id":"1","message_id":"m_SYNTH_%d"}`, n)))
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *mciGraph) setMode(m string) { g.mu.Lock(); g.mode = m; g.mu.Unlock() }

func (g *mciGraph) all() []mciGraphReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]mciGraphReq(nil), g.reqs...)
}

// posts counts POSTs for one comment id ("" = every request of any method).
func (g *mciGraph) posts(comment string) int {
	n := 0
	for _, r := range g.all() {
		if (comment == "" || r.comment == comment) && (comment == "" || r.method == http.MethodPost) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------------------
// Reply plan + dispatcher harness
// ---------------------------------------------------------------------------------------

type mciReply struct {
	s                             mciSent
	intake                        mciIntake
	ev                            lcEventRow
	op, provider, asset, bundleID string
}

// planReply posts one comment and applies it. op is "" when no reply was planned.
func (e *mciEnv) planReply(t *testing.T, igLive bool, from, text string) mciReply {
	t.Helper()
	var r mciReply
	if igLive {
		r.s = e.postIG(t, "live_comments", "", from, text, mciAt(3*time.Second), nil)
		r.provider, r.asset = "instagram", e.igAsset
	} else {
		r.s = e.postFB(t, "", from, text, mciAt(3*time.Second), nil)
		r.provider, r.asset = "facebook", e.pageAsset
	}
	e.apply(t)
	return e.refresh(t, r)
}

func (e *mciEnv) refresh(t *testing.T, r mciReply) mciReply {
	t.Helper()
	object := "page"
	if r.provider == "instagram" {
		object = "instagram"
	}
	r.intake = e.mustIntake(t, object, r.asset, r.s.comment)
	if r.intake.AppliedEvent != "" {
		r.ev = lcEvent(t, e.h.f, r.intake.AppliedEvent)
		r.bundleID = r.ev.bundle
	}
	r.op = ""
	_ = e.h.f.owner.QueryRow(context.Background(), `SELECT id::text FROM integration.operations WHERE action='meta.private_reply' AND request->>'comment_ref'=$1`, r.s.comment).Scan(&r.op)
	return r
}

func (e *mciEnv) opCount(t *testing.T, comment string) int64 {
	return miCount(t, e.h.f.owner, `SELECT count(*) FROM integration.operations WHERE action='meta.private_reply' AND request->>'comment_ref'=$1`, comment)
}

func (e *mciEnv) token(t *testing.T, r mciReply) claims.LinkToken {
	t.Helper()
	tok, err := claims.SystemLinkToken(e.link, e.h.f.tenantA, e.h.f.storeA1, r.bundleID, r.op)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type mciDispatcher struct {
	e      *mciEnv
	g      *mciGraph
	queue  string
	routes []integration.DispatchRoute
	client *river.Client[pgx.Tx]
}

func mciDispatchOptions() integration.DispatcherOptions {
	o := t06DispatchOptions()
	o.CallTimeout = 700 * time.Millisecond
	o.MaxGenerations = 2
	return o
}

// newDispatcher builds the real metareply routes against the fake Graph and starts a dispatcher on
// a private River queue. edit may wrap or replace routes; link overrides the reply-link key.
func (e *mciEnv) newDispatcher(t *testing.T, g *mciGraph, link *claims.ReplyLinkKey, edit func(*mciDispatcher, *pgxpool.Pool)) *mciDispatcher {
	t.Helper()
	f := e.h.f
	pool := miPool(t, f, "commerce_worker")
	key := e.link
	if link != nil {
		key = *link
	}
	routes, err := metareply.Routes(pool, key, e.pageKeys, metareply.Config{GraphBaseURL: g.srv.URL, GraphVersion: "v99.0", HTTPClient: g.srv.Client()})
	if err != nil {
		t.Fatalf("metareply.Routes: %v", err)
	}
	d := &mciDispatcher{e: e, g: g, queue: "mci_dispatch_" + t04Tag(), routes: routes}
	if edit != nil {
		edit(d, pool)
	}
	d.client = t06StartDispatcher(t, pool, d.queue, d.routes, mciDispatchOptions())
	return d
}

// run moves the operation's River job (still available after the intake commit) onto this
// dispatcher's private queue; nothing else consumes the default queue in these tests.
func (d *mciDispatcher) run(t *testing.T, op string) {
	t.Helper()
	tag, err := d.e.h.f.owner.Exec(context.Background(), `UPDATE river.river_job SET queue=$1 WHERE state='available' AND id=(SELECT job_id FROM integration.operations WHERE id=$2)`, d.queue, op)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("operation job not available for the private queue: %v", err)
	}
}

func (e *mciEnv) opState(t *testing.T, op string) (state, code, ref, job string) {
	t.Helper()
	err := e.h.f.owner.QueryRow(context.Background(), `SELECT o.state,coalesce(o.result_code,''),coalesce(o.provider_reference,''),coalesce(j.state::text,'') FROM integration.operations o LEFT JOIN river.river_job j ON j.id=o.job_id WHERE o.id=$1`, op).Scan(&state, &code, &ref, &job)
	if err != nil {
		t.Fatalf("read operation: %v", err)
	}
	return
}

// awaitOp polls (bounded) until the operation reaches state and, when jobStates is given, the
// job one of those states.
func (e *mciEnv) awaitOp(t *testing.T, op, state string, budget time.Duration, jobStates ...string) (code, ref string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var s, c, r, j string
	for time.Now().Before(deadline) {
		s, c, r, j = e.opState(t, op)
		if s == state {
			ok := len(jobStates) == 0
			for _, want := range jobStates {
				ok = ok || j == want
			}
			if ok {
				return c, r
			}
		}
		time.Sleep(20 * time.Millisecond) // polls persisted state; no race conclusion depends on the delay
	}
	t.Fatalf("operation persisted %s/%s/job=%s, want %s (job %v)", s, c, j, state, jobStates)
	return
}

// ---------------------------------------------------------------------------------------
// MCI07
// ---------------------------------------------------------------------------------------

// TestMetaClaimsMCI07PlanShapeAndBudget: a comment that creates a bundle on a private_reply source
// plans exactly one operation with the frozen request, semantic key, principal, binding, link
// (generation 1, hash of the re-derived token, 72 h) and River job; nothing else does.
func TestMetaClaimsMCI07PlanShapeAndBudget(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	f := e.h.f
	ctx := context.Background()
	auditBefore := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action='meta.private_reply.planned'`, f.tenantA, f.storeA1)

	for _, igLive := range []bool{false, true} {
		name := map[bool]string{false: "facebook", true: "instagram live"}[igLive]
		t.Run(name, func(t *testing.T) {
			r := e.planReply(t, igLive, "", "A1+2")
			if r.op == "" || r.intake.State != "APPLIED" || r.ev.outcome != "ACCEPTED" {
				t.Fatalf("no reply planned for a new bundle on a private_reply source: %+v", r.intake)
			}
			binding, object := e.pageBinding, "page"
			if igLive {
				binding, object = e.igBinding, "instagram"
			}
			var provider, action, purpose, actorKind, key, external, principal, bindingID, state, source string
			var bindingVersion, currentVersion int64
			var requestSize int
			var argsOK bool
			err := f.owner.QueryRow(ctx, `SELECT o.provider,o.action,o.purpose,o.actor_kind,o.semantic_key,o.external_asset_id,o.principal_id::text,o.binding_id::text,o.state,
				o.binding_version,b.semantic_version,octet_length(o.request::text),
				(SELECT j.args=jsonb_build_object('operation_id',o.id::text,'version',1) AND j.kind='external_operation_v1' AND j.queue='default' FROM river.river_job j WHERE j.id=o.job_id),
				o.request->>'source_id'
				FROM integration.operations o JOIN integration.bindings b ON b.id=o.binding_id WHERE o.id=$1`, r.op).
				Scan(&provider, &action, &purpose, &actorKind, &key, &external, &principal, &bindingID, &state, &bindingVersion, &currentVersion, &requestSize, &argsOK, &source)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(object + "|" + r.asset + "|" + r.s.comment))
			if provider != r.provider || action != "meta.private_reply" || purpose != "service" || actorKind != "MERCHANT" || state != "READY" ||
				key != "mpr:"+hex.EncodeToString(sum[:])[:48] || external != r.asset || principal != e.h.actor || bindingID != binding ||
				bindingVersion != currentVersion || !argsOK {
				t.Fatalf("operation wrong: provider=%s action=%s purpose=%s actor=%s state=%s key=%s external=%s principal=%s binding=%s v=%d/%d args=%t",
					provider, action, purpose, actorKind, state, key, external, principal, bindingID, bindingVersion, currentVersion, argsOK)
			}
			var req map[string]any
			var raw string
			if err := f.owner.QueryRow(ctx, `SELECT request::text FROM integration.operations WHERE id=$1`, r.op).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{"asset_id", "bundle_id", "comment_ref", "deadline_at", "link_generation", "link_key_id", "live_media", "locale", "message_type", "origin", "origin_ref",
				"platform", "policy", "session_id", "source_id", "takeover_generation", "template", "v"}
			var gotKeys []string
			for k := range req {
				gotKeys = append(gotKeys, k)
			}
			sort.Strings(gotKeys)
			if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
				t.Fatalf("request keys %v want %v (frozen request + ruling b origin)", gotKeys, wantKeys)
			}
			srcID := e.srcFB
			if igLive {
				srcID = e.srcIG
			}
			for k, want := range map[string]any{"v": float64(1), "platform": r.provider, "source_id": srcID, "asset_id": r.asset, "comment_ref": r.s.comment, "bundle_id": r.bundleID,
				"session_id": e.session, "link_generation": float64(1), "link_key_id": e.link.ID(), "locale": "zh-TW", "template": "claim-link/v1", "policy": "mpr-policy/v1",
				"message_type": "first_private_reply", "takeover_generation": float64(0), "origin": e.origin, "live_media": igLive} {
				if req[k] != want {
					t.Fatalf("request[%s]=%v want %v", k, req[k], want)
				}
			}
			if requestSize > 2048 || source != srcID {
				t.Fatalf("request size %d (limit 2048) or source mismatch", requestSize)
			}
			token := e.token(t, r)
			for _, secret := range []string{string(token), r.s.text, r.s.name, r.s.from} {
				if strings.Contains(raw, secret) {
					t.Fatalf("the frozen request contains a secret or personal value")
				}
			}
			link := lcLinkState(t, f, r.bundleID)
			want := sha256.Sum256([]byte(token))
			if !link.found || link.generation != 1 || !link.exactly72 || hex.EncodeToString(link.hash) != hex.EncodeToString(want[:]) {
				t.Fatalf("link row wrong: found=%t generation=%d 72h=%t hash-matches=%t", link.found, link.generation, link.exactly72, hex.EncodeToString(link.hash) == hex.EncodeToString(want[:]))
			}
			var principalOK, deadlineOK bool
			if err := f.owner.QueryRow(ctx, `SELECT l.principal_id=$2::uuid,
				(o.request->>'deadline_at')::timestamptz = least(i.occurred_at + interval '7 days' - interval '1 hour', l.expires_at - interval '10 minutes',
				  CASE WHEN i.live_media THEN i.received_at + interval '15 minutes' END)
				FROM claims.links l, claims.meta_intake i, integration.operations o WHERE l.bundle_id=$1 AND i.id=$3 AND o.id=$4`, r.bundleID, e.h.actor, r.intake.ID, r.op).Scan(&principalOK, &deadlineOK); err != nil {
				t.Fatal(err)
			}
			if !principalOK || !deadlineOK {
				t.Fatalf("link principal ok=%t; deadline_at equals least(occurred+7d-1h, expires-10min, live: received+15min): %t", principalOK, deadlineOK)
			}
			// Budget: a second comment of the same actor updates the bundle and plans nothing.
			again := e.planReplyFor(t, igLive, r.s.from, "A1+3")
			if again.op != "" || e.opCount(t, again.s.comment) != 0 || again.intake.State != "APPLIED" || again.ev.bundle != r.bundleID {
				t.Fatalf("a later comment of the same actor planned a second reply or opened another bundle: %+v", again.intake)
			}
			if n := miCount(t, f.owner, `SELECT count(*) FROM integration.operations WHERE action='meta.private_reply' AND request->>'bundle_id'=$1`, r.bundleID); n != 1 {
				t.Fatalf("bundle has %d reply operations, want exactly 1", n)
			}
		})
	}
	if delta := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action='meta.private_reply.planned'`, f.tenantA, f.storeA1) - auditBefore; delta != 2 {
		t.Fatalf("meta.private_reply.planned audit rows: +%d want +2", delta)
	}
}

func (e *mciEnv) planReplyFor(t *testing.T, igLive bool, from, text string) mciReply {
	return e.planReply(t, igLive, from, text)
}

// TestMetaClaimsMCI07ReplySkips: each precondition failure before apply yields an ACCEPTED claim,
// an APPLIED intake, no job/link/operation and exactly one claim_reply_skipped:<code> audit row.
func TestMetaClaimsMCI07ReplySkips(t *testing.T) {
	cases := []struct {
		name string
		opts mciOpts
		edit func(t *testing.T, e *mciEnv)
		code string
	}{
		{"binding_disabled", mciOpts{private: true}, func(t *testing.T, e *mciEnv) {
			mustExec(t, e.h.f.owner, `UPDATE integration.bindings SET enabled=false,semantic_version=semantic_version+1 WHERE id=$1`, e.pageBinding)
		}, "binding_disabled"},
		{"binding_changed", mciOpts{private: true}, func(t *testing.T, e *mciEnv) {
			// The Page-token rows reference the binding's (provider, asset) (0064 FK: an asset change is a new
			// binding), so the re-point is only possible once the token rows are gone.
			mustExec(t, e.h.f.owner, `DELETE FROM integration.meta_page_heads WHERE binding_id=$1`, e.pageBinding)
			mustExec(t, e.h.f.owner, `DELETE FROM integration.meta_page_credentials WHERE binding_id=$1`, e.pageBinding)
			mustExec(t, e.h.f.owner, `UPDATE integration.bindings SET external_asset_id=$2,semantic_version=semantic_version+1 WHERE id=$1`, e.pageBinding, mciDigits(14))
		}, "binding_changed"},
		{"source_off", mciOpts{private: true}, func(t *testing.T, e *mciEnv) {
			mustExec(t, e.h.f.owner, `UPDATE live.claim_sources SET private_reply=false WHERE id=$1`, e.srcFB)
		}, "source_off"},
		{"no_storefront", mciOpts{private: true, noStore: true}, func(t *testing.T, e *mciEnv) {}, "no_storefront"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := mciSetup(t, c.opts)
			f := e.h.f
			s := e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
			c.edit(t, e)
			before := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action=$3 AND principal_id=$4`, f.tenantA, f.storeA1, "claim_reply_skipped:"+c.code, e.h.actor)
			e.apply(t)
			r := e.refresh(t, mciReply{s: s, provider: "facebook", asset: e.pageAsset})
			if r.intake.State != "APPLIED" || r.ev.outcome != "ACCEPTED" || r.bundleID == "" {
				t.Fatalf("the claim itself must commit ACCEPTED/APPLIED on a skip: %+v", r.intake)
			}
			if e.opCount(t, s.comment) != 0 || miCount(t, f.owner, `SELECT count(*) FROM claims.links WHERE bundle_id=$1`, r.bundleID) != 0 {
				t.Fatal("a skipped reply left an operation or a link")
			}
			after := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action=$3 AND principal_id=$4`, f.tenantA, f.storeA1, "claim_reply_skipped:"+c.code, e.h.actor)
			want := int64(1)
			if c.code == "source_off" {
				// §5.3 step 3: §6.2 (and so claim_reply_plannable and its audit) runs only for a source with
				// private_reply=true; a source that has replies off writes no skip audit (ruling u). The audited
				// source_off skip is the race inside §6.2, exercised directly below.
				want = 0
			}
			if after-before != want {
				t.Fatalf("audit claim_reply_skipped:%s +%d want +%d (principal = source principal)", c.code, after-before, want)
			}
			if c.code == "source_off" {
				e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
				tx, id := e.leaseTx(t, pgx.ReadCommitted, true)
				if _, err := claims.IngestMetaIntake(context.Background(), tx, id); err != nil {
					t.Fatal(err)
				}
				var code string
				if err := tx.QueryRow(context.Background(), `SELECT integration.claim_reply_plannable($1::uuid)`, id).Scan(&code); err != nil || code != "source_off" {
					t.Fatalf("claim_reply_plannable with private_reply off: %q %v", code, err)
				}
				if err := tx.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
				if n := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action=$3 AND principal_id=$4`, f.tenantA, f.storeA1, "claim_reply_skipped:source_off", e.h.actor); n-after != 1 {
					t.Fatalf("claim_reply_plannable source_off audit +%d want +1", n-after)
				}
			}
		})
	}
}

// TestMetaClaimsMCI07BindingVersionBumpIsNotASkip (IR-17): a bumped binding version with the
// binding enabled and the same object id plans the reply with the CURRENT version, and the
// dispatcher's own gate then lets it through to SUCCEEDED.
func TestMetaClaimsMCI07BindingVersionBumpIsNotASkip(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	f := e.h.f
	g := newMciGraph(t)
	d := e.newDispatcher(t, g, nil, nil)
	s := e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
	skipsBefore := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action LIKE 'claim_reply_skipped:%' AND principal_id=$3`, f.tenantA, f.storeA1, e.h.actor)
	// SetBindingEnabled off then on bumps semantic_version twice; the object id is unchanged.
	mustExec(t, f.owner, `UPDATE integration.bindings SET semantic_version=semantic_version+2 WHERE id=$1`, e.pageBinding)
	e.apply(t)
	r := e.refresh(t, mciReply{s: s, provider: "facebook", asset: e.pageAsset})
	if r.op == "" {
		t.Fatal("a version bump alone must not skip the reply")
	}
	var opVersion, current int64
	if err := f.owner.QueryRow(context.Background(), `SELECT o.binding_version,b.semantic_version FROM integration.operations o JOIN integration.bindings b ON b.id=o.binding_id WHERE o.id=$1`, r.op).Scan(&opVersion, &current); err != nil || opVersion != current {
		t.Fatalf("operation binding_version=%d, current=%d (must be the CURRENT enabled version): %v", opVersion, current, err)
	}
	if n := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND action LIKE 'claim_reply_skipped:%' AND principal_id=$3`, f.tenantA, f.storeA1, e.h.actor); n != skipsBefore {
		t.Fatal("a skip audit row was written for a mere version bump")
	}
	d.run(t, r.op)
	e.awaitOp(t, r.op, "SUCCEEDED", 30*time.Second, "completed")
	if g.posts(s.comment) != 1 {
		t.Fatalf("expected exactly one POST, got %d", g.posts(s.comment))
	}
}

// TestMetaClaimsMCI07SendAndOutcomes: the real routes against the fake Graph. 2xx with a
// message id is SUCCEEDED; every other result (400, 5xx, 429, garbled, no message id, timeout)
// is UNKNOWN and the comment is POSTed exactly once, ever (Reconcile is query-only, U4).
func TestMetaClaimsMCI07SendAndOutcomes(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	f := e.h.f
	g := newMciGraph(t)
	d := e.newDispatcher(t, g, nil, nil)

	t.Run("2xx sends the link once, token only in the JSON body", func(t *testing.T) {
		for _, ig := range []bool{false, true} {
			r := e.planReply(t, ig, "", "A1")
			g.setMode("ok")
			d.run(t, r.op)
			_, ref := e.awaitOp(t, r.op, "SUCCEEDED", 30*time.Second, "completed")
			var posted []mciGraphReq
			for _, q := range g.all() {
				if q.comment == r.s.comment {
					posted = append(posted, q)
				}
			}
			if len(posted) != 1 || !strings.HasPrefix(ref, "m_SYNTH_") {
				t.Fatalf("posts=%d provider_reference=%q", len(posted), ref)
			}
			q := posted[0]
			wantToken, wantSecret := e.pageToken, "facebook"
			if ig {
				wantToken, wantSecret = e.igToken, "instagram"
			}
			link, err := metareply.RenderClaimLink("zh-TW", e.origin, e.token(t, r))
			if err != nil {
				t.Fatal(err)
			}
			if q.method != http.MethodPost || q.path != "/v99.0/"+r.asset+"/messages" || q.rawQuery != "" {
				t.Fatalf("%s request line: %s %s ?%s", wantSecret, q.method, q.path, q.rawQuery)
			}
			if q.bodyToken != wantToken || q.header.Get("Authorization") != "" {
				t.Fatalf("%s: token must travel only in the JSON body while AuthorizationHeader is off (U6)", wantSecret)
			}
			if q.text != link || !strings.Contains(q.text, e.origin+"/zh-TW/claim#t="+string(e.token(t, r))) {
				t.Fatal("message text is not the rendered claim link with the re-derived token")
			}
			for name, values := range q.header {
				for _, v := range values {
					if strings.Contains(v, wantToken) || strings.Contains(v, string(e.token(t, r))) {
						t.Fatalf("a secret leaked into header %s", name)
					}
				}
			}
			if strings.Contains(q.path+q.rawQuery, wantToken) {
				t.Fatal("token in URL")
			}
		}
	})

	t.Run("locale en renders the /en/ link", func(t *testing.T) {
		var version int64
		if err := f.owner.QueryRow(context.Background(), `SELECT version FROM live.claim_sources WHERE id=$1`, e.srcFB).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if _, err := e.putSource(e.session, "page", e.pageAsset, e.postID, true, "en", true, version); err != nil {
			t.Fatalf("locale update: %v", err)
		}
		r := e.planReply(t, false, "", "A1")
		g.setMode("ok")
		d.run(t, r.op)
		e.awaitOp(t, r.op, "SUCCEEDED", 30*time.Second, "completed")
		for _, q := range g.all() {
			if q.comment == r.s.comment && !strings.Contains(q.text, e.origin+"/en/claim#t=") {
				t.Fatalf("locale en not rendered: %q", q.text)
			}
		}
	})

	for _, mode := range []string{"400", "5xx", "429", "garbled", "nomsgid", "hang"} {
		mode := mode
		t.Run("UNKNOWN then query-only: "+mode, func(t *testing.T) {
			r := e.planReply(t, false, "", "A1")
			g.setMode(mode)
			d.run(t, r.op)
			e.awaitOp(t, r.op, "UNKNOWN", 40*time.Second, "cancelled", "discarded")
			g.setMode("ok") // a wrongful second POST would now succeed and be counted
			if n := g.posts(r.s.comment); n != 1 {
				t.Fatalf("mode %s: %d POSTs for one comment, want exactly 1 (UNKNOWN is never blind-retried)", mode, n)
			}
			for _, q := range g.all() {
				if q.comment == r.s.comment && q.method != http.MethodPost {
					t.Fatalf("Reconcile issued a %s to Graph; it must be query-only and R1 has no proven read (U4)", q.method)
				}
			}
			if n := e.opCount(t, r.s.comment); n != 1 {
				t.Fatal("operation count changed")
			}
		})
	}
}

// TestMetaClaimsMCI07CheckDenials: every Check denial and the credential gate end BLOCKED_POLICY
// with zero HTTP calls, whatever the cause; the code differs only where the dispatcher defines it.
func TestMetaClaimsMCI07CheckDenials(t *testing.T) {
	type denial struct {
		name     string
		igLive   bool
		beforeAp func(t *testing.T, e *mciEnv, r mciReply)                // after staging, before apply
		after    func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher // after plan, before dispatch; may return a custom dispatcher
		wantCode string
		// wantState defaults to BLOCKED_POLICY. The binding gate is the dispatcher's own (§6.3 "re-checked by
		// the dispatcher's own final gate"), whose frozen outcome for a never-dispatched READY op is terminal
		// STALE_BINDING (external-operation-v1 §5), still with zero HTTP calls.
		wantState string
	}
	g := newMciGraph(t)
	denials := []denial{
		{name: "deadline (IG live: received 1 h ago, +15 min already passed)", igLive: true, wantCode: "policy_denied",
			beforeAp: func(t *testing.T, e *mciEnv, r mciReply) {
				mustExec(t, e.h.f.owner, `UPDATE claims.meta_intake SET received_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, r.intake.ID)
			}},
		{name: "source off", wantCode: "policy_denied", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			mustExec(t, e.h.f.owner, `UPDATE live.claim_sources SET private_reply=false WHERE id=$1`, e.srcFB)
			return nil
		}},
		{name: "principal revoked", wantCode: "policy_denied", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			mustExec(t, e.h.f.owner, `DELETE FROM identity.store_grants WHERE principal_id=$1 AND permission='integration:execute'`, e.h.actor)
			return nil
		}},
		{name: "IG live window closed", igLive: true, wantCode: "policy_denied", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			e.h.closeWindow(t, e.session)
			return nil
		}},
		{name: "merchant rotated the link", wantCode: "policy_denied", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			e.h.link(t, e.session, r.bundleID, 1, false)
			return nil
		}},
		{name: "merchant released a bound link", wantCode: "policy_denied", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			c, err := buyerIssue(t, e.h)
			if err != nil {
				t.Fatal(err)
			}
			version := miCount(t, e.h.f.owner, `SELECT version FROM claims.bundles WHERE id=$1`, r.bundleID)
			if _, err := e.h.redeem(c, t04Key("mci07-redeem"), e.token(t, r), version); err != nil {
				t.Fatalf("redeeming the system link before release: %v", err)
			}
			e.h.link(t, e.session, r.bundleID, 1, true)
			return nil
		}},
		{name: "link expiring within 10 minutes", wantCode: "policy_denied", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			mustExec(t, e.h.f.owner, `UPDATE claims.links SET expires_at=clock_timestamp()+interval '5 minutes' WHERE bundle_id=$1`, r.bundleID)
			return nil
		}},
		{name: "link key id changed", wantCode: "policy_denied", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			other, err := claims.NewReplyLinkKey(randomBytes(32))
			if err != nil {
				t.Fatal(err)
			}
			return e.newDispatcher(t, g, &other, nil)
		}},
		{name: "binding changed after plan (dispatcher gate)", wantCode: "binding_changed", wantState: "STALE_BINDING", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			mustExec(t, e.h.f.owner, `UPDATE integration.bindings SET semantic_version=semantic_version+1 WHERE id=$1`, e.pageBinding)
			return nil
		}},
		{name: "no Page token head", wantCode: "credential_unavailable", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			mustExec(t, e.h.f.owner, `DELETE FROM integration.meta_page_heads WHERE binding_id=$1`, e.pageBinding)
			return nil
		}},
		{name: "current Page token lacks the attested scope", wantCode: "credential_unavailable", after: func(t *testing.T, e *mciEnv, r mciReply) *mciDispatcher {
			e.registerTokenVersion(t, "facebook", e.pageBinding, e.pageAsset, 1, []string{"pages_show_list"}, "SENTINEL-EAAG-NOSCOPE-"+t04Tag())
			return nil
		}},
	}
	for _, c := range denials {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := mciSetup(t, mciOpts{private: true})
			base := e.newDispatcher(t, g, nil, nil)
			var s mciSent
			if c.igLive {
				s = e.postIG(t, "live_comments", "", "", "A1", mciAt(3*time.Second), nil)
			} else {
				s = e.postFB(t, "", "", "A1", mciAt(3*time.Second), nil)
			}
			r := mciReply{s: s, provider: "facebook", asset: e.pageAsset}
			if c.igLive {
				r.provider, r.asset = "instagram", e.igAsset
			}
			r = e.refresh(t, r)
			if c.beforeAp != nil {
				c.beforeAp(t, e, r)
			}
			e.apply(t)
			r = e.refresh(t, r)
			if r.op == "" {
				t.Fatal("the reply was not planned; the denial cannot be exercised")
			}
			d := base
			if c.after != nil {
				if custom := c.after(t, e, r); custom != nil {
					d = custom
				}
			}
			before := g.posts("")
			d.run(t, r.op)
			want := "BLOCKED_POLICY"
			if c.wantState != "" {
				want = c.wantState
			}
			code, _ := e.awaitOp(t, r.op, want, 30*time.Second)
			if c.wantCode != "" && code != c.wantCode {
				t.Fatalf("result_code=%q want %q", code, c.wantCode)
			}
			if g.posts("") != before {
				t.Fatalf("a denied reply reached Graph (%d new requests)", g.posts("")-before)
			}
		})
	}

	t.Run("Check infrastructure error is UNKNOWN policy_check_failed, never a denial or a send", func(t *testing.T) {
		e := mciSetup(t, mciOpts{private: true})
		d := e.newDispatcher(t, g, nil, func(d *mciDispatcher, pool *pgxpool.Pool) {
			// Routes were built with a valid worker pool; a cancelled context makes every Check
			// round trip fail with a non-policy (infrastructure) error.
			for i := range d.routes {
				check := d.routes[i].Check
				d.routes[i].Check = func(ctx context.Context, in integration.DispatchRequest) error {
					c, cancel := context.WithCancel(ctx)
					cancel() // a cancelled context makes the SQL round trip fail: infrastructure, not policy
					return check(c, in)
				}
			}
		})
		r := e.planReply(t, false, "", "A1")
		before := g.posts("")
		d.run(t, r.op)
		deadline := time.Now().Add(30 * time.Second)
		for {
			state, code, _, _ := e.opState(t, r.op)
			if state == "UNKNOWN" {
				if code != "policy_check_failed" {
					t.Fatalf("first UNKNOWN code=%q want policy_check_failed", code)
				}
				break
			}
			if state == "BLOCKED_POLICY" || state == "SUCCEEDED" {
				t.Fatalf("infrastructure error became %s/%s", state, code)
			}
			if time.Now().After(deadline) {
				t.Fatal("operation never reached UNKNOWN")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if g.posts("") != before {
			t.Fatal("a Check infrastructure error reached Graph")
		}
	})
}

func (e *mciEnv) registerTokenVersion(t *testing.T, provider, binding, asset string, expected int64, scopes []string, token string) {
	t.Helper()
	f := e.h.f
	if _, err := metareply.RegisterPageToken(context.Background(), e.page.registrar, e.pageKeys, metareply.Registration{TenantID: f.tenantA, StoreID: f.storeA1, PrincipalID: e.h.actor,
		BindingID: binding, Provider: provider, AssetID: asset, ExpectedVersion: expected, Scopes: scopes}, token); err != nil {
		t.Fatalf("register token version %d: %v", expected+1, err)
	}
}

// TestMetaClaimsMCI07SecretVisibility: the Page token exists only inside DispatchWithSecret's
// Secret. Wrappers around the real routes assert that neither Check, Reconcile nor any request or
// context they receive holds it, that DispatchWithSecret receives exactly the registered token, and
// that the dispatcher zeroes the Secret's backing bytes afterwards.
func TestMetaClaimsMCI07SecretVisibility(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	g := newMciGraph(t)
	var mu sync.Mutex
	var checks, reconciles, dispatches int
	var held []integration.Secret
	seesToken := func(ctx context.Context, in integration.DispatchRequest) bool {
		b, _ := json.Marshal(in)
		blob := string(b) + fmt.Sprintf("%v|%+v|%#v", ctx, ctx, in)
		return strings.Contains(blob, e.pageToken) || strings.Contains(blob, e.igToken)
	}
	d := e.newDispatcher(t, g, nil, func(d *mciDispatcher, _ *pgxpool.Pool) {
		for i := range d.routes {
			route := d.routes[i]
			d.routes[i].Check = func(ctx context.Context, in integration.DispatchRequest) error {
				mu.Lock()
				checks++
				mu.Unlock()
				if seesToken(ctx, in) {
					t.Error("Page token visible to Check")
				}
				return route.Check(ctx, in)
			}
			d.routes[i].Reconcile = func(ctx context.Context, in integration.DispatchRequest) (integration.Outcome, error) {
				mu.Lock()
				reconciles++
				mu.Unlock()
				if seesToken(ctx, in) {
					t.Error("Page token visible to Reconcile")
				}
				return route.Reconcile(ctx, in)
			}
			d.routes[i].DispatchWithSecret = func(ctx context.Context, in integration.DispatchRequest, s integration.Secret) (integration.Outcome, error) {
				mu.Lock()
				dispatches++
				held = append(held, s)
				mu.Unlock()
				if seesToken(ctx, in) {
					t.Error("Page token visible in the DispatchRequest")
				}
				if got := string(s.Reveal()); got != e.pageToken && got != e.igToken {
					t.Errorf("DispatchWithSecret received a Secret that is not the registered Page token")
				}
				return route.DispatchWithSecret(ctx, in, s)
			}
		}
	})
	ok := e.planReply(t, false, "", "A1")
	g.setMode("ok")
	d.run(t, ok.op)
	e.awaitOp(t, ok.op, "SUCCEEDED", 30*time.Second, "completed")
	unk := e.planReply(t, false, "", "A1")
	g.setMode("5xx")
	d.run(t, unk.op)
	e.awaitOp(t, unk.op, "UNKNOWN", 40*time.Second, "cancelled", "discarded")

	mu.Lock()
	defer mu.Unlock()
	if checks < 2 || reconciles < 1 || dispatches != 2 || len(held) != 2 {
		t.Fatalf("instrumentation saw checks=%d reconciles=%d dispatches=%d (want >=2, >=1, 2)", checks, reconciles, dispatches)
	}
	for i, s := range held {
		for _, b := range s.Reveal() {
			if b != 0 {
				t.Fatalf("Secret #%d backing bytes were not zeroed after DispatchWithSecret returned", i)
			}
		}
	}
	if g.posts("") != 2 {
		t.Fatalf("Graph saw %d POSTs, want 2 (one per comment)", g.posts(""))
	}
}

// TestMetaClaimsMCI07ChildKillAfterSend: a real claims-worker process sends, the fake Graph has
// recorded the POST, the process is SIGKILLed before it learns the answer. Recovery must go
// through Reconcile (UNKNOWN), never a second Dispatch: exactly one POST ever.
func TestMetaClaimsMCI07ChildKillAfterSend(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	f := e.h.f
	g := newMciGraph(t)
	g.setMode("hang")
	r := e.planReply(t, false, "", "A1")
	if r.op == "" {
		t.Fatal("no reply planned")
	}
	binary := mrBuild(t, "../../cmd/claims-worker", "claims-worker")
	child := mrLaunch(t, binary, "claims-worker", e.workerEnv(t, g.srv.URL))
	select {
	case <-g.received:
	case err := <-child.done:
		child.exited = true
		t.Fatalf("claims-worker exited before sending: %v log=%s", err, child.logPath)
	case <-time.After(45 * time.Second):
		t.Fatalf("claims-worker never sent the reply; log=%s", child.logPath)
	}
	mrStop(t, child, syscall.SIGKILL, false)
	g.setMode("ok") // any second POST would now succeed and be counted
	queue := "mci_recover_" + t04Tag()
	// Only age this owned fixture (as the T06 crash gate does): River must rescue the job itself.
	mustExec(t, f.owner, `UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, r.op)
	mustExec(t, f.owner, `UPDATE river.river_job SET queue=$1,attempted_at=clock_timestamp()-interval '2 hours' WHERE state='running' AND id=(SELECT job_id FROM integration.operations WHERE id=$2)`, queue, r.op)
	pool := miPool(t, f, "commerce_worker")
	routes, err := metareply.Routes(pool, e.link, e.pageKeys, metareply.Config{GraphBaseURL: g.srv.URL, GraphVersion: "v99.0", HTTPClient: g.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t06StartDispatcher(t, pool, queue, routes, mciDispatchOptions())
	e.awaitOp(t, r.op, "UNKNOWN", 60*time.Second, "cancelled", "discarded")
	if n := g.posts(r.s.comment); n != 1 || g.posts("") != 1 {
		t.Fatalf("POSTs after kill+recovery: comment=%d total=%d, want exactly 1", n, g.posts(""))
	}
}

// TestMetaClaimsMCI07DispatcherRouteShape: the §6.4 route contract. A route sets either Dispatch
// or the LoadSecret/DispatchWithSecret pair; the stricter lease inequality applies only to routes
// that set LoadSecret.
func TestMetaClaimsMCI07DispatcherRouteShape(t *testing.T) {
	f := fixture(t)
	pool := miPool(t, f, "commerce_worker")
	ctx := context.Background()
	base := t06Route()
	load := func(context.Context, pgx.Tx, integration.SecretClaim) (integration.Secret, error) {
		return integration.NewSecret([]byte("synthetic")), nil
	}
	send := func(context.Context, integration.DispatchRequest, integration.Secret) (integration.Outcome, error) {
		return integration.Outcome{State: "SUCCEEDED", Code: "mock", ProviderReference: "x"}, nil
	}
	pair := base
	pair.Dispatch, pair.LoadSecret, pair.DispatchWithSecret = nil, load, send
	loadOnly := base
	loadOnly.LoadSecret = load
	sendOnly := base
	sendOnly.Dispatch, sendOnly.DispatchWithSecret = nil, send
	all := pair
	all.Dispatch = base.Dispatch
	none := base
	none.Dispatch = nil
	opts := t06DispatchOptions()
	for name, c := range map[string]struct {
		route integration.DispatchRoute
		ok    bool
	}{"plain Dispatch": {base, true}, "LoadSecret+DispatchWithSecret": {pair, true}, "Dispatch and LoadSecret": {loadOnly, false},
		"DispatchWithSecret without LoadSecret": {sendOnly, false}, "Dispatch plus the pair": {all, false}, "no send function": {none, false}} {
		if _, err := integration.NewDispatcher(ctx, pool, []integration.DispatchRoute{c.route}, opts); (err == nil) != c.ok {
			t.Fatalf("%s: NewDispatcher err=%v want ok=%t", name, err, c.ok)
		}
	}
	// Lease inequality: CallTimeout+2*DB+1s < lease is the legacy rule; routes with LoadSecret need
	// CallTimeout+3*DB+1s < lease. 5s+4s+1s=10s < 12s but 5s+6s+1s=12s is not < 12s.
	tight := opts
	tight.LeaseSeconds, tight.CallTimeout, tight.DBTimeout = 12, 5*time.Second, 2*time.Second
	if _, err := integration.NewDispatcher(ctx, pool, []integration.DispatchRoute{base}, tight); err != nil {
		t.Fatalf("existing routes must keep the legacy inequality: %v", err)
	}
	if _, err := integration.NewDispatcher(ctx, pool, []integration.DispatchRoute{pair}, tight); err == nil {
		t.Fatal("a LoadSecret route accepted a lease that violates CallTimeout+3*DBTimeout+1s < lease")
	}
	roomy := opts
	roomy.LeaseSeconds, roomy.CallTimeout, roomy.DBTimeout = 30, 5*time.Second, 2*time.Second
	if _, err := integration.NewDispatcher(ctx, pool, []integration.DispatchRoute{pair}, roomy); err != nil {
		t.Fatalf("a LoadSecret route with a roomy lease was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------------------
// MCI08
// ---------------------------------------------------------------------------------------

func mciNeedles(values map[string]string) map[string][]string {
	out := map[string][]string{}
	for name, v := range values {
		out[name] = []string{v, hex.EncodeToString([]byte(v))}
	}
	return out
}

// TestMetaClaimsMCI08PrivacyScansAndCustody: after a full run (FB + IG, NO_MATCH, unknown keyword,
// MATCH with private reply through a real claims-worker process), no comment text, unresolved
// keyword, display name, username, raw sender id, link token or Page token exists in any text,
// bytea or jsonb column of claims, live, integration, ops, river, river_meta or meta_inbox, nor in
// the worker's log; the Page-token ciphertext binds every AAD field.
func TestMetaClaimsMCI08PrivacyScansAndCustody(t *testing.T) {
	e := mciSetup(t, mciOpts{private: true})
	f := e.h.f
	ctx := context.Background()
	g := newMciGraph(t)
	textSentinel := "SENTINELTXT" + strings.ToUpper(t04Tag()) + " please hold two for me"
	keywordSentinel := "Q" + strings.ToUpper(t04Tag()[:9])
	fbFrom, igFrom, plainFrom := mciDigits(16), mciDigits(16), mciDigits(16)

	nm := e.postFB(t, "", plainFrom, textSentinel, mciAt(3*time.Second), nil)
	uk := e.postFB(t, "", plainFrom, keywordSentinel, mciAt(3*time.Second), nil)
	fb := e.planReply(t, false, fbFrom, "A1+2")
	ig := e.planReply(t, true, igFrom, "A1")
	e.apply(t)
	if fb.op == "" || ig.op == "" {
		t.Fatal("test bug: both replies must be planned")
	}
	// Send through a real claims-worker process so its log can be scanned too.
	binary := mrBuild(t, "../../cmd/claims-worker", "claims-worker")
	child := mrLaunch(t, binary, "claims-worker", e.workerEnv(t, g.srv.URL))
	e.awaitOp(t, fb.op, "SUCCEEDED", 60*time.Second, "completed")
	e.awaitOp(t, ig.op, "SUCCEEDED", 60*time.Second, "completed")
	fbTok, igTok := string(e.token(t, fb)), string(e.token(t, ig))

	needles := mciNeedles(map[string]string{
		"comment text": textSentinel, "unresolved keyword": keywordSentinel, "FB display name": nm.name, "FB sender name of the reply": fb.s.name, "IG username": ig.s.name,
		"raw FB sender id": fbFrom, "raw IG sender id": igFrom, "raw sender id (no-match)": plainFrom,
		"FB link token": fbTok, "IG link token": igTok, "FB Page token": e.pageToken, "IG Page token": e.igToken,
	})
	schemas := []string{"claims", "live", "integration", "ops", "river", "river_meta", "meta_inbox"}
	for name, forms := range needles {
		for _, needle := range forms {
			if hits := lcFind(t, f, needle, nil, schemas...); len(hits) != 0 {
				t.Errorf("%s persisted in %v", name, hits)
			}
		}
	}
	// The Page token is only ever seen by the fake Graph.
	sawToken := 0
	for _, q := range g.all() {
		if q.bodyToken == e.pageToken || q.bodyToken == e.igToken {
			sawToken++
		}
	}
	if sawToken != 2 {
		t.Fatalf("the fake Graph received the Page token in %d requests, want 2", sawToken)
	}
	// Worker log: nothing secret, and the one startup line listing the registered routes (IR-13).
	mrStop(t, child, syscall.SIGKILL, false)
	logBytes, err := os.ReadFile(child.logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	secrets := map[string]string{"link key": e.linkRaw2b64(), "page keyring key": b64(e.pageKeyRaw), "link key hex": hex.EncodeToString(e.linkRaw), "intake login password": passwordOf(t, e.intakeLogin)}
	for name, v := range secrets {
		if v != "" && strings.Contains(logText, v) {
			t.Errorf("claims-worker log contains the %s", name)
		}
	}
	for name, forms := range needles {
		for _, needle := range forms {
			if strings.Contains(logText, needle) {
				t.Errorf("claims-worker log contains the %s", name)
			}
		}
	}
	if !strings.Contains(logText, "meta.private_reply") || !strings.Contains(logText, "facebook") || !strings.Contains(logText, "instagram") {
		t.Errorf("claims-worker startup did not log the registered routes (IR-13); log=%s", child.logPath)
	}
	_ = nm
	_ = uk

	// Custody: the stored Page-token ciphertext is bound to every AAD field (§7).
	var tenant, store, binding, keyID string
	var version int64
	var nonce, ciphertext []byte
	if err := f.owner.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,binding_id::text,version,key_id,nonce,ciphertext FROM integration.meta_page_credentials WHERE binding_id=$1 ORDER BY version DESC LIMIT 1`, e.pageBinding).
		Scan(&tenant, &store, &binding, &version, &keyID, &nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	scope := metareply.PageTokenScope{TenantID: tenant, StoreID: store, BindingID: binding, Provider: "facebook", AssetID: e.pageAsset, Version: version}
	secret, err := e.pageKeys.Open(scope, keyID, nonce, ciphertext)
	if err != nil || string(secret.Reveal()) != e.pageToken {
		t.Fatalf("the registered ciphertext does not open under its own AAD: %v", err)
	}
	tamper := map[string]func() (metareply.PageTokenScope, string, []byte, []byte){
		"tenant": func() (metareply.PageTokenScope, string, []byte, []byte) {
			s := scope
			s.TenantID = randomUUID()
			return s, keyID, nonce, ciphertext
		},
		"store": func() (metareply.PageTokenScope, string, []byte, []byte) {
			s := scope
			s.StoreID = randomUUID()
			return s, keyID, nonce, ciphertext
		},
		"binding": func() (metareply.PageTokenScope, string, []byte, []byte) {
			s := scope
			s.BindingID = randomUUID()
			return s, keyID, nonce, ciphertext
		},
		"provider": func() (metareply.PageTokenScope, string, []byte, []byte) {
			s := scope
			s.Provider = "instagram"
			return s, keyID, nonce, ciphertext
		},
		"asset": func() (metareply.PageTokenScope, string, []byte, []byte) {
			s := scope
			s.AssetID = mciDigits(14)
			return s, keyID, nonce, ciphertext
		},
		"version": func() (metareply.PageTokenScope, string, []byte, []byte) {
			s := scope
			s.Version++
			return s, keyID, nonce, ciphertext
		},
		"key id": func() (metareply.PageTokenScope, string, []byte, []byte) { return scope, "pt_key_2", nonce, ciphertext },
		"nonce flip": func() (metareply.PageTokenScope, string, []byte, []byte) {
			n := append([]byte(nil), nonce...)
			n[0] ^= 1
			return scope, keyID, n, ciphertext
		},
		"ciphertext flip": func() (metareply.PageTokenScope, string, []byte, []byte) {
			c := append([]byte(nil), ciphertext...)
			c[len(c)-1] ^= 1
			return scope, keyID, nonce, c
		},
		"truncated": func() (metareply.PageTokenScope, string, []byte, []byte) {
			return scope, keyID, nonce, ciphertext[:len(ciphertext)-1]
		},
	}
	for name, mk := range tamper {
		s, k, n, c := mk()
		if got, err := e.pageKeys.Open(s, k, n, c); err == nil || strings.Contains(string(got.Reveal()), "SENTINEL") {
			t.Errorf("Open accepted a ciphertext with a swapped/tampered %s", name)
		}
	}
	var plain bool
	if err := f.owner.QueryRow(ctx, `SELECT position($1::bytea IN ciphertext)>0 FROM integration.meta_page_credentials WHERE binding_id=$2 ORDER BY version DESC LIMIT 1`, []byte(e.pageToken), e.pageBinding).Scan(&plain); err != nil || plain {
		t.Fatalf("Page token stored in clear: %v", err)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func (e *mciEnv) linkRaw2b64() string { return b64(e.linkRaw) }

func passwordOf(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	p, _ := u.User.Password()
	return p
}

// TestMetaClaimsMCI08ProcessesCannotLoadReplySecrets: the meta-worker and API processes never read
// the reply-link key or the Page-token keyring (they belong to cmd/claims-worker and the registrar
// only), so a compromise of either cannot forge a link or use a Page token.
func TestMetaClaimsMCI08ProcessesCannotLoadReplySecrets(t *testing.T) {
	forbidden := []string{"COMMERCE_CLAIMS_REPLY_LINK_KEY", "COMMERCE_META_PAGE_TOKEN_"}
	for _, dir := range []string{"../../cmd/api", "../../cmd/meta-worker"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources under %s: %v", dir, err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range forbidden {
				if strings.Contains(string(body), bad) {
					t.Errorf("%s references %q", file, bad)
				}
			}
		}
	}
}
