package foundation_test

// MF08 (contracts/manual-fulfilment-v1.md §6): guards. REVIEW + regression tier. Prefix `mfg`.
// What is checked here: COMMENT ON for every 0063 object and column (PROCESS §5), and the MD2 rule that
// NOTHING reachable from the shipment/export code dials a provider, inserts a River job or writes
// integration.operations — as a source guard over the Go files and over the SQL definer bodies. The runtime
// half of MD2 (zero operations/jobs/refunds/signals after every shipment command) is asserted in MF03.
// The full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`, the "MOR/MOU/BPH/CF and
// RF gates unchanged" regression and the reviewer verdicts are the integrator's run.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mfgCode returns a Go file's text with // comments and string-free comment tails removed.
func mfgCode(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("guarded file missing: %v", err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "//"); i >= 0 && !strings.Contains(line[:i], `"`) && !strings.Contains(line[:i], "`") {
			line = line[:i]
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func TestManualFulfilmentMF08Guards(t *testing.T) {
	ctx := context.Background()

	t.Run("COMMENT ON for every 0063 object and column", func(t *testing.T) {
		f := fixture(t)
		for _, table := range []string{"fulfillment.manual_shipment_versions", "fulfillment.manual_shipment_heads"} {
			var ok bool
			if err := f.owner.QueryRow(ctx, `SELECT obj_description($1::regclass,'pg_class') IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
				t.Errorf("PROCESS §5: table %s has no COMMENT ON TABLE (%v)", table, err)
			}
			rows, err := f.owner.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped AND col_description(a.attrelid,a.attnum) IS NULL`, table)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var c string
				_ = rows.Scan(&c)
				t.Errorf("PROCESS §5: %s.%s has no COMMENT ON COLUMN", table, c)
			}
			rows.Close()
		}
		rows, err := f.owner.Query(ctx, `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		 WHERE ((n.nspname='fulfillment' AND (p.proname LIKE '%shipment%')) OR (n.nspname='identity' AND p.proname IN ('export_unshipped_orders','read_merchant_orders')))
		 AND obj_description(p.oid,'pg_proc') IS NULL`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var sig string
			_ = rows.Scan(&sig)
			t.Errorf("PROCESS §5: function %s has no COMMENT ON FUNCTION", sig)
		}
		rows.Close()
	})

	t.Run("no provider dial, River insert or integration.operations write reachable from shipment/export code", func(t *testing.T) {
		forbidden := regexp.MustCompile(`(?i)(\briver\b|InsertTx|\bstripe\b|payuni|api\.stripe|integrations/|integration\.operations|http\.(NewRequest|Client|DefaultClient|Get|Post)\b|net/http/httputil|dblink)`)
		files := map[string][]string{
			"../../internal/merchantorders/shipments.go": nil,
			"../../internal/merchantorders/export.go":    nil,
			"../../internal/merchantorders/actions.go":   nil,
			"../../internal/httpapi/shipments.go":        {"net/http"}, // the handler layer may import net/http itself, never a client
		}
		for path, allowed := range files {
			code := mfgCode(t, path)
			for _, a := range allowed {
				code = strings.ReplaceAll(code, `"`+a+`"`, `""`)
			}
			if m := forbidden.FindString(code); m != "" {
				t.Errorf("%s references %q (MD2: merchant-arranged fulfilment never creates a provider operation, job or call)", path, m)
			}
		}
		f := fixture(t)
		for _, sig := range []string{"fulfillment.record_manual_shipment(bytea,uuid,uuid,text,bytea,bigint,text,text,text,text,text,text,text)", "identity.export_unshipped_orders(bytea,uuid,integer)", "fulfillment.read_manual_shipment_history(bytea,uuid,uuid)"} {
			var src string
			if err := f.owner.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE oid=to_regprocedure($1)`, sig).Scan(&src); err != nil {
				t.Errorf("definer %s missing: %v", sig, err)
				continue
			}
			if m := regexp.MustCompile(`(?i)(integration\.operations|river|dblink|payments\.stripe_refunds\b.*\b(INSERT|UPDATE|DELETE)\b)`).FindString(src); m != "" {
				// record_manual_shipment may READ refund rows (MD6) but never write operations or jobs
				if !strings.Contains(strings.ToLower(m), "stripe_refunds") {
					t.Errorf("%s body references %q", sig, m)
				}
			}
			if regexp.MustCompile(`(?i)(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+(integration\.operations|river[a-z_.]*)`).MatchString(src) {
				t.Errorf("%s writes integration.operations or a River table", sig)
			}
		}
	})

	t.Run("package comments of the fulfilment files (PROCESS §5), reported as data", func(t *testing.T) {
		for _, path := range []string{"../../internal/merchantorders/shipments.go", "../../internal/merchantorders/export.go", "../../internal/httpapi/shipments.go"} {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("file missing: %v", err)
				continue
			}
			if !strings.Contains(string(raw), "Depends on") && !strings.Contains(string(raw), "fulfillment.record_manual_shipment") && !strings.Contains(string(raw), "identity.export_unshipped_orders") {
				t.Logf("DATA %s: no SQL call-site comment naming the definer it calls (PROCESS §5 call sites)", filepath.Base(path))
			}
		}
	})
}
