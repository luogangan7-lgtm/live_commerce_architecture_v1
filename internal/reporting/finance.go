// finance.go serves the BD7 finance summary (contracts/customers-billing-v1.md §3.1, §5): strict parsing of the
// date range, one database call, strict decoding and re-verification of the rows, totals, and the CSV.
//
// Non-goals: no write, no HTTP (internal/httpapi/finance.go), no currency conversion: totals are per
// (currency, environment) so no sum ever mixes a test-mode and a live amount or two currencies (I05).

package reporting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// Timezone is the finance day boundary (Q11, UTC+8, no DST).
const Timezone = "Asia/Taipei"

// MaxRangeDays is the largest allowed to-from difference (a range of 92 calendar days, §7).
const MaxRangeDays = 91

var (
	// ErrUnavailable is any projection failure (malformed or inconsistent database result).
	ErrUnavailable = errors.New("finance unavailable")
	dayPattern     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	currencyCode   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// FinanceRow is one day of one (currency, environment); amounts are integer minor units (I05).
type FinanceRow struct {
	Day           string `json:"day"`
	Currency      string `json:"currency"`
	Environment   string `json:"environment"`
	CapturedCount int64  `json:"captured_count"`
	CapturedMinor int64  `json:"captured_minor"`
	RefundedMinor int64  `json:"refunded_minor"`
	NetMinor      int64  `json:"net_minor"`
}

// FinanceSummary: Totals hold one row per (currency, environment) with Day == "".
type FinanceSummary struct {
	From     string       `json:"from"`
	To       string       `json:"to"`
	Timezone string       `json:"timezone"`
	Rows     []FinanceRow `json:"rows"`
	Totals   []FinanceRow `json:"totals"`
}

// ParseRange validates from/to as YYYY-MM-DD calendar dates with 0 <= to-from <= 91 (D13).
func ParseRange(from, to string) (time.Time, time.Time, error) {
	if !dayPattern.MatchString(from) || !dayPattern.MatchString(to) {
		return time.Time{}, time.Time{}, command.ErrInvalid
	}
	f, e1 := time.Parse("2006-01-02", from)
	t, e2 := time.Parse("2006-01-02", to)
	if e1 != nil || e2 != nil || f.Format("2006-01-02") != from || t.Format("2006-01-02") != to {
		return time.Time{}, time.Time{}, command.ErrInvalid
	}
	if days := t.Sub(f).Hours() / 24; days < 0 || days > MaxRangeDays {
		return time.Time{}, time.Time{}, command.ErrInvalid
	}
	return f, t, nil
}

// Finance returns the daily rows and per-(currency, environment) totals for the range.
func Finance(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, from, to string) (FinanceSummary, error) {
	rows, err := read(ctx, tx, scope, token, from, to, false)
	if err != nil {
		return FinanceSummary{}, err
	}
	return FinanceSummary{From: from, To: to, Timezone: Timezone, Rows: rows, Totals: Totals(rows)}, nil
}

// FinanceCSV returns the same rows as CSV through identity.export_finance_summary (orders:export AND
// orders:read, one audit row finance.exported). Columns are D13's; totals are not included.
func FinanceCSV(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, from, to string) ([]byte, error) {
	rows, err := read(ctx, tx, scope, token, from, to, true)
	if err != nil {
		return nil, err
	}
	return CSV(rows)
}

func read(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, from, to string, export bool) ([]FinanceRow, error) {
	if tx == nil || !command.ValidID(scope.TenantID) || !command.ValidID(scope.StoreID) || !command.ValidID(scope.PrincipalID) ||
		scope.Revision < 1 || len(token) < 32 || len(token) > 512 {
		return nil, command.ErrInvalid
	}
	if _, _, err := ParseRange(from, to); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(token))
	permission, query := "orders:read", `SELECT identity.read_finance_summary($1,$2::uuid,$3::date,$4::date)`
	if export {
		// identity.export_finance_summary: same rows, orders:export + orders:read, audit row finance.exported.
		permission, query = "orders:export", `SELECT identity.export_finance_summary($1,$2::uuid,$3::date,$4::date)`
	} // else identity.read_finance_summary: orders:read, no write
	var raw []byte
	if err := tx.QueryRow(ctx, query, hash[:], scope.StoreID, from, to).Scan(&raw); err != nil {
		return nil, mapError(err)
	}
	// Second fence with the original Go Scope (merchantorders.read pattern).
	if err := platform.RequirePermission(ctx, tx, scope, token, permission); err != nil {
		return nil, mapError(err)
	}
	return decodeRows(raw, from, to)
}

// decodeRows re-verifies the database projection: strict keys, days inside the range, closed environment
// vocabulary, non-negative amounts, net = captured - refunded (I05), and strict (day, currency, environment)
// order with no duplicates.
func decodeRows(raw []byte, from, to string) ([]FinanceRow, error) {
	var objects []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &objects) != nil {
		return nil, ErrUnavailable
	}
	rows := make([]FinanceRow, 0, len(objects))
	for _, object := range objects {
		var fields map[string]json.RawMessage
		if json.Unmarshal(object, &fields) != nil || len(fields) != 7 {
			return nil, ErrUnavailable
		}
		var row FinanceRow
		dec := json.NewDecoder(bytes.NewReader(object))
		dec.DisallowUnknownFields()
		if dec.Decode(&row) != nil || !validRow(row, from, to) {
			return nil, ErrUnavailable
		}
		if n := len(rows); n > 0 && !rowLess(rows[n-1], row) {
			return nil, ErrUnavailable
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func validRow(r FinanceRow, from, to string) bool {
	return dayPattern.MatchString(r.Day) && r.Day >= from && r.Day <= to && currencyCode.MatchString(r.Currency) &&
		(r.Environment == "SANDBOX" || r.Environment == "LIVE") &&
		r.CapturedCount >= 0 && r.CapturedMinor >= 0 && r.RefundedMinor >= 0 &&
		(r.CapturedCount > 0 || r.CapturedMinor == 0) && r.NetMinor == r.CapturedMinor-r.RefundedMinor
}

func rowLess(a, b FinanceRow) bool {
	if a.Day != b.Day {
		return a.Day < b.Day
	}
	if a.Currency != b.Currency {
		return a.Currency < b.Currency
	}
	return a.Environment < b.Environment
}

// Totals sums rows per (currency, environment); the result is sorted by currency then environment and has
// Day == "". A sum never crosses a currency or an environment (I05).
func Totals(rows []FinanceRow) []FinanceRow {
	type key struct{ currency, environment string }
	sums := map[key]*FinanceRow{}
	for _, r := range rows {
		k := key{r.Currency, r.Environment}
		t := sums[k]
		if t == nil {
			t = &FinanceRow{Currency: r.Currency, Environment: r.Environment}
			sums[k] = t
		}
		t.CapturedCount += r.CapturedCount
		t.CapturedMinor += r.CapturedMinor
		t.RefundedMinor += r.RefundedMinor
		t.NetMinor += r.NetMinor
	}
	out := make([]FinanceRow, 0, len(sums))
	for _, t := range sums {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return rowLess(out[i], out[j]) })
	return out
}

// CSV renders the D13 columns with a header row. Every field is a date, a 3-letter currency, a closed
// environment word or an integer, so no field can start a spreadsheet formula.
func CSV(rows []FinanceRow) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"day", "currency", "environment", "captured_count", "captured_minor", "refunded_minor", "net_minor"}); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if err := w.Write([]string{r.Day, r.Currency, r.Environment, strconv.FormatInt(r.CapturedCount, 10),
			strconv.FormatInt(r.CapturedMinor, 10), strconv.FormatInt(r.RefundedMinor, 10), strconv.FormatInt(r.NetMinor, 10)}); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mapError turns a database or platform error into a stable sentinel; raw pgconn messages never leave here.
func mapError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "PT400":
			return command.ErrInvalid
		case "PT401":
			return platform.ErrUnauthorized
		case "PT403":
			return platform.ErrForbidden
		case "PT404":
			return platform.ErrScopeNotFound
		case "40001", "40P01", "55P03", "57014":
			return err // the HTTP layer maps deadlocks and lock timeouts to a retry
		}
		return ErrUnavailable
	}
	if errors.Is(err, platform.ErrUnauthorized) || errors.Is(err, platform.ErrForbidden) ||
		errors.Is(err, platform.ErrScopeNotFound) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return ErrUnavailable
}
