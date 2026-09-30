package foundation_test

// TCV09 TestCvsNoIframeAndPrivacy (contracts/taiwan-cvs-logistics-v1.md §10 TCV09, §5.2 "no iframe, no target", §7.1 "no logging of request/response
// bodies, keys, recipient data, trade numbers or codes", PROCESS section 5, I11). Tier REVIEW + regression (source guards; no database).
// The reviewer/security_reviewer verdicts of TCV09 are not this test's. Runtime leak scans are in TCV03 (Location/body), TCV05 (operations, job args,
// events, captured process logs) and TCV06 (recipient sentinel); this file guards the source so a leak cannot be reintroduced silently.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const tcgBase = "00c1d94" // the branch base of the R2 CVS lane: "frame-src unchanged" is measured against it

func tcgRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func tcgRead(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(b)
}

func tcgWalk(t *testing.T, root string, dirs []string, keep func(rel string) bool) []string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		_ = filepath.Walk(filepath.Join(root, d), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if info.IsDir() {
				if base := info.Name(); base == "node_modules" || base == ".next" || base == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if keep(rel) {
				out = append(out, rel)
			}
			return nil
		})
	}
	return out
}

// codeLines returns the non-comment lines of a Go/TS source with their 1-based numbers (a line is dropped when it is a comment line; a trailing
// comment is cut).
func codeLines(src string) map[int]string {
	out := map[int]string{}
	for i, l := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "*") || strings.HasPrefix(trim, "/*") {
			continue
		}
		if k := strings.Index(l, "//"); k >= 0 && !strings.Contains(l[:k], `"`) {
			l = l[:k]
		}
		out[i+1] = l
	}
	return out
}

func TestCvsNoIframeAndPrivacy(t *testing.T) {
	root := tcgRoot(t)

	t.Run("CSP frame-src is unchanged and no ECPay frame exists", func(t *testing.T) {
		frameSrc := regexp.MustCompile(`frame-src[^;"]*`)
		for _, app := range []string{"admin", "storefront"} {
			rel := "apps/" + app + "/next.config.ts"
			now := tcgRead(t, root, rel)
			base, err := exec.Command("git", "-C", root, "show", tcgBase+":"+rel).Output()
			if err != nil {
				t.Errorf("NOT_RUN: cannot read %s at the base %s: %v", rel, tcgBase, err)
				continue
			}
			if got, want := frameSrc.FindAllString(now, -1), frameSrc.FindAllString(string(base), -1); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("%s: frame-src directives changed: %v (base %v)", rel, got, want)
			}
			for _, d := range frameSrc.FindAllString(now, -1) {
				if strings.Contains(strings.ToLower(d), "ecpay") {
					t.Errorf("%s: a frame-src allows ECPay: %s", rel, d)
				}
			}
			if !strings.Contains(now, `key: "X-Frame-Options", value: "DENY"`) {
				t.Errorf("%s: X-Frame-Options DENY was removed", rel)
			}
			// the top-level map POST needs form-action entries; they must be the two exact map endpoints, never a wildcard host
			for _, m := range regexp.MustCompile(`https?://[^\s"';]*ecpay[^\s"';]*|\*\.ecpay[^\s"';]*`).FindAllString(now, -1) {
				if m != "https://logistics-stage.ecpay.com.tw/Express/map" && m != "https://logistics.ecpay.com.tw/Express/map" {
					t.Errorf("%s: an ECPay CSP source other than the two map endpoints: %s", rel, m)
				}
			}
		}
		if s := tcgRead(t, root, "apps/storefront/next.config.ts"); !strings.Contains(s, "https://logistics-stage.ecpay.com.tw/Express/map") || !strings.Contains(s, "https://logistics.ecpay.com.tw/Express/map") {
			t.Error("the storefront form-action must allow exactly the stage and production map endpoints (top-level form POST)")
		}
	})

	t.Run("only the ecpay package names ECPay hosts in code; no iframe, target or window.open in the map picker", func(t *testing.T) {
		goFiles := tcgWalk(t, root, []string{"internal", "cmd"}, func(rel string) bool {
			return strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") && !strings.Contains(rel, "internal/integrations/shipping/ecpay/")
		})
		for _, rel := range goFiles {
			for n, l := range codeLines(tcgRead(t, root, rel)) {
				if strings.Contains(l, "ecpay.com.tw") {
					t.Errorf("%s:%d dials or names an ECPay host outside the ecpay package: %q", rel, n, strings.TrimSpace(l))
				}
			}
		}
		// The apps never dial ECPay: a host string may appear only as an allowlist that validates the form action the Go API returns
		// (storefront lib/cvs-contract.ts, admin lib/logistics-model.ts); any fetch()/XMLHttpRequest naming ECPay is refused. The iframe and
		// window.open guard covers the CVS files (the wider buyer app has its own, unrelated window.open in payment code).
		tsFiles := tcgWalk(t, root, []string{"apps"}, func(rel string) bool {
			return (strings.HasSuffix(rel, ".ts") || strings.HasSuffix(rel, ".tsx")) && !strings.Contains(rel, "/tests/") && !strings.HasSuffix(rel, "next.config.ts")
		})
		for _, rel := range tsFiles {
			low := strings.ToLower(filepath.Base(rel))
			cvsFile := strings.Contains(low, "cvs") || strings.Contains(low, "ecpay")
			for n, l := range codeLines(tcgRead(t, root, rel)) {
				if strings.Contains(l, "ecpay.com.tw") && regexp.MustCompile(`fetch\(|XMLHttpRequest|axios`).MatchString(l) {
					t.Errorf("%s:%d dials an ECPay host from app code: %q", rel, n, strings.TrimSpace(l))
				}
				if cvsFile && (strings.Contains(l, "<iframe") || strings.Contains(l, "window.open")) {
					t.Errorf("%s:%d %q: no iframe / window.open in the CVS files (the admin print tab is opened by an anchor/form, not window.open)", rel, n, strings.TrimSpace(l))
				}
			}
		}
		picker := tcgRead(t, root, "apps/storefront/components/CvsPickup.tsx")
		for n, l := range codeLines(picker) {
			// the only legitimate `target` is on an official store-search anchor (checked below); the map form never carries one
			if regexp.MustCompile(`form\.target|\.target\s*=[^=]|setAttribute\(\s*["']target|target:\s*["']`).MatchString(l) {
				t.Errorf("CvsPickup.tsx:%d sets a target on the map form: %q (same tab only, F4)", n, strings.TrimSpace(l))
			}
		}
		// each anchor with target="_blank" carries rel noopener noreferrer
		for _, m := range regexp.MustCompile(`<a [^>]*target="_blank"[^>]*>`).FindAllString(picker, -1) {
			if !strings.Contains(m, `rel="noopener noreferrer"`) {
				t.Errorf("an anchor opens a new tab without rel=noopener noreferrer: %s", m)
			}
		}
		if !regexp.MustCompile(`form\.submit\(\)`).MatchString(picker) {
			t.Error("CvsPickup.tsx must submit the map form top-level (form.submit())")
		}
		if regexp.MustCompile(`form\.setAttribute\(\s*["']target`).MatchString(picker) {
			t.Error("the map form must not set a target attribute")
		}
	})

	t.Run("no key, recipient or trade-number identifier in a logging statement of the CVS code", func(t *testing.T) {
		logging := regexp.MustCompile(`slog\.|\blog\.(Print|Fatal|Panic)|fmt\.(Print|Fprint)|\.Logger\b|logger\.`)
		forbidden := regexp.MustCompile(`(?i)hashkey|hashiv|hash_key|hash_iv|MerchantTradeNo|CVSPaymentNo|CVSValidationNo|ReceiverName|ReceiverCellPhone|SenderCellPhone|recipient|\bphone\b|snapshot|AllPayLogisticsID|LogisticsID`)
		files := tcgWalk(t, root, []string{"internal/integrations/shipping/ecpay", "internal/fulfillment", "internal/checkout", "internal/buyerhttp", "internal/httpapi"}, func(rel string) bool {
			base := filepath.Base(rel)
			return strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") && !strings.Contains(rel, "/ecpaytest/") &&
				(strings.HasPrefix(base, "cvs") || strings.Contains(rel, "internal/integrations/shipping/ecpay/"))
		})
		if len(files) < 6 {
			t.Fatalf("CVS source set suspiciously small: %v", files)
		}
		for _, rel := range files {
			for n, l := range codeLines(tcgRead(t, root, rel)) {
				if logging.MatchString(l) && forbidden.MatchString(l) {
					t.Errorf("%s:%d logs a forbidden identifier: %q", rel, n, strings.TrimSpace(l))
				}
			}
		}
	})

	t.Run("PROCESS section 5: package comments and app file headers", func(t *testing.T) {
		for _, dir := range []string{"internal/integrations/shipping/ecpay", "internal/integrations/shipping/ecpay/ecpayroute", "internal/integrations/shipping/ecpay/ecpaytest"} {
			found := false
			entries, _ := os.ReadDir(filepath.Join(root, dir))
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
					if regexp.MustCompile(`(?m)^// Package \w+ (owns|is)`).MatchString(tcgRead(t, root, dir+"/"+e.Name())) {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("%s has no `// Package x owns ...` comment", dir)
			}
		}
		// every CVS-named app file starts with a comment naming its BFF route or Go endpoint
		apps := tcgWalk(t, root, []string{"apps"}, func(rel string) bool {
			low := strings.ToLower(filepath.Base(rel))
			// route and component files (PROCESS section 5 names "route/component"); pure data/copy modules are exempt
			return (strings.HasSuffix(rel, ".tsx") || strings.HasSuffix(rel, "route.ts")) && !strings.Contains(rel, "/tests/") && (strings.Contains(low, "cvs") || strings.Contains(low, "ecpay") || strings.Contains(rel, "/cvs"))
		})
		if len(apps) == 0 {
			t.Error("no CVS app files found")
		}
		for _, rel := range apps {
			src := tcgRead(t, root, rel)
			head := src
			if len(head) > 1500 {
				head = head[:1500]
			}
			body := strings.TrimSpace(src)
			for strings.HasPrefix(body, `"use client"`) || strings.HasPrefix(body, `'use client'`) { // a directive must stay first
				body = strings.TrimSpace(body[strings.Index(body, "\n")+1:])
			}
			head = body
			if len(head) > 1500 {
				head = head[:1500]
			}
			first := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
			if !(strings.HasPrefix(first, "//") || strings.HasPrefix(first, "/*")) {
				t.Errorf("%s does not start with a comment (PROCESS section 5, Frontend)", rel)
				continue
			}
			if !strings.Contains(head, "/api/") && !strings.Contains(head, "/v1/") {
				t.Errorf("%s: the header comment names neither a BFF route (/api/...) nor a Go endpoint (/v1/...)", rel)
			}
		}
		// the Go CVS files start with a comment as well
		for _, rel := range tcgWalk(t, root, []string{"internal/fulfillment", "internal/checkout", "internal/buyerhttp", "internal/httpapi"}, func(rel string) bool {
			base := filepath.Base(rel)
			return strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") && strings.HasPrefix(base, "cvs")
		}) {
			if first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(tcgRead(t, root, rel)), "\n", 2)[0]); !strings.HasPrefix(first, "//") {
				t.Errorf("%s does not start with a comment", rel)
			}
		}
	})
}
