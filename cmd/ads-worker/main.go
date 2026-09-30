package main

// main.go: config + River assembly of the ads worker. Same shape as cmd/claims-worker (config
// redaction, worker pool, core.NewDispatcher, River client), with the queue set to "ads" only.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/ads"
	"livecommerce/internal/attribution"
	"livecommerce/internal/attribution/capiroute"
	"livecommerce/internal/integrations/core"
	metaads "livecommerce/internal/integrations/meta_ads"
	"livecommerce/internal/integrations/meta_ads/tokenopen"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
)

// queueAds is the only River queue this process works (ads-core D4, post_river/0015). Working
// "default" would steal claims-worker's private-reply operations into errRouteMissing.
const queueAds = "ads"

var (
	errWorkerConfig   = errors.New("ads_worker_invalid_config")
	errWorkerDatabase = errors.New("ads_worker_database_unavailable")
	errWorkerRoutes   = errors.New("ads_worker_routes_unavailable")
	errWorkerStart    = errors.New("ads_worker_start_failed")
	errWorkerStop     = errors.New("ads_worker_stop_failed")
)

type workerConfig struct {
	workerDSN string
	keys      *tokenopen.Keyring
	graph     metaads.Config
	// externalIDKey is the C3 HMAC key of the CAPI external_id (COMMERCE_CAPI_EXTERNAL_ID_KEY_FILE, worker only, O-D).
	externalIDKey []byte
}

func (workerConfig) String() string     { return "adsWorkerConfig{redacted}" }
func (c workerConfig) GoString() string { return c.String() }
func (workerConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal("adsWorkerConfig{redacted}")
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func validDSN(s string) bool { return len(s) >= 1 && len(s) <= 8192 && strings.TrimSpace(s) != "" }

// loadConfig reads exactly the four variables listed in doc.go (the env sentinel test records every
// name asked for). All are required: unlike claims-worker there is no enable flag, the process exists
// only where ads are deployed, and a half-configured ads worker must not start.
func loadConfig(getenv func(string) string) (workerConfig, error) {
	var c workerConfig
	if getenv == nil {
		return c, errWorkerConfig
	}
	c.workerDSN = getenv("COMMERCE_ADS_WORKER_DATABASE_URL")
	if !validDSN(c.workerDSN) {
		return workerConfig{}, errWorkerConfig
	}
	keys, err := tokenopen.LoadKeyring(getenv)
	if err != nil {
		return workerConfig{}, errWorkerConfig
	}
	c.keys = keys
	// GraphBaseURL stays the fixed production host: no env can point this process at another host.
	c.graph = metaads.Config{GraphBaseURL: metaads.GraphHost, GraphVersion: getenv("COMMERCE_META_ADS_GRAPH_VERSION"),
		PartnerAgent: getenv("COMMERCE_META_ADS_PARTNER_AGENT")}
	if c.graph.PartnerAgent == "" {
		return workerConfig{}, errWorkerConfig
	}
	if _, err := metaads.NewClient(c.graph); err != nil {
		return workerConfig{}, errWorkerConfig
	}
	key, err := metaads.SecretFromEnv(getenv, "COMMERCE_CAPI_EXTERNAL_ID_KEY", 4096)
	if err != nil || len(key) < 32 {
		clear(key)
		return workerConfig{}, errWorkerConfig
	}
	c.externalIDKey = key
	return c, nil
}

// dispatcherOptions widens the per-call bound to 15 s: a reconcile listing may walk several pages and
// an insights read makes three GETs inside one call context. The lease inequality
// (CallTimeout + 3*DBTimeout + 1 s < LeaseSeconds) still holds: 15+6+1 < 30.
func dispatcherOptions() core.DispatcherOptions {
	o := core.DefaultDispatcherOptions()
	o.CallTimeout = 15 * time.Second
	return o
}

func run(ctx context.Context, getenv func(string) string) error {
	c, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	if ctx == nil {
		return errWorkerConfig
	}
	startup, done := context.WithTimeout(ctx, 20*time.Second)
	defer done()
	pool, err := platform.OpenWorkerPool(startup, c.workerDSN)
	if err != nil {
		return errWorkerDatabase
	}
	defer pool.Close()
	// ads.Checker.Check: PG only, no network, no secret; nil in reconcile mode (ruling X2).
	routes, err := metaads.Routes(pool, c.graph, c.keys, ads.NewChecker(pool).Check)
	if err != nil {
		return errWorkerRoutes
	}
	// ads-capi: the one meta_dataset/meta.capi.purchase route (Check, lease-fenced user data, one POST, never resent).
	capiRoutes, err := capiroute.Routes(pool, c.graph, c.keys, c.externalIDKey)
	clear(c.externalIDKey) // Routes copied it
	if err != nil {
		return errWorkerRoutes
	}
	routes = append(routes, capiRoutes...)
	dispatcher, err := core.NewDispatcher(startup, pool, routes, dispatcherOptions())
	if err != nil {
		return errWorkerRoutes
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, dispatcher)
	// ads domain workers (publish-advance sweeper, insights planner, OAuth-state purge): plan and
	// record only, never call Meta.
	if err := ads.AddWorkers(workers, pool); err != nil {
		return errWorkerRoutes
	}
	// capi_purchase_sweep_v1: plans CAPI operations and purges stale browser contexts (never calls Meta).
	if err := attribution.AddWorkers(workers, pool); err != nil {
		return errWorkerRoutes
	}
	// Queue "ads" only. A stuck job (crash mid-dispatch) is rescued after one minute so a pause is not
	// stranded for River's default hour.
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema: "river", Workers: workers, RescueStuckJobsAfter: time.Minute,
		Queues:       map[string]river.QueueConfig{queueAds: {MaxWorkers: 4}},
		PeriodicJobs: append(ads.PeriodicJobs(), attribution.PeriodicJobs()...),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return errWorkerStart
	}
	done()
	// IR-13: operations without a registered route stay READY by design, so the log names every route
	// this process serves. Fixed provider/action/purpose strings only.
	names := make([]string, 0, len(routes))
	for _, r := range routes {
		names = append(names, r.Provider+"/"+r.Action+"/"+r.Purpose)
	}
	slog.Info("ads_worker_routes", "routes", strings.Join(names, ","))
	// Fixed local startup witness; never implies Meta access.
	runErr := jobqueue.Run(ctx, client, "ads_worker_ready")
	switch {
	case errors.Is(runErr, jobqueue.ErrStart):
		return errWorkerStart
	case errors.Is(runErr, jobqueue.ErrStop):
		return errWorkerStop
	default:
		return runErr
	}
}
