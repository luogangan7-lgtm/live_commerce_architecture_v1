package foundation_test

// RF12 (contracts/stripe-refund-v1.md §4.4, §4.6, §9, PROCESS §5): guards. Prefix `srg`.
//
// REVIEW + regression tier. What this test can do without owning the implementation:
//   1. every B1 function replaced by 0062 / post_river 0013 is compared with its LAST pre-0062
//      definition (extracted from the immutable migration text) — old/new sha256 recorded, the
//      one-line delta of apply_stripe_observation asserted exactly, and for the others the
//      "nothing checkout-related disappeared" rule (every schema-qualified name the old body used is
//      still used, roles/owner/search_path unchanged). The unified diff goes to the evidence file.
//   2. source guard: only internal/integrations/psp/stripe dials Stripe.
//   3. COMMENT ON for every 0062 object and column (PROCESS §5), package-comment shape for the
//      packages that already conform (the others are listed as data, not failed: pre-existing).
//   4. a MOCK refund run whose logs and DB-wide scan must carry no key, whsec, body or ARN.
// The full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py` and the reviewer
// verdicts are the integrator's run (contract: "Full-suite regression and reviewer verdicts").

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/psp/stripe/stripetest"
)

// srgOld returns the last definition of name before 0062/post_river 0013, and where it came from.
func srgOld(t *testing.T, name string) (body, file string) {
	t.Helper()
	return srgOldUntil(t, name, func(base string, post bool) bool {
		return strings.HasPrefix(base, "0062_") || (post && base >= "0013_") || (!post && base >= "0062_")
	})
}

// srgOldUntil is srgOld with an explicit cut-off: files for which stop(base, isPostRiver) is true are
// ignored (they are the change under test and everything after it).
func srgOldUntil(t *testing.T, name string, stop func(base string, post bool) bool) (body, file string) {
	t.Helper()
	root := "../../migrations"
	var files []string
	for _, pattern := range []string{"[0-9][0-9][0-9][0-9]_*.sql", "post_river/[0-9][0-9][0-9][0-9]_*.sql"} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(m)
		files = append(files, m...)
	}
	def := regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	as := regexp.MustCompile(`(?is)\bAS\s+(\$[A-Za-z_]*\$)`)
	for _, f := range files {
		base := filepath.Base(f)
		if stop(base, strings.Contains(f, "post_river")) {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		for _, m := range def.FindAllStringIndex(s, -1) {
			a := as.FindStringSubmatchIndex(s[m[1]:])
			if a == nil {
				continue
			}
			tag := s[m[1]+a[2] : m[1]+a[3]]
			start := m[1] + a[1]
			end := strings.Index(s[start:], tag)
			if end < 0 {
				continue
			}
			body, file = s[start:start+end], f
		}
	}
	if body == "" {
		t.Fatalf("no pre-0062 definition of %s found in the migrations", name)
	}
	return body, file
}

func srgLines(body string) (out []string) {
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return
}

// srgDiff returns the lines only in a (removed) and only in b (added) by LCS.
func srgDiff(a, b []string) (removed, added []string, unified string) {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var u strings.Builder
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			removed = append(removed, a[i])
			fmt.Fprintf(&u, "- %s\n", a[i])
			i++
		default:
			added = append(added, b[j])
			fmt.Fprintf(&u, "+ %s\n", b[j])
			j++
		}
	}
	for ; i < n; i++ {
		removed = append(removed, a[i])
		fmt.Fprintf(&u, "- %s\n", a[i])
	}
	for ; j < m; j++ {
		added = append(added, b[j])
		fmt.Fprintf(&u, "+ %s\n", b[j])
	}
	return removed, added, u.String()
}

var srgQualified = regexp.MustCompile(`\b(?:payments|integration|checkout|inventory|fulfillment|identity|ops|river_payment|river)\.[a-z_]+\b`)

func srgNames(body string) map[string]bool {
	out := map[string]bool{}
	for _, n := range srgQualified.FindAllString(body, -1) {
		out[n] = true
	}
	return out
}

func TestStripeRF12Guards(t *testing.T) {
	ctx := context.Background()
	evidence := &strings.Builder{}
	defer func() {
		if dir := os.Getenv("LC_EVIDENCE_DIR"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, "rf12-replaced-functions.txt"), []byte(evidence.String()), 0o644)
		}
	}()

	t.Run("replaced B1 functions differ from their last definition only by the stated delta", func(t *testing.T) {
		f := fixture(t)
		type fn struct{ schema, name string }
		for _, x := range []fn{{"payments", "stripe_webhook_prepare"}, {"payments", "stripe_webhook_commit"}, {"payments", "guard_stripe_receipt_link"},
			{"integration", "load_stripe_signal"}, {"integration", "consume_stripe_signal"}, {"integration", "payment_job_queue"},
			{"payments", "apply_stripe_observation"}, {"payments", "apply_capture"}} {
			qualified := x.schema + "." + x.name
			oldBody, file := srgOld(t, qualified)
			rows, err := f.owner.Query(ctx, `SELECT p.prosrc,pg_get_userbyid(p.proowner),p.proconfig::text,
			 coalesce((SELECT string_agg(pg_get_userbyid(a.grantee),',' ORDER BY pg_get_userbyid(a.grantee)) FROM aclexplode(p.proacl) a WHERE a.privilege_type='EXECUTE' AND a.grantee<>p.proowner AND a.grantee<>0),''),
			 EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE')
			 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.proname=$2`, x.schema, x.name)
			if err != nil {
				t.Fatal(err)
			}
			var newBody, owner, config, grants string
			var publicExec bool
			count := 0
			for rows.Next() {
				count++
				if err := rows.Scan(&newBody, &owner, &config, &grants, &publicExec); err != nil {
					t.Fatal(err)
				}
			}
			rows.Close()
			if count != 1 {
				t.Errorf("%s: %d overloads after 0062 (DROP + CREATE must leave exactly one)", qualified, count)
				continue
			}
			oldSum, newSum := sha256.Sum256([]byte(oldBody)), sha256.Sum256([]byte(newBody))
			removed, added, unified := srgDiff(srgLines(oldBody), srgLines(newBody))
			fmt.Fprintf(evidence, "== %s (old from %s)\nold sha256 %s\nnew sha256 %s\nremoved %d added %d\n%s\n", qualified, file, hex.EncodeToString(oldSum[:]), hex.EncodeToString(newSum[:]), len(removed), len(added), unified)
			if oldSum == newSum {
				t.Errorf("%s: body unchanged: 0062 was required to replace it", qualified)
			}
			// nothing checkout-related disappeared
			newNames := srgNames(newBody)
			for name := range srgNames(oldBody) {
				if !newNames[name] {
					t.Errorf("%s: the old body used %s and the new one no longer does (behaviour dropped)", qualified, name)
				}
			}
			// owner, ACL, search_path preserved (from the B1 migration text)
			raw := readMigrationText(t)
			if m := regexp.MustCompile(`(?is)ALTER FUNCTION\s+`+regexp.QuoteMeta(qualified)+`\s*\([^)]*\)\s+OWNER TO\s+(\w+)`).FindAllStringSubmatch(raw, -1); len(m) > 0 {
				if want := m[len(m)-1][1]; owner != want {
					t.Errorf("%s: owner %s, B1 owner %s", qualified, owner, want)
				}
			}
			var oldGrant []string
			for _, m := range regexp.MustCompile(`(?is)GRANT EXECUTE ON FUNCTION\s+`+regexp.QuoteMeta(qualified)+`\s*\([^)]*\)\s+TO\s+([\w,\s]+?);`).FindAllStringSubmatch(raw, -1) {
				oldGrant = nil
				for _, r := range strings.Split(m[1], ",") {
					if r = strings.TrimSpace(r); r != "" {
						oldGrant = append(oldGrant, r)
					}
				}
			}
			sort.Strings(oldGrant)
			if len(oldGrant) > 0 && grants != strings.Join(oldGrant, ",") {
				t.Errorf("%s: EXECUTE granted to [%s], B1 granted [%s]", qualified, grants, strings.Join(oldGrant, ","))
			}
			if publicExec {
				t.Errorf("%s: PUBLIC can EXECUTE", qualified)
			}
			if !strings.Contains(config, "search_path=pg_catalog") {
				t.Errorf("%s: proconfig %s lost search_path=pg_catalog", qualified, config)
			}
			switch x.name {
			case "apply_stripe_observation":
				// §4.4: ONE change, the post-capture review branch runs only when v_new_capture OR v_closed_before.
				// SQL comment lines are documentation (PROCESS §5), not behaviour: the delta is judged on code lines.
				var code []string
				for _, l := range added {
					if !strings.HasPrefix(strings.TrimSpace(l), "--") {
						code = append(code, l)
					}
				}
				if len(removed) != 1 || removed[0] != "IF v_review THEN" || len(code) != 1 ||
					!strings.Contains(code[0], "v_review") || !strings.Contains(code[0], "v_new_capture") || !strings.Contains(code[0], "v_closed_before") {
					t.Errorf("apply_stripe_observation delta is not the single stated guard:\n%s", unified)
				}
			case "apply_capture":
				for _, want := range []string{"apply_stripe_refund", "apply_stripe_charge"} {
					if !strings.Contains(newBody, want) {
						t.Errorf("dispatcher does not route to %s", want)
					}
				}
				if !strings.Contains(newBody, "refund") || !strings.Contains(newBody, "charge") {
					t.Error("dispatcher does not branch on Object refund|charge")
				}
			case "stripe_webhook_prepare", "stripe_webhook_commit", "guard_stripe_receipt_link":
				if !strings.Contains(newBody, "refund_id") || !strings.Contains(newBody, "object_type") && x.name != "guard_stripe_receipt_link" {
					t.Errorf("%s does not handle refund_id / object_type", qualified)
				}
			case "load_stripe_signal", "consume_stripe_signal":
				if !strings.Contains(newBody, "PAYMENT_REFUND") || !strings.Contains(newBody, "require_stripe_refund") || !strings.Contains(newBody, "require_stripe_query") {
					t.Errorf("%s does not dispatch the fence on actor_kind PAYMENT_REFUND -> require_stripe_refund / BUYER_PAYMENT_QUERY -> require_stripe_query", qualified)
				}
			case "payment_job_queue":
				if !strings.Contains(newBody, "stripe_refunds") {
					t.Error("payment_job_queue does not resolve the attempt through payments.stripe_refunds")
				}
			}
		}
	})

	t.Run("only internal/integrations/psp/stripe dials Stripe", func(t *testing.T) {
		var offenders []string
		for _, root := range []string{"../../cmd", "../../internal"} {
			_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return err
				}
				if strings.Contains(filepath.ToSlash(path), "/internal/integrations/psp/stripe/") ||
					strings.Contains(filepath.ToSlash(path), "/internal/billing/") { // customers-billing-v1: platform billing client
					return nil
				}
				raw, _ := os.ReadFile(path)
				// Comments name hosts on purpose (PROCESS §5 external hosts, e.g. api.stripe.com); only code counts.
				var code strings.Builder
				for _, line := range strings.Split(string(raw), "\n") {
					if i := strings.Index(line, "//"); i >= 0 && !strings.Contains(line[:i], `"`) {
						line = line[:i]
					}
					code.WriteString(line + "\n")
				}
				s := code.String()
				if strings.Contains(s, "api.stripe.com") || strings.Contains(s, "stripetest") {
					offenders = append(offenders, path)
				}
				return nil
			})
		}
		if len(offenders) != 0 {
			t.Fatalf("product code outside psp/stripe references api.stripe.com or the fake: %v", offenders)
		}
		// apps/** (TypeScript) must not call the Stripe API either.
		var appOffenders []string
		_ = filepath.WalkDir("../../apps", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".next" || d.Name() == "dist") {
				return fs.SkipDir
			}
			if d.IsDir() || !(strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") || strings.HasSuffix(path, ".mjs")) || strings.Contains(path, ".test.") || strings.Contains(path, ".spec.") {
				return nil
			}
			if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "api.stripe.com") {
				appOffenders = append(appOffenders, path)
			}
			return nil
		})
		if len(appOffenders) != 0 {
			t.Fatalf("front-end code references api.stripe.com: %v", appOffenders)
		}
	})

	t.Run("COMMENT ON for every 0062 object and column", func(t *testing.T) {
		f := fixture(t)
		for _, table := range []string{"payments.stripe_refunds", "payments.refund_facts"} {
			rows, err := f.owner.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped AND col_description(a.attrelid,a.attnum) IS NULL`, table)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var c string
				_ = rows.Scan(&c)
				t.Errorf("RF12/PROCESS §5: %s.%s has no COMMENT ON COLUMN", table, c)
			}
			rows.Close()
		}
		for _, c := range []struct{ table, col string }{{"payments.stripe_signals", "refund_id"}, {"payments.stripe_webhook_receipts", "refund_id"}, {"payments.stripe_sessions", "charge_signal_count"}} {
			var ok bool
			if err := f.owner.QueryRow(ctx, `SELECT col_description($1::regclass,(SELECT attnum FROM pg_attribute WHERE attrelid=$1::regclass AND attname=$2)) IS NOT NULL`, c.table, c.col).Scan(&ok); err != nil || !ok {
				t.Errorf("RF12/PROCESS §5: new column %s.%s has no COMMENT ON COLUMN (%v)", c.table, c.col, err)
			}
		}
		// Case-sensitive, not after a letter: the Stripe refund identifiers (stripe_refunds, refund_facts,
		// p_refund ...), not PAYUNi's CardRefundType fields in the pre-0062 record_payment_query.
		rows, err := f.owner.Query(ctx, `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.prosrc ~ '(^|[^A-Za-z])refund' AND n.nspname IN ('payments','integration','identity')
		 AND obj_description(p.oid,'pg_proc') IS NULL AND p.prokind='f'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var sig string
			_ = rows.Scan(&sig)
			t.Errorf("RF12/PROCESS §5: function %s (mentions refunds) has no COMMENT ON FUNCTION", sig)
		}
		rows.Close()
	})

	t.Run("package comments (PROCESS §5)", func(t *testing.T) {
		mustConform := map[string]bool{"internal/integrations/psp/stripe/stripetest": true, "internal/payments/stripewebhook": true}
		for _, dir := range []string{"internal/integrations/psp/stripe", "internal/integrations/psp/stripe/stripetest", "internal/payments", "internal/payments/stripewebhook", "internal/merchantorders", "internal/httpapi", "internal/checkout"} {
			var doc strings.Builder
			files, _ := filepath.Glob(filepath.Join("../..", dir, "*.go"))
			for _, file := range files {
				if strings.HasSuffix(file, "_test.go") {
					continue
				}
				raw, _ := os.ReadFile(file)
				lines := strings.Split(string(raw), "\n")
				for i, l := range lines {
					if strings.HasPrefix(l, "package ") {
						for j := i - 1; j >= 0 && strings.HasPrefix(lines[j], "//"); j-- {
							doc.WriteString(lines[j] + "\n")
						}
						break
					}
				}
			}
			text := doc.String()
			missing := []string{}
			for marker, want := range map[string]string{"owns": "Package ", "never": "It never"} {
				if !strings.Contains(text, want) {
					missing = append(missing, marker)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 && mustConform[dir] {
				t.Errorf("%s: package comment lacks %v (PROCESS §5: owns / It never; internal depends/used-by is generated, R1 ruling F10)", dir, missing)
			} else if len(missing) > 0 {
				t.Logf("DATA %s: package comment lacks %v (pre-existing; reviewer decision)", dir, missing)
			}
		}
	})

	t.Run("logs and rows carry no key, whsec, body or ARN after a full MOCK refund run", func(t *testing.T) {
		logs := &swhLogBuffer{}
		oldSlog, oldLog := slog.Default(), log.Writer()
		slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		log.SetOutput(logs)
		t.Cleanup(func() { slog.SetDefault(oldSlog); log.SetOutput(oldLog) })
		e := rfxNew(t)
		a := e.stripeStore(t)
		endpoint, secret := e.endpoint(t, a)
		e.startWorker(t)
		o := e.pay(t, a, endpoint, secret)
		e.grant(t, o, "orders:read", "payments:refund")
		id := e.mustRefund(t, o, 1000, "requested_by_customer")
		e.awaitRefundFact(t, id, o.attempt, "SUCCEEDED")
		for _, body := range [][]byte{e.fake.RefundEventBody(srhEvent("srg"), "refund.updated", e.fake.RefundByRef(id), false), e.fake.ChargeEventBody(srhEvent("srg"), o.pi, false)} {
			if status := e.deliverRaw(t, endpoint, secret, body); status != 200 {
				t.Fatalf("webhook answered %d", status)
			}
		}
		e.awaitRefund(t, "webhook signals consumed", id, o.attempt, 45*time.Second, `SELECT NOT EXISTS(SELECT 1 FROM payments.stripe_signals WHERE consumed_at IS NULL AND attempt_id=(SELECT attempt_id FROM payments.stripe_refunds WHERE id=$1))`)
		needles := []string{stripetest.SentinelARN, stripetest.SentinelReceiptURL, stripetest.SentinelEmail, secret, a.secret, "destination_details", "receipt_url", "billing_details"}
		if hits := swhScan(t, e.f, needles...); len(hits) != 0 {
			t.Fatalf("sensitive strings persisted in %v", hits)
		}
		out := logs.String()
		for _, n := range append(needles, "Stripe-Signature", "whsec_", "sk_test_", "Bearer ") {
			if strings.Contains(out, n) {
				t.Fatalf("logs contain %q", n)
			}
		}
	})
}

// readMigrationText concatenates every migration up to (excluding) 0062/post_river 0013.
func readMigrationText(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, pattern := range []string{"[0-9][0-9][0-9][0-9]_*.sql", "post_river/[0-9][0-9][0-9][0-9]_*.sql"} {
		m, _ := filepath.Glob(filepath.Join("../../migrations", pattern))
		sort.Strings(m)
		for _, f := range m {
			base := filepath.Base(f)
			if strings.Contains(f, "post_river") && base >= "0013_" || !strings.Contains(f, "post_river") && base >= "0062_" {
				continue
			}
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(raw)
			b.WriteString("\n")
		}
	}
	return b.String()
}
