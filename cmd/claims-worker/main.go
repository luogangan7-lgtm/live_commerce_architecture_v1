package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/claims"
	"livecommerce/internal/claimsintake"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/metareply"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/integrations/shipping/ecpay/ecpayroute"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
	"livecommerce/internal/retention"
)

var (
	errWorkerConfig   = errors.New("claims_worker_invalid_config")
	errWorkerDatabase = errors.New("claims_worker_database_unavailable")
	errWorkerRoutes   = errors.New("claims_worker_routes_unavailable")
	errWorkerStart    = errors.New("claims_worker_start_failed")
	errWorkerStop     = errors.New("claims_worker_stop_failed")
)

type workerConfig struct {
	enabled      bool
	intakeDSN    string
	workerDSN    string
	retentionDSN string
	linkKey      claims.ReplyLinkKey
	pageKeys     *metareply.PageTokenKeyring
	graph        metareply.Config
	// ECPay CVS route (taiwan-cvs-logistics-v1 §7.4): registered only when ecpayCfg.Enabled.
	ecpayCfg    ecpay.Config
	ecpayKeys   *ecpay.Keyring
	ecpayClient *ecpay.Client
}

func (workerConfig) String() string     { return "claimsWorkerConfig{redacted}" }
func (c workerConfig) GoString() string { return c.String() }
func (workerConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal("claimsWorkerConfig{redacted}")
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

// loadConfig reads only the variables listed in doc.go: never K_actor, the Meta payload keyring or a
// Stripe variable (the env sentinel test records every name asked for; MCI10 greps this package for them).
func loadConfig(getenv func(string) string) (workerConfig, error) {
	var c workerConfig
	if getenv == nil {
		return c, errWorkerConfig
	}
	switch getenv("COMMERCE_CLAIMS_WORKER_ENABLED") {
	case "", "0":
		return c, nil
	case "1":
		c.enabled = true
	default:
		return workerConfig{}, errWorkerConfig
	}
	c.intakeDSN = getenv("COMMERCE_CLAIMS_INTAKE_DATABASE_URL")
	c.workerDSN = getenv("COMMERCE_WORKER_DATABASE_URL")
	// U08 (claims-retention-purge-v1 §5): the hourly purge job runs on its own login, required whenever the worker is on.
	c.retentionDSN = getenv("COMMERCE_RETENTION_JOB_DATABASE_URL")
	if !validDSN(c.intakeDSN) || !validDSN(c.workerDSN) || !validDSN(c.retentionDSN) {
		return workerConfig{}, errWorkerConfig
	}
	raw, err := base64.StdEncoding.DecodeString(getenv("COMMERCE_CLAIMS_REPLY_LINK_KEY"))
	if err != nil {
		return workerConfig{}, errWorkerConfig
	}
	if c.linkKey, err = claims.NewReplyLinkKey(raw); err != nil {
		return workerConfig{}, errWorkerConfig
	}
	if c.pageKeys, err = metareply.LoadPageTokenKeyring(getenv); err != nil {
		return workerConfig{}, errWorkerConfig
	}
	c.graph = metareply.Config{GraphBaseURL: metareply.GraphHost, GraphVersion: getenv("COMMERCE_META_GRAPH_VERSION")}
	if base := getenv("COMMERCE_META_GRAPH_BASE_URL"); base != "" {
		c.graph.GraphBaseURL = base
	}
	switch getenv("COMMERCE_META_GRAPH_AUTH_HEADER") {
	case "", "0":
	case "1":
		c.graph.AuthorizationHeader = true
	default:
		return workerConfig{}, errWorkerConfig
	}
	if c.graph.Validate() != nil {
		return workerConfig{}, errWorkerConfig
	}
	if c.ecpayCfg, err = ecpay.LoadConfig(getenv); err != nil {
		return workerConfig{}, errWorkerConfig
	}
	if c.ecpayCfg.Enabled {
		// One ECPay environment per deployment = its payment profile (§12, LQ5): PROVIDER_MOCK runs SANDBOX.
		env := ecpay.EnvSandbox
		switch getenv("COMMERCE_PAYMENT_PROFILE") {
		case "PROVIDER_MOCK", "SANDBOX":
		case "LIVE":
			env = ecpay.EnvLive
		default:
			return workerConfig{}, errWorkerConfig
		}
		if c.ecpayKeys, err = ecpay.LoadKeyring(getenv); err != nil {
			return workerConfig{}, errWorkerConfig
		}
		if c.ecpayClient, err = ecpay.NewClient(env, nil); err != nil {
			return workerConfig{}, errWorkerConfig
		}
	}
	return c, nil
}

func run(ctx context.Context, getenv func(string) string) error {
	c, err := loadConfig(getenv)
	if err != nil || !c.enabled {
		return err
	}
	if ctx == nil {
		return errWorkerConfig
	}
	startup, done := context.WithTimeout(ctx, 20*time.Second)
	defer done()
	intakePool, err := platform.OpenClaimsIntakePool(startup, c.intakeDSN)
	if err != nil {
		return errWorkerDatabase
	}
	defer intakePool.Close()
	workerPool, err := platform.OpenWorkerPool(startup, c.workerDSN)
	if err != nil {
		return errWorkerDatabase
	}
	defer workerPool.Close()
	if !sameDatabase(startup, intakePool, workerPool) {
		return errWorkerDatabase // the operation the poller plans must be the one the dispatcher reads
	}
	// platform.OpenRetentionJobPool: admission of lc_retention_job (exactly one authority); the purge job's only SQL.
	retentionPool, err := platform.OpenRetentionJobPool(startup, c.retentionDSN)
	if err != nil {
		return errWorkerDatabase
	}
	defer retentionPool.Close()
	if !sameDatabase(startup, retentionPool, workerPool) {
		return errWorkerDatabase // the purge must run against the database whose river schema holds its job
	}
	retentionWorker, err := retention.NewWorker(retentionPool)
	if err != nil {
		return errWorkerDatabase
	}
	routes, err := metareply.Routes(workerPool, c.linkKey, c.pageKeys, c.graph)
	if err != nil {
		return errWorkerRoutes
	}
	if c.ecpayCfg.Enabled {
		ecpayRoutes, err := ecpayroute.Routes(workerPool, c.ecpayKeys, c.ecpayClient, c.ecpayCfg)
		if err != nil {
			return errWorkerRoutes
		}
		routes = append(routes, ecpayRoutes...)
	}
	dispatcher, err := core.NewDispatcher(startup, workerPool, routes, core.DefaultDispatcherOptions())
	if err != nil {
		return errWorkerRoutes
	}
	poller, err := claimsintake.New(startup, intakePool, c.linkKey, claimsintake.Config{})
	if err != nil {
		return errWorkerDatabase
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, dispatcher)
	river.AddWorker(workers, retentionWorker)
	// Default queue of the main river schema: the only queue external_operation_v1 jobs use. A stuck job
	// (crash mid-dispatch) is rescued after one minute so a reply is not stranded for River's default hour.
	client, err := river.NewClient(riverpgxv5.New(workerPool), &river.Config{
		Schema: "river", Workers: workers, RescueStuckJobsAfter: retention.RescueWindow,
		// U08: hourly + on start, unique per hour (retention.JobArgs.InsertOpts); inserted by commerce_worker (IR-4).
		PeriodicJobs: []*river.PeriodicJob{retention.PeriodicJob()},
		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return errWorkerStart
	}
	done()
	// IR-13: operations without a registered route stay READY by design, so the log names every route this
	// process serves. Fixed provider/action/purpose strings only.
	names := make([]string, 0, len(routes))
	for _, r := range routes {
		names = append(names, r.Provider+"/"+r.Action+"/"+r.Purpose)
	}
	slog.Info("claims_worker_routes", "routes", strings.Join(names, ","))

	pollCtx, stopPoll := context.WithCancel(ctx)
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		_ = poller.Run(pollCtx)
	}()
	// Fixed local startup witness; never implies Meta access.
	runErr := jobqueue.Run(ctx, client, "claims_worker_ready")
	stopPoll()
	<-polled
	switch {
	case errors.Is(runErr, jobqueue.ErrStart):
		return errWorkerStart
	case errors.Is(runErr, jobqueue.ErrStop):
		return errWorkerStop
	default:
		return runErr
	}
}

// sameDatabase compares current_database() of the two pools (one bounded query each).
func sameDatabase(ctx context.Context, a, b *pgxpool.Pool) bool {
	var na, nb string
	return a.QueryRow(ctx, `SELECT current_database()`).Scan(&na) == nil &&
		b.QueryRow(ctx, `SELECT current_database()`).Scan(&nb) == nil && na == nb
}
