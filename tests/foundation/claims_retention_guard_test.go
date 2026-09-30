// claims_retention_guard_test.go is the independent CRP10 gate of U08 (contract claims-retention-purge-v1 §7 row CRP10):
// source guards that hold without a database, plus the cheap parts of the regression list (check_packet, gofmt). The
// expensive part of the regression list (KC01-15, MCI01-10, MC/MIso, CB05 when 0078 is present, full `go test -race
// ./...`, `go vet ./...`) is run as commands and logged under output/u08-claims-retention/ (see the unit return). CRP10
// is completed by a separate security_reviewer verdict, which this file is not. Evidence label: REVIEW (source scan).
package foundation_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const crRoot = "../.."

// crSource is one non-test source file of the repository (relative slash path) and its text.
type crSource struct{ path, text string }

// crFiles walks the repository directories named and returns their files (no tests, no vendored or generated trees).
func crFiles(t *testing.T, dirs []string, exts ...string) []crSource {
	t.Helper()
	var out []crSource
	for _, dir := range dirs {
		root := filepath.Join(crRoot, dir)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", ".git", ".worktrees", "output":
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, "_test.go") {
				return nil
			}
			ok := len(exts) == 0
			for _, e := range exts {
				if strings.HasSuffix(p, e) {
					ok = true
				}
			}
			if !ok {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(crRoot, p)
			out = append(out, crSource{filepath.ToSlash(rel), string(b)})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no source files found under %v: the guard would prove nothing", dirs)
	}
	return out
}

// CRP10 (REVIEW): the source guards of contract §7.
//   - definer callers: only internal/retention (non-test Go) names the §3 definers in a call.
//   - K_actor: COMMERCE_CLAIMS_ACTOR_KEY is read only by cmd/meta-worker and cmd/retention-admin (and defined in
//     internal/integrations/meta); claims-worker and every deploy service but meta-worker never mention it.
//   - operator DSN: COMMERCE_RETENTION_OPERATOR_DATABASE_URL exists only in cmd/retention-admin; it is absent from
//     deploy/**, deploy/scripts/smoke.sh and .github/workflows/*.
//   - no network and no new import in retention code (internal/retention, cmd/retention-admin).
//   - claims-worker never loads the actor key.
//   - the cheap regression items: gofmt and scripts/check_packet.py.
func TestClaimsRetentionCRP10SourceGuards(t *testing.T) {
	code := crFiles(t, []string{"internal", "cmd", "scripts", "deploy", ".github"})
	if len(code) < 200 {
		t.Fatalf("only %d source files scanned; the walk is broken", len(code))
	}
	// the source guard itself must be able to see a violation: a synthetic file is matched by the same regexes
	sample := "package x\nfunc f(){ pool.QueryRow(ctx, `SELECT claims.run_retention($1)`, 1) }\n"
	definerCall := regexp.MustCompile(`claims\.(run_retention|erase_actor|set_retention_policy|retention_status|replay_actor_erasures|apply_actor_erasure)\s*\(`)
	if !definerCall.MatchString(sample) {
		t.Fatal("the definer-call pattern does not match a call (guard cannot fail)")
	}

	t.Run("definer-callers-only-in-internal-retention", func(t *testing.T) {
		var bad []string
		callers := 0
		for _, f := range code {
			if !definerCall.MatchString(f.text) {
				continue
			}
			if strings.HasPrefix(f.path, "internal/retention/") && strings.HasSuffix(f.path, ".go") {
				callers++
				continue
			}
			bad = append(bad, f.path)
		}
		if callers == 0 {
			t.Error("internal/retention no longer calls any definer: the guard's allowed set is empty")
		}
		if len(bad) != 0 {
			sort.Strings(bad)
			t.Errorf("the §3 definers are called outside internal/retention: %v", bad)
		}
	})

	t.Run("actor-key-loaded-only-by-meta-worker-and-retention-admin", func(t *testing.T) {
		const env = "COMMERCE_CLAIMS_ACTOR_KEY"
		loadCall := regexp.MustCompile(`LoadClaimsActorKey\s*\(`)
		var readers, loaders []string
		for _, f := range code {
			if strings.Contains(f.text, env) {
				readers = append(readers, f.path)
			}
			if loadCall.MatchString(f.text) && strings.HasSuffix(f.path, ".go") {
				loaders = append(loaders, f.path)
			}
		}
		isAllowed := func(p string) bool {
			return strings.HasPrefix(p, "cmd/meta-worker/") || strings.HasPrefix(p, "cmd/retention-admin/") ||
				p == "internal/integrations/meta/claim_intake.go" || p == "deploy/compose.yml"
		}
		for _, p := range readers {
			if !isAllowed(p) {
				t.Errorf("%s mentions %s: only cmd/meta-worker and cmd/retention-admin may load K_actor", p, env)
			}
		}
		for _, p := range loaders {
			if !(strings.HasPrefix(p, "cmd/meta-worker/") || strings.HasPrefix(p, "cmd/retention-admin/") || p == "internal/integrations/meta/claim_intake.go") {
				t.Errorf("%s calls LoadClaimsActorKey", p)
			}
		}
		// both allowed loaders actually load it (the guard's allowed set is real)
		has := func(prefix string) bool {
			for _, p := range loaders {
				if strings.HasPrefix(p, prefix) {
					return true
				}
			}
			return false
		}
		if !has("cmd/meta-worker/") || !has("cmd/retention-admin/") {
			t.Errorf("K_actor loaders are %v, want cmd/meta-worker and cmd/retention-admin", loaders)
		}
		// the compose service blocks: only meta-worker carries the variable
		var compose string
		for _, f := range code {
			if f.path == "deploy/compose.yml" {
				compose = f.text
			}
		}
		block := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\s*$`)
		idx := block.FindAllStringSubmatchIndex(compose, -1)
		for i, m := range idx {
			name := compose[m[2]:m[3]]
			end := len(compose)
			if i+1 < len(idx) {
				end = idx[i+1][0]
			}
			if strings.Contains(compose[m[0]:end], env) && name != "meta-worker" {
				t.Errorf("deploy/compose.yml service %s carries %s (only meta-worker may)", name, env)
			}
		}
		// claims-worker never loads it (Go source)
		for _, f := range code {
			if strings.HasPrefix(f.path, "cmd/claims-worker/") && (strings.Contains(f.text, env) || loadCall.MatchString(f.text) || strings.Contains(f.text, "ClaimsActorKey")) {
				t.Errorf("%s loads or names the actor key", f.path)
			}
		}
	})

	t.Run("operator-dsn-name-only-in-retention-admin", func(t *testing.T) {
		const name = "COMMERCE_RETENTION_OPERATOR_DATABASE_URL"
		all := crFiles(t, []string{"internal", "cmd", "scripts", "deploy", ".github"})
		used := false
		for _, f := range all {
			if !strings.Contains(f.text, name) {
				continue
			}
			if strings.HasPrefix(f.path, "cmd/retention-admin/") {
				used = true
				continue
			}
			t.Errorf("%s references %s (deploy/**, deploy/scripts/smoke.sh, .github/workflows/* and every service must never hold the operator DSN)", f.path, name)
		}
		if !used {
			t.Error("cmd/retention-admin no longer reads the operator DSN: the guard's allowed set is empty")
		}
		for _, prefix := range []string{"deploy/", ".github/workflows/"} {
			for _, f := range all {
				if strings.HasPrefix(f.path, prefix) && strings.Contains(f.text, name) {
					t.Errorf("%s contains the operator DSN name", f.path)
				}
			}
		}
	})

	t.Run("retention-code-has-no-network-and-no-new-import", func(t *testing.T) {
		forbidden := map[string]bool{"net": true, "net/http": true, "net/http/httptest": true, "net/rpc": true, "net/smtp": true, "net/textproto": true,
			"net/mail": true, "crypto/tls": true, "os/exec": true, "plugin": true, "unsafe": true, "syscall": false}
		thirdParty := []string{"github.com/jackc/pgx/v5", "github.com/riverqueue/river"}
		internalAllowed := map[string]bool{"livecommerce/internal/platform": true, "livecommerce/internal/integrations/meta": true, "livecommerce/internal/retention": true}
		checked := 0
		for _, f := range crFiles(t, []string{"internal/retention", "cmd/retention-admin"}, ".go") {
			file, err := parser.ParseFile(token.NewFileSet(), f.path, f.text, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", f.path, err)
			}
			checked++
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				switch {
				case forbidden[path]:
					t.Errorf("%s imports %s (no network / process code in retention)", f.path, path)
				case strings.HasPrefix(path, "livecommerce/"):
					if !internalAllowed[path] {
						t.Errorf("%s imports the internal package %s (allowed: %v)", f.path, path, keys(internalAllowed))
					}
				case strings.Contains(strings.SplitN(path, "/", 2)[0], "."):
					ok := false
					for _, p := range thirdParty {
						if strings.HasPrefix(path, p) {
							ok = true
						}
					}
					if !ok {
						t.Errorf("%s imports the new third-party package %s", f.path, path)
					}
				}
			}
		}
		if checked < 4 {
			t.Errorf("only %d retention Go files checked", checked)
		}
		// the same scan detects a violation
		bad, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\nimport \"net/http\"\n", parser.ImportsOnly)
		if err != nil || len(bad.Imports) != 1 || !forbidden[strings.Trim(bad.Imports[0].Path.Value, `"`)] {
			t.Error("the import guard cannot see net/http")
		}
	})

	t.Run("regression-cheap-items", func(t *testing.T) {
		gofmt := exec.Command("gofmt", "-l", "internal/retention", "cmd/retention-admin", "cmd/claims-worker", "internal/platform", "internal/integrations/meta", "tests/foundation")
		gofmt.Dir = crRoot
		out, err := gofmt.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("gofmt -l: %v %q", err, out)
		}
		// check_packet.py rewrites the tracked experiments/results/packet-check.json on every run: restore it so the guard
		// leaves the working tree as it found it.
		resultFile := filepath.Join(crRoot, "experiments", "results", "packet-check.json")
		if keep, err := os.ReadFile(resultFile); err == nil {
			t.Cleanup(func() { _ = os.WriteFile(resultFile, keep, 0o644) })
		}
		pkt := exec.Command("python3", "scripts/check_packet.py")
		pkt.Dir = crRoot
		if out, err := pkt.CombinedOutput(); err != nil {
			t.Errorf("python3 scripts/check_packet.py: %v\n%s", err, out)
		}
		if matches, _ := filepath.Glob(filepath.Join(crRoot, "migrations", "0078_*.sql")); len(matches) == 0 {
			t.Log("migrations/0078 (customers-billing) is not in this lane: CB05 is NOT_RUN here (run it on the merged release branch)")
		}
	})
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
