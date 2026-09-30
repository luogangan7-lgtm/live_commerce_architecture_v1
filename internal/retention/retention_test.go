package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Unit tests of the pure logic of package retention (no database): redaction, error mapping,
// selector shapes, result parsing and the job loop. The SQL is exercised by the CRP gates.

const (
	sentinelKey     = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"
	sentinelRef     = "918273645_5544332211"
	sentinelRequest = "0f0e0d0c-0b0a-4908-8706-050403020100"
)

func TestCountsFormattingPrintsNumbersOnly(t *testing.T) {
	c := Counts{"bundles": 3, "links": 12, "more": 1}
	want := "bundles=3 links=12 more=1"
	for _, got := range []string{c.String(), c.GoString(), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprintf("%s", c)} {
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	b, err := json.Marshal(c)
	if err != nil || string(b) != `{"bundles":3,"links":12,"more":1}` {
		t.Fatalf("json %s %v", b, err)
	}
	b, err = json.Marshal(struct{ C Counts }{c})
	if err != nil || !strings.Contains(string(b), `"bundles":3`) {
		t.Fatalf("embedded json %s %v", b, err)
	}
}

func TestSelectorNeverPrintsAnIdentifier(t *testing.T) {
	s := Selector{Request: sentinelRequest, Object: "page", Asset: "55", ActorKey: sentinelKey, PeerKeys: []string{sentinelKey},
		CommentRef: sentinelRef, Tenant: sentinelRequest, Store: sentinelRequest, Bundle: sentinelRequest}
	b, _ := json.Marshal(s)
	outs := []string{s.String(), s.GoString(), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%v", &s), string(b),
		fmt.Sprintf("%v", []Selector{s}), fmt.Sprintf("%+v", map[string]Selector{"k": s})}
	for _, out := range outs {
		for _, secret := range []string{sentinelKey, sentinelRef, sentinelRequest, "918273645"} {
			if strings.Contains(out, secret) {
				t.Fatalf("selector formatting leaked %q in %q", secret, out)
			}
		}
	}
}

func TestMapErrorSQLStates(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{{"22023", ErrUsage}, {"PT404", ErrNotFound}, {"PT409", ErrConflict}, {"55P03", ErrBusy}, {"XX000", errFailed}, {"23505", errFailed}, {"42501", errFailed}}
	for _, c := range cases {
		got := mapError(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: c.code, Message: "driver text with " + sentinelKey}))
		if !errors.Is(got, c.want) {
			t.Errorf("%s -> %v, want %v", c.code, got, c.want)
		}
		if strings.Contains(got.Error(), sentinelKey) {
			t.Errorf("%s leaked the driver message", c.code)
		}
	}
	if got := mapError(errors.New("dial tcp postgres://u:" + sentinelKey + "@h/db")); !errors.Is(got, errFailed) || strings.Contains(got.Error(), sentinelKey) {
		t.Fatalf("non-pg error -> %v", got)
	}
	if mapError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestParseCounts(t *testing.T) {
	c, all, err := parseCounts([]byte(`{"bundles":2,"links":5,"unknown_new_key":9,"held":1,"retry_after":1790000000}`))
	if err != nil {
		t.Fatal(err)
	}
	if c["bundles"] != 2 || c["links"] != 5 {
		t.Fatalf("counts %v", c)
	}
	if _, ok := c["unknown_new_key"]; ok {
		t.Fatal("keys outside D5 must be dropped")
	}
	if _, ok := c["retry_after"]; ok || all["retry_after"] != 1790000000 || all["held"] != 1 {
		t.Fatalf("raw values %v %v", c, all)
	}
	for _, bad := range []string{`{"bundles":"3"}`, `{"bundles":true}`, `{"bundles":1.5}`, `[1]`, `not json`, ``} {
		if _, _, err := parseCounts([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidSelectorShapes(t *testing.T) {
	uuid1, uuid2, uuid3 := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	key := strings.Repeat("a", 64)
	good := []Selector{
		{Request: uuid1, Object: "page", Asset: "55", ActorKey: key},
		{Request: uuid1, Object: "instagram", Asset: "55", ActorKey: key, PeerKeys: []string{key, key}},
		{Request: uuid1, Object: "page", Asset: "55", CommentRef: "1_2"},
		{Request: uuid1, Tenant: uuid2, Store: uuid3, Bundle: uuid1},
	}
	for i, s := range good {
		if !validSelector(s) {
			t.Errorf("good[%d] rejected", i)
		}
	}
	bad := map[string]Selector{
		"no selector":       {Request: uuid1, Object: "page", Asset: "55"},
		"two selectors":     {Request: uuid1, Object: "page", Asset: "55", ActorKey: key, CommentRef: "1_2"},
		"bad request":       {Request: "x", Object: "page", Asset: "55", ActorKey: key},
		"bad object":        {Request: uuid1, Object: "facebook", Asset: "55", ActorKey: key},
		"bad asset":         {Request: uuid1, Object: "page", Asset: "5x", ActorKey: key},
		"short key":         {Request: uuid1, Object: "page", Asset: "55", ActorKey: "abc"},
		"upper key":         {Request: uuid1, Object: "page", Asset: "55", ActorKey: strings.ToUpper(key)},
		"nine peers":        {Request: uuid1, Object: "page", Asset: "55", ActorKey: key, PeerKeys: make([]string, 9)},
		"bad peer":          {Request: uuid1, Object: "page", Asset: "55", ActorKey: key, PeerKeys: []string{"zz"}},
		"peer with comment": {Request: uuid1, Object: "page", Asset: "55", CommentRef: "1_2", PeerKeys: []string{key}},
		"bad comment":       {Request: uuid1, Object: "page", Asset: "55", CommentRef: "a b"},
		"bundle+object":     {Request: uuid1, Tenant: uuid2, Store: uuid3, Bundle: uuid1, Object: "page"},
		"bundle no tenant":  {Request: uuid1, Store: uuid3, Bundle: uuid1},
		"tenant with key":   {Request: uuid1, Object: "page", Asset: "55", ActorKey: key, Tenant: uuid2},
	}
	for name, s := range bad {
		if validSelector(s) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOperationsRejectBadInputWithoutAPool(t *testing.T) {
	ctx := context.Background()
	if _, err := RunOnce(ctx, nil, 500); !errors.Is(err, ErrUsage) {
		t.Fatalf("RunOnce nil pool: %v", err)
	}
	if _, _, err := Erase(ctx, nil, Selector{}); !errors.Is(err, ErrUsage) {
		t.Fatalf("Erase nil pool: %v", err)
	}
	if _, err := SetPolicy(ctx, nil, 1, Policy{}); !errors.Is(err, ErrUsage) {
		t.Fatalf("SetPolicy nil pool: %v", err)
	}
	if _, err := GetStatus(ctx, nil); !errors.Is(err, ErrUsage) {
		t.Fatalf("GetStatus nil pool: %v", err)
	}
	if _, err := Replay(ctx, nil, nil); !errors.Is(err, ErrUsage) {
		t.Fatalf("Replay nil pool: %v", err)
	}
	if _, err := NewWorker(nil); err == nil {
		t.Fatal("NewWorker(nil) must fail")
	}
}

func TestWorkerLoop(t *testing.T) {
	run := func(steps []Counts, failAt int) (int, error) {
		calls := 0
		w := &Worker{batch: func(context.Context) (Counts, error) {
			i := calls
			calls++
			if failAt == calls {
				return nil, errFailed
			}
			if i >= len(steps) {
				return Counts{"more": 1}, nil
			}
			return steps[i], nil
		}}
		err := w.Work(context.Background(), &river.Job[JobArgs]{})
		return calls, err
	}
	if n, err := run([]Counts{{"more": 0, "bundles": 1}}, 0); err != nil || n != 1 {
		t.Fatalf("stops on more=0: calls=%d err=%v", n, err)
	}
	if n, err := run([]Counts{{"more": 1}, {"more": 1}, {"more": 0}}, 0); err != nil || n != 3 {
		t.Fatalf("three batches: calls=%d err=%v", n, err)
	}
	if n, err := run([]Counts{{"more": 1}, {"busy": 1}}, 0); err != nil || n != 2 {
		t.Fatalf("stops on busy: calls=%d err=%v", n, err)
	}
	if n, err := run(nil, 0); err != nil || n != maxBatches {
		t.Fatalf("stops at %d batches: calls=%d err=%v", maxBatches, n, err)
	}
	if n, err := run([]Counts{{"more": 1}, {"more": 1}}, 2); !errors.Is(err, errFailed) || n != 2 {
		t.Fatalf("database error must return for River retry: calls=%d err=%v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &Worker{batch: func(context.Context) (Counts, error) { t.Fatal("batch after cancel"); return nil, nil }}
	if err := w.Work(ctx, &river.Job[JobArgs]{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

// A long backlog must end the job inside the rescue window: no new batch starts after the budget, and the rest
// waits for the next hour (more stays 1, the job still succeeds).
func TestWorkerStopsStartingBatchesAfterBudget(t *testing.T) {
	calls := 0
	w := &Worker{budget: 30 * time.Millisecond, batch: func(context.Context) (Counts, error) {
		calls++
		time.Sleep(20 * time.Millisecond)
		return Counts{"more": 1}, nil
	}}
	if err := w.Work(context.Background(), &river.Job[JobArgs]{}); err != nil {
		t.Fatal(err)
	}
	if calls < 1 || calls >= maxBatches {
		t.Fatalf("calls=%d, want the budget to stop the loop after at least 1 and fewer than %d batches", calls, maxBatches)
	}
}

func TestJobContract(t *testing.T) {
	if (JobArgs{}).Kind() != "claims_retention_v1" || JobKind != "claims_retention_v1" {
		t.Fatal("kind")
	}
	opts := (JobArgs{}).InsertOpts()
	if opts.UniqueOpts.ByPeriod != time.Hour || opts.Queue != "" {
		t.Fatalf("insert opts %+v", opts)
	}
	w := &Worker{}
	// r2-close-retention: River rescues a job running longer than cmd/claims-worker's RescueStuckJobsAfter (one minute)
	// and starts a second runner, so the job must time out before that.
	if d := w.Timeout(nil); d <= 0 || d >= RescueWindow {
		t.Fatalf("timeout %v, want 0 < timeout < rescue window %v", d, RescueWindow)
	}
	if RescueWindow != time.Minute {
		t.Fatal("RescueWindow must equal the claims-worker RescueStuckJobsAfter (one minute)")
	}
	if PeriodicJob() == nil {
		t.Fatal("periodic job")
	}
	// The worker registers with River without a runtime panic (kind, args, worker types line up).
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
}

// TestRiverClientAcceptsWorkerAndPeriodicJob builds a client (no connection is made) the way
// cmd/claims-worker will: it fails if the worker's args, the periodic job or the schedule are
// not valid for River v0.40.0.
func TestRiverClientAcceptsWorkerAndPeriodicJob(t *testing.T) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &Worker{})
	_, err := river.NewClient(riverpgxv5.New(nil), &river.Config{
		Schema: "river", Workers: workers, PeriodicJobs: []*river.PeriodicJob{PeriodicJob()},
	})
	if err != nil {
		t.Fatalf("river rejected the retention worker/periodic job: %v", err)
	}
}
