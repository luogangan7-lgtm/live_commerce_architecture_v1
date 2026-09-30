// finance_test.go covers the pure finance logic: range parsing, strict row decoding and re-verification,
// totals per (currency, environment) and the CSV. Real-PG day boundaries and refund rules are gated by
// customers-billing-tests (CB03/CB09), not here.

package reporting

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		ok       bool
	}{
		{"2026-09-01", "2026-09-01", true},
		{"2026-09-01", "2026-09-30", true},
		{"2026-01-01", "2026-04-02", true},  // 91 days apart = 92 calendar days
		{"2026-01-01", "2026-04-03", false}, // 92 days apart
		{"2026-09-02", "2026-09-01", false},
		{"2026-02-30", "2026-03-01", false},
		{"2026-9-01", "2026-09-02", false},
		{"2026-09-01T00:00:00Z", "2026-09-02", false},
		{"", "", false},
		{"2026-09-01", "", false},
		{"2028-02-29", "2028-03-01", true}, // leap day exists
		{"2027-02-29", "2027-03-01", false},
	} {
		_, _, err := ParseRange(tc.from, tc.to)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, command.ErrInvalid)) {
			t.Fatalf("%s..%s: %v (want ok=%v)", tc.from, tc.to, err, tc.ok)
		}
	}
}

const rowsOK = `[
 {"day":"2026-09-01","currency":"TWD","environment":"SANDBOX","captured_count":2,"captured_minor":3000,"refunded_minor":500,"net_minor":2500},
 {"day":"2026-09-01","currency":"USD","environment":"SANDBOX","captured_count":1,"captured_minor":900,"refunded_minor":0,"net_minor":900},
 {"day":"2026-09-02","currency":"TWD","environment":"LIVE","captured_count":0,"captured_minor":0,"refunded_minor":200,"net_minor":-200},
 {"day":"2026-09-02","currency":"TWD","environment":"SANDBOX","captured_count":1,"captured_minor":1000,"refunded_minor":0,"net_minor":1000}]`

func TestDecodeRowsAndTotals(t *testing.T) {
	rows, err := decodeRows([]byte(rowsOK), "2026-09-01", "2026-09-02")
	if err != nil || len(rows) != 4 {
		t.Fatalf("%v %v", rows, err)
	}
	totals := Totals(rows)
	// I05: TWD SANDBOX, TWD LIVE and USD SANDBOX never merge.
	want := []FinanceRow{
		{Currency: "TWD", Environment: "LIVE", CapturedCount: 0, CapturedMinor: 0, RefundedMinor: 200, NetMinor: -200},
		{Currency: "TWD", Environment: "SANDBOX", CapturedCount: 3, CapturedMinor: 4000, RefundedMinor: 500, NetMinor: 3500},
		{Currency: "USD", Environment: "SANDBOX", CapturedCount: 1, CapturedMinor: 900, RefundedMinor: 0, NetMinor: 900},
	}
	if len(totals) != len(want) {
		t.Fatalf("%+v", totals)
	}
	for i := range want {
		if totals[i] != want[i] || totals[i].Day != "" {
			t.Fatalf("total %d: %+v want %+v", i, totals[i], want[i])
		}
	}
	if got := Totals(nil); len(got) != 0 {
		t.Fatalf("empty totals: %v", got)
	}
}

func TestDecodeRowsRejectsDrift(t *testing.T) {
	row := `{"day":"2026-09-01","currency":"TWD","environment":"SANDBOX","captured_count":1,"captured_minor":100,"refunded_minor":0,"net_minor":100}`
	for name, mutate := range map[string]func(string) string{
		"net mismatch":      func(s string) string { return strings.Replace(s, `"net_minor":100`, `"net_minor":99`, 1) },
		"day out of range":  func(s string) string { return strings.Replace(s, "2026-09-01", "2026-10-01", 1) },
		"bad environment":   func(s string) string { return strings.Replace(s, "SANDBOX", "PROVIDER_MOCK", 1) },
		"bad currency":      func(s string) string { return strings.Replace(s, "TWD", "twd", 1) },
		"negative captured": func(s string) string { return strings.Replace(s, `"captured_minor":100`, `"captured_minor":-100`, 1) },
		"extra key": func(s string) string {
			return strings.Replace(s, `"net_minor":100`, `"net_minor":100,"owner_id":"x"`, 1)
		},
		"missing key":          func(s string) string { return strings.Replace(s, `,"net_minor":100`, ``, 1) },
		"amount without count": func(s string) string { return strings.Replace(s, `"captured_count":1`, `"captured_count":0`, 1) },
	} {
		if _, err := decodeRows([]byte("["+mutate(row)+"]"), "2026-09-01", "2026-09-30"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := decodeRows([]byte("["+row+","+row+"]"), "2026-09-01", "2026-09-30"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("duplicate row accepted")
	}
	for _, bad := range []string{``, `{}`, `null`, `[1]`, `[{}]`} {
		if _, err := decodeRows([]byte(bad), "2026-09-01", "2026-09-30"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestCSV(t *testing.T) {
	rows, _ := decodeRows([]byte(rowsOK), "2026-09-01", "2026-09-02")
	body, err := CSV(rows)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if lines[0] != "day,currency,environment,captured_count,captured_minor,refunded_minor,net_minor" || len(lines) != 5 {
		t.Fatalf("%q", lines)
	}
	if lines[3] != "2026-09-02,TWD,LIVE,0,0,200,-200" {
		t.Fatalf("negative net line: %q", lines[3])
	}
	for _, line := range lines[1:] {
		if strings.ContainsAny(line, `"=@`) || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			t.Fatalf("formula-capable field: %q", line)
		}
	}
	empty, _ := CSV(nil)
	if strings.Count(string(empty), "\n") != 1 {
		t.Fatalf("empty csv should be the header only: %q", empty)
	}
}

func TestReadRefusesBadInputBeforeDatabase(t *testing.T) {
	good := platform.Scope{TenantID: "11111111-1111-4111-8111-111111111111", StoreID: "22222222-2222-4222-8222-222222222222",
		PrincipalID: "33333333-3333-4333-8333-333333333333", Revision: 1}
	if _, err := Finance(context.Background(), nil, good, strings.Repeat("t", 40), "2026-09-01", "2026-09-02"); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil tx: %v", err)
	}
	if _, err := FinanceCSV(context.Background(), nil, good, strings.Repeat("t", 40), "2026-09-01", "2026-09-02"); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil tx csv: %v", err)
	}
}

func TestMapError(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "customer Alice"} }
	for code, want := range map[string]error{"PT400": command.ErrInvalid, "PT401": platform.ErrUnauthorized, "PT403": platform.ErrForbidden,
		"PT404": platform.ErrScopeNotFound, "XX000": ErrUnavailable} {
		if got := mapError(pg(code)); !errors.Is(got, want) || strings.Contains(got.Error(), "Alice") {
			t.Fatalf("%s: %v", code, got)
		}
	}
	if got := mapError(pg("40P01")); errors.Is(got, ErrUnavailable) {
		t.Fatal("deadlock hidden")
	}
}
