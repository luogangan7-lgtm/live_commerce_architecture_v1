// Package live owns the merchant-scoped live-planning aggregate: drafts, the Studio projection,
// rehearsal and media plans, the LiveKit Egress execution, stop and recovery workers, and browser-
// input preparation. Callers pass a transaction from platform.WithScope and roll it back on every
// error.
//
// It never serves HTTP (internal/httpapi does), never talks to LiveKit except through
// internal/integrations/livekit, and never lets a worker decide authorization: plans are admitted in
// SQL and workers execute leased, fenced jobs.
package live

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

const (
	managePermission = "live:manage"
	readPermission   = "live:read"
)

type DraftInput struct {
	Title       string     `json:"title"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	AspectRatio string     `json:"aspect_ratio"`
}

type Draft struct {
	ID          string     `json:"session_id"`
	ProgramID   string     `json:"program_id"`
	Title       string     `json:"title"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	AspectRatio string     `json:"aspect_ratio"`
	State       string     `json:"state"`
	Version     int64      `json:"version"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// CreateDraft persists one session, program, audit event, and replay receipt.
func CreateDraft(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in DraftInput) (Draft, error) {
	in, err := canonicalInput(in)
	if err != nil {
		return Draft{}, err
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Draft{}, err
	}
	request := struct {
		PrincipalID string `json:"principal_id"`
		DraftInput
	}{scope.PrincipalID, in}
	var out Draft
	err = command.Run(ctx, tx, scope, "live.draft.create", key, request, &out, func() error {
		if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
			return err
		}
		var scheduled pgtype.Timestamptz
		err := tx.QueryRow(ctx, `INSERT INTO live.sessions(tenant_id,store_id,principal_id,title,scheduled_at)
			VALUES($1,$2,$3,$4,$5) RETURNING id::text,title,scheduled_at,version,created_at,updated_at`,
			scope.TenantID, scope.StoreID, scope.PrincipalID, in.Title, in.ScheduledAt).
			Scan(&out.ID, &out.Title, &scheduled, &out.Version, &out.CreatedAt, &out.UpdatedAt)
		if err != nil {
			return mapError(err)
		}
		out.ScheduledAt = scheduledTime(scheduled)
		err = tx.QueryRow(ctx, `INSERT INTO live.programs(tenant_id,store_id,session_id,principal_id,aspect_ratio)
			VALUES($1,$2,$3,$4,$5) RETURNING id::text,aspect_ratio,state`,
			scope.TenantID, scope.StoreID, out.ID, scope.PrincipalID, in.AspectRatio).
			Scan(&out.ProgramID, &out.AspectRatio, &out.State)
		if err != nil {
			return mapError(err)
		}
		return command.Audit(ctx, tx, scope, "live.draft.created")
	})
	if err != nil {
		return Draft{}, mapError(err)
	}
	// Run may replay a saved receipt after waiting; revocation still denies it.
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Draft{}, err
	}
	return out, nil
}

// UpdateDraft changes both rows once for the expected session version.
func UpdateDraft(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, id string, expectedVersion int64, in DraftInput) (Draft, error) {
	if !command.ValidID(id) || expectedVersion < 1 {
		return Draft{}, command.ErrInvalid
	}
	in, err := canonicalInput(in)
	if err != nil {
		return Draft{}, err
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Draft{}, err
	}
	request := struct {
		PrincipalID     string `json:"principal_id"`
		SessionID       string `json:"session_id"`
		ExpectedVersion int64  `json:"expected_version"`
		DraftInput
	}{scope.PrincipalID, id, expectedVersion, in}
	var out Draft
	err = command.Run(ctx, tx, scope, "live.draft.update", key, request, &out, func() error {
		var version int64
		// LOCK: session before program, so every aggregate mutation has one order.
		err := tx.QueryRow(ctx, `SELECT version FROM live.sessions
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 FOR UPDATE`,
			scope.TenantID, scope.StoreID, id).Scan(&version)
		if err != nil {
			return mapError(err)
		}
		var programID string
		err = tx.QueryRow(ctx, `SELECT id::text FROM live.programs
			WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 FOR UPDATE`,
			scope.TenantID, scope.StoreID, id).Scan(&programID)
		if err != nil {
			return mapError(err)
		}
		if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
			return err
		}
		if version != expectedVersion {
			return command.ErrConflict
		}
		var scheduled pgtype.Timestamptz
		err = tx.QueryRow(ctx, `UPDATE live.sessions SET title=$4,scheduled_at=$5,version=version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3 AND version=$6
			RETURNING id::text,title,scheduled_at,version,created_at,updated_at`,
			scope.TenantID, scope.StoreID, id, in.Title, in.ScheduledAt, expectedVersion).
			Scan(&out.ID, &out.Title, &scheduled, &out.Version, &out.CreatedAt, &out.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return command.ErrConflict
		}
		if err != nil {
			return mapError(err)
		}
		out.ScheduledAt = scheduledTime(scheduled)
		err = tx.QueryRow(ctx, `UPDATE live.programs SET aspect_ratio=$4
			WHERE tenant_id=$1 AND store_id=$2 AND id=$3
			RETURNING id::text,aspect_ratio,state`,
			scope.TenantID, scope.StoreID, programID, in.AspectRatio).
			Scan(&out.ProgramID, &out.AspectRatio, &out.State)
		if err != nil {
			return mapError(err)
		}
		return command.Audit(ctx, tx, scope, "live.draft.updated")
	})
	if err != nil {
		return Draft{}, mapError(err)
	}
	// A replay remains an authenticated request even after its lock wait.
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Draft{}, err
	}
	return out, nil
}

func GetDraft(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, id string) (Draft, error) {
	if !command.ValidID(id) {
		return Draft{}, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return Draft{}, err
	}
	var out Draft
	var scheduled pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT s.id::text,p.id::text,s.title,s.scheduled_at,p.aspect_ratio,p.state,
		s.version,s.created_at,s.updated_at FROM live.sessions s
		JOIN live.programs p ON p.tenant_id=s.tenant_id AND p.store_id=s.store_id AND p.session_id=s.id
		WHERE s.tenant_id=$1 AND s.store_id=$2 AND s.id=$3`,
		scope.TenantID, scope.StoreID, id).
		Scan(&out.ID, &out.ProgramID, &out.Title, &scheduled, &out.AspectRatio, &out.State,
			&out.Version, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return Draft{}, mapError(err)
	}
	out.ScheduledAt = scheduledTime(scheduled)
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return Draft{}, err
	}
	return out, nil
}

func canonicalInput(in DraftInput) (DraftInput, error) {
	if !utf8.ValidString(in.Title) || strings.TrimSpace(in.Title) != in.Title ||
		utf8.RuneCountInString(in.Title) < 1 || utf8.RuneCountInString(in.Title) > 200 ||
		(in.AspectRatio != "16:9" && in.AspectRatio != "9:16") {
		return DraftInput{}, command.ErrInvalid
	}
	for _, r := range in.Title {
		if unicode.IsControl(r) {
			return DraftInput{}, command.ErrInvalid
		}
	}
	if in.ScheduledAt != nil {
		value := in.ScheduledAt.UTC().Truncate(time.Microsecond)
		if value.Year() < 2000 || value.Year() > 2199 {
			return DraftInput{}, command.ErrInvalid
		}
		in.ScheduledAt = &value
	}
	return in, nil
}

func scheduledTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	utc := value.Time.UTC()
	return &utc
}

func authorize(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, permission string) error {
	if tx == nil || !command.ValidID(scope.TenantID) || !command.ValidID(scope.StoreID) ||
		!command.ValidID(scope.PrincipalID) || scope.Revision < 1 {
		return command.ErrInvalid
	}
	var isolation, tenantID, storeID, principalID string
	err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'),
		coalesce(current_setting('app.tenant_id',true),''),
		coalesce(current_setting('app.store_id',true),''),
		coalesce(current_setting('app.principal_id',true),'')`).
		Scan(&isolation, &tenantID, &storeID, &principalID)
	if err != nil {
		return err
	}
	if isolation != "read committed" || tenantID != scope.TenantID || storeID != scope.StoreID || principalID != scope.PrincipalID {
		return command.ErrInvalid
	}
	return platform.RequirePermission(ctx, tx, scope, token, permission)
}

func mapError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return command.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return command.ErrConflict
		case "23503":
			return command.ErrNotFound
		case "22001", "22007", "22008", "22023", "22P02", "23514":
			return command.ErrInvalid
		}
	}
	return err
}
