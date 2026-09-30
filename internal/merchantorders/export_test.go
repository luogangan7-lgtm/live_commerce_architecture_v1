package merchantorders

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

func exportRow() ExportRow {
	return ExportRow{OrderID: orderID, CreatedAtUTC: "2026-09-25T04:05:06Z", ServiceCode: "cvs_711", DestinationKind: "cvs_711",
		RecipientName: "Chen, \"Amy\"", Phone: "+886912-345-678", Country: "TW", PickupNamespace: "seven", PickupCode: "000123",
		PickupName: "Shop", PickupAddress: "2 St", Items: "SKU-1×2; SKU-2×1", TotalMinor: 1100, Currency: "TWD", PickupSource: "merchant_attested"}
}

func csvOf(t *testing.T, rows ...ExportRow) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteUnshippedCSV(&b, rows); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// MF01 CSV encoder: BOM, CRLF, RFC 4180 quoting, formula guard, phone rule.
func TestWriteUnshippedCSV(t *testing.T) {
	got := csvOf(t, exportRow())
	lines := strings.Split(got, "\r\n")
	if !strings.HasPrefix(got, "\xEF\xBB\xBForder_id,created_at_utc,service_code,destination_kind,recipient_name,phone,country,region,city,postal_code,line1,line2,pickup_namespace,pickup_code,pickup_name,pickup_address,items,total_minor,currency,pickup_source\r\n") ||
		len(lines) != 3 || lines[2] != "" || strings.Contains(got, "\n\n") || strings.Count(got, "\n") != strings.Count(got, "\r\n") {
		t.Fatalf("framing: %q", got)
	}
	want := orderID + `,2026-09-25T04:05:06Z,cvs_711,cvs_711,"Chen, ""Amy""",0912345678,TW,,,,,,seven,000123,Shop,2 St,SKU-1×2; SKU-2×1,1100,TWD,merchant_attested`
	if lines[1] != want {
		t.Fatalf("row:\n got %s\nwant %s", lines[1], want)
	}
	if !strings.Contains(lines[1], ",000123,") {
		t.Fatal("pickup code lost its leading zeroes")
	}
	// Header only for an empty export.
	if empty := csvOf(t); strings.Count(empty, "\r\n") != 1 {
		t.Fatalf("empty export: %q", empty)
	}
	// Embedded CR and LF stay inside the quotes byte for byte (encoding/csv would drop the CR).
	r := exportRow()
	r.Line1 = "a\r\nb\nc\rd"
	if got = csvOf(t, r); !strings.Contains(got, "\"a\r\nb\nc\rd\"") {
		t.Fatalf("multiline cell: %q", got)
	}
}

func TestCSVFormulaGuardAndPhone(t *testing.T) {
	for cell, want := range map[string]string{
		"=1+1": "'=1+1", "+cmd": "'+cmd", "-2": "'-2", "@SUM(A1)": "'@SUM(A1)",
		" =1": "' =1", "   +1": "'   +1", "\t=1": "'\t=1", "\rx": "'\rx", "\nx": "'\nx", "  \t@x": "'  \t@x",
		"plain": "plain", "a=b": "a=b", "": "", "Ann-Marie": "Ann-Marie", "1-2": "1-2",
	} {
		if got := guardFormula(cell); got != want {
			t.Errorf("guard(%q)=%q want %q", cell, got, want)
		}
	}
	// Injection prefixes end up in the file; multi-line ones are quoted around the apostrophe.
	for _, cell := range []string{"=HYPERLINK(\"x\")", " +1", "\n=1"} {
		r := exportRow()
		r.RecipientName = cell
		got := csvOf(t, r)
		if !strings.Contains(got, "'"+strings.ReplaceAll(cell, `"`, `""`)) {
			t.Errorf("cell %q not guarded in %q", cell, got)
		}
	}
	for phone, want := range map[string]string{
		"+886912345678": "0912345678", "+886 912-345-678": "0912345678", "+8860912345678": "0912345678", "0912-345-678": "0912345678",
		"+81 90-1234-5678": "819012345678", "(02) 1234 5678": "0212345678", "+1 (415) 555-0100": "14155550100", "+886": "0", "": "",
	} {
		if got := exportPhone(phone); got != want {
			t.Errorf("phone %q => %q want %q", phone, got, want)
		}
		if guardFormula(exportPhone(phone)) != exportPhone(phone) {
			t.Errorf("phone %q got a guard prefix", phone)
		}
	}
}

func exportJSONRow(i int) map[string]any {
	return map[string]any{"order_id": fmt.Sprintf("33333333-3333-4333-8333-%012d", i), "created_at_utc": "2026-09-25T04:05:06Z",
		"service_code": "home", "destination_kind": "home", "recipient_name": "Buyer", "phone": "+886900000001", "country": "TW",
		"region": "", "city": "Taipei", "postal_code": "", "line1": "3 Main St", "line2": "", "pickup_namespace": "", "pickup_code": "",
		"pickup_name": "", "pickup_address": "", "items": []any{map[string]any{"code": "SKU-1", "quantity": 2}, map[string]any{"code": "SKU-2", "quantity": 1}},
		"total_minor": 110, "currency": "TWD", "pickup_source": ""}
}

func exportRows(n int) []any {
	rows := make([]any, n)
	for i := range rows {
		rows[i] = exportJSONRow(i + 1)
	}
	return rows
}

func TestExportUnshippedTruncationAndFences(t *testing.T) {
	for _, tc := range []struct {
		rows      int
		exported  int
		truncated bool
	}{{0, 0, false}, {1, 1, false}, {1000, 1000, false}, {1001, 1000, true}} {
		ex := &exportTx{result: raw(exportRows(tc.rows))}
		got, err := ExportUnshipped(context.Background(), ex, scope, token)
		if err != nil || got.Rows != tc.exported || got.Truncated != tc.truncated || ex.calls != 3 {
			t.Fatalf("rows=%d: %+v calls=%d err=%v", tc.rows, got, ex.calls, err)
		}
		if ex.args[2] != 1001 {
			t.Fatalf("SQL asked for %v rows, want 1001 to detect truncation", ex.args[2])
		}
		if n := strings.Count(string(got.Body), "\r\n"); n != tc.exported+1 {
			t.Fatalf("rows=%d: %d CRLF lines", tc.rows, n)
		}
		if tc.rows > 0 && !strings.Contains(string(got.Body), "SKU-1×2; SKU-2×1,110,TWD") {
			t.Fatalf("items/total column: %q", got.Body)
		}
	}
	for code, expected := range map[string]error{"PT401": platform.ErrUnauthorized, "PT403": platform.ErrForbidden, "PT404": platform.ErrScopeNotFound, "PT503": ErrUnavailable} {
		_, err := ExportUnshipped(context.Background(), &exportTx{err: &pgconn.PgError{Code: code}}, scope, token)
		if !errors.Is(err, expected) {
			t.Errorf("%s => %v", code, err)
		}
	}
	if _, err := ExportUnshipped(context.Background(), &exportTx{}, scope, "short"); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("short token: %v", err)
	}
	bad := exportJSONRow(1)
	bad["extra"] = "x"
	missing := exportJSONRow(1)
	delete(missing, "line2")
	badItem := exportJSONRow(1)
	badItem["items"] = []any{map[string]any{"code": "bad code!", "quantity": 1}}
	noItems := exportJSONRow(1)
	noItems["items"] = []any{}
	for name, row := range map[string]map[string]any{"extra key": bad, "missing key": missing, "bad item": badItem, "no items": noItems} {
		if _, err := ExportUnshipped(context.Background(), &exportTx{result: raw([]any{row})}, scope, token); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ExportUnshipped(context.Background(), &exportTx{result: raw(exportRows(1002))}, scope, token); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("more than 1001 rows must fail: %v", err)
	}
}

// exportTx answers the export statement first, then two resolve_access fences (orders:export, orders:read).
type exportTx struct {
	recordTx
	result []byte
	err    error
}

func (t *exportTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if t.calls == 0 {
		t.calls++
		t.args = args
		if !strings.Contains(query, "identity.export_unshipped_orders") {
			panic("unexpected query " + query)
		}
		return fakeRow{func(dest ...any) error {
			if t.err != nil {
				return t.err
			}
			*dest[0].(*[]byte) = t.result
			return nil
		}}
	}
	t.calls++
	return t.recordTx.authRow(query)
}
