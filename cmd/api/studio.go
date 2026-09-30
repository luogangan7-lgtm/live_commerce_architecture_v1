package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/live"
)

var errStudioConfig = errors.New("studio_invalid_config")
var errStudioDatabase = errors.New("studio_database_unavailable")

// studioConfig splits Studio (R1 ruling G2): enabled = live-session planning, keyword claims and
// claim-source (COMMERCE_STUDIO_ENABLED); media = LiveKit rehearsal/input planning
// (COMMERCE_STUDIO_MEDIA_ENABLED, MOCK-only, never deployed in R1; requires enabled).
type studioConfig struct{ enabled, media bool }

func loadStudioConfig(getenv func(string) string, identityEnabled bool, addr string) (studioConfig, error) {
	if getenv == nil {
		return studioConfig{}, errStudioConfig
	}
	enabled, err := flag(getenv("COMMERCE_STUDIO_ENABLED"))
	if err != nil {
		return studioConfig{}, errStudioConfig
	}
	media, err := flag(getenv("COMMERCE_STUDIO_MEDIA_ENABLED"))
	if err != nil || (media && !enabled) {
		return studioConfig{}, errStudioConfig
	}
	if !enabled {
		return studioConfig{}, nil
	}
	if !identityEnabled || !privateIdentityAddress(addr) {
		return studioConfig{}, errStudioConfig
	}
	return studioConfig{enabled: true, media: media}, nil
}

// buildStudioPlanner builds the media planner only when media is on; planning-only Studio never
// touches the media subsystem (no live.media_plan_ready(), no river_media client).
// API owns only an insert-only River client; its lifecycle stays in media-worker.
func buildStudioPlanner(ctx context.Context, pool *pgxpool.Pool, config studioConfig) (*live.MediaPlanner, error) {
	if !config.media {
		return nil, nil
	}
	if ctx == nil || pool == nil {
		return nil, errStudioDatabase
	}
	var ready bool
	if err := pool.QueryRow(ctx, `SELECT live.media_plan_ready()`).Scan(&ready); err != nil || !ready {
		return nil, errStudioDatabase
	}
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_media"})
	if err != nil {
		return nil, errStudioDatabase
	}
	planner, err := live.NewMediaPlanner(jobs)
	if err != nil {
		return nil, errStudioDatabase
	}
	return planner, nil
}
