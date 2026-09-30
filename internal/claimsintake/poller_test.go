package claimsintake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
)

// TestClassify pins the §5.3 failure table: which errors end the row (final) and which retry under the
// fail_meta_intake backoff and 10-attempt cap. Codes must satisfy fail_meta_intake's ^[a-z0-9_]{1,40}$.
func TestClassify(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code}) }
	cases := []struct {
		name  string
		err   error
		code  string
		final bool
	}{
		{"invalid sentinel", command.ErrInvalid, "invalid", true},
		{"22023", pg("22023"), "invalid", true},
		{"23505 reply key", pg("23505"), "reply_key_conflict", true},
		{"23514", pg("23514"), "sqlstate_23514", true},
		{"23503", pg("23503"), "sqlstate_23503", true},
		{"42501", pg("42501"), "sqlstate_42501", true},
		{"not found sentinel", command.ErrNotFound, "not_found", true},
		{"deadlock", pg("40P01"), "sqlstate_40p01", false},
		{"lock timeout", pg("55P03"), "sqlstate_55p03", false},
		{"statement timeout", pg("57014"), "sqlstate_57014", false},
		{"connection", pg("08006"), "sqlstate_08006", false},
		{"conflict sentinel", command.ErrConflict, "conflict", false},
		{"deadline", context.DeadlineExceeded, "timeout", false},
		{"unknown", errors.New("boom: secret-looking text"), "error", false},
	}
	for _, c := range cases {
		code, final := classify(c.err)
		if code != c.code || final != c.final {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, code, final, c.code, c.final)
		}
		if len(code) < 1 || len(code) > 40 {
			t.Errorf("%s: code %q out of fail_meta_intake bounds", c.name, code)
		}
		for _, r := range code {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
				t.Errorf("%s: code %q has character %q", c.name, code, r)
			}
		}
	}
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	key, err := claims.NewReplyLinkKey(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, run := range map[string]func() error{
		"nil pool":         func() error { _, e := New(ctx, nil, key, Config{}); return e },
		"zero key":         func() error { _, e := New(ctx, nil, claims.ReplyLinkKey{}, Config{}); return e },
		"too many workers": func() error { _, e := New(ctx, nil, key, Config{Workers: 9}); return e },
		"negative workers": func() error { _, e := New(ctx, nil, key, Config{Workers: -1}); return e },
		"tiny sleep":       func() error { _, e := New(ctx, nil, key, Config{IdleSleep: time.Millisecond}); return e },
	} {
		if err := run(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSQLStateHelper(t *testing.T) {
	// finish is exercised against real PG in the MCI gates; here only the error class of a nil-safe
	// transaction-free path: sqlState never panics on non-PG errors.
	if sqlState(errors.New("x")) != "" || sqlState(&pgconn.PgError{Code: "40001"}) != "40001" || sqlState(pgx.ErrNoRows) != "" {
		t.Fatal("sqlState")
	}
}
