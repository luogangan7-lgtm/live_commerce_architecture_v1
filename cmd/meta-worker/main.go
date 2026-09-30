package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"livecommerce/internal/integrations/meta"
	"livecommerce/internal/jobqueue"
	"livecommerce/internal/platform"
)

var (
	errWorkerConfig   = errors.New("meta_worker_invalid_config")
	errWorkerDatabase = errors.New("meta_worker_database_unavailable")
	errWorkerStart    = errors.New("meta_worker_start_failed")
	errWorkerStop     = errors.New("meta_worker_stop_failed")
)

type workerConfig struct {
	enabled     bool
	workerDSN   string
	consumerDSN string
	concurrency int
	keys        *meta.PayloadKeyring
	actor       meta.ClaimsActorKey // K_actor; zero = claim staging off (meta-claims-intake-v1 §3)
}

func (workerConfig) String() string               { return "metaWorkerConfig{redacted}" }
func (c workerConfig) GoString() string           { return c.String() }
func (workerConfig) MarshalJSON() ([]byte, error) { return json.Marshal("metaWorkerConfig{redacted}") }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func loadConfig(getenv func(string) string) (workerConfig, error) {
	var c workerConfig
	if getenv == nil {
		return c, errWorkerConfig
	}
	switch getenv("COMMERCE_META_WORKER_ENABLED") {
	case "", "0":
		return c, nil
	case "1":
		c.enabled = true
	default:
		return workerConfig{}, errWorkerConfig
	}
	c.workerDSN = getenv("COMMERCE_META_WORKER_DATABASE_URL")
	c.consumerDSN = getenv("COMMERCE_META_CONSUMER_DATABASE_URL")
	if len(c.workerDSN) < 1 || len(c.workerDSN) > 8192 || strings.TrimSpace(c.workerDSN) == "" ||
		len(c.consumerDSN) < 1 || len(c.consumerDSN) > 8192 || strings.TrimSpace(c.consumerDSN) == "" {
		return workerConfig{}, errWorkerConfig
	}
	c.concurrency = 4
	if raw := getenv("COMMERCE_META_WORKER_CONCURRENCY"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 16 || strconv.Itoa(n) != raw {
			return workerConfig{}, errWorkerConfig
		}
		c.concurrency = n
	}
	var err error
	c.keys, err = meta.LoadPayloadKeyring(getenv)
	if err != nil {
		return workerConfig{}, errWorkerConfig
	}
	// COMMERCE_CLAIMS_ACTOR_KEY unset keeps the consumer byte-identical to MC01-07 (no staging);
	// a malformed value is a configuration error, never a silent "off".
	if c.actor, _, err = meta.LoadClaimsActorKey(getenv); err != nil {
		return workerConfig{}, errWorkerConfig
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
	startup, done := context.WithTimeout(ctx, 10*time.Second)
	workerPool, err := platform.OpenMetaWorkerPool(startup, c.workerDSN)
	if err != nil {
		done()
		return errWorkerDatabase
	}
	defer workerPool.Close()
	consumerPool, err := platform.OpenMetaConsumerPool(startup, c.consumerDSN)
	if err != nil {
		done()
		return errWorkerDatabase
	}
	defer consumerPool.Close()
	client, err := meta.NewConsumerClientWithClaims(startup, workerPool, consumerPool, c.keys, c.actor, c.concurrency)
	done()
	if err != nil {
		return errWorkerDatabase
	}
	// This fixed witness confirms only local startup, never provider approval.
	switch err := jobqueue.Run(ctx, client, "meta_worker_ready"); {
	case errors.Is(err, jobqueue.ErrStart):
		return errWorkerStart
	case errors.Is(err, jobqueue.ErrStop):
		return errWorkerStop
	default:
		return err
	}
}
