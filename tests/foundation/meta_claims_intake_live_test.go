// T10c meta claims intake gate MCI11 (LIVE, read-only probes, contracts/meta-claims-intake-v1.md §12),
// written by the independent test_worker. It is skipped unless META_LIVE_PAGE_ID and
// META_LIVE_PAGE_TOKEN are set, which the R1 harness never does: the evidence label of this file in R1
// is NOT_RUN (a skip is never PASS).
//
// Rules the probe enforces on itself: GET only (the HTTP transport refuses every other method, so a
// programming error cannot send a message), the token never appears in a URL, a log line or a failure
// message, Graph version comes from META_LIVE_GRAPH_VERSION (U5: no default is assumed), and every
// probe result is reported by name. MCI12 (the one owner-approved private reply) is deliberately not
// written: it needs O1-O5 and a per-send approval in chat.
//
// External: graph.facebook.com (https://developers.facebook.com/docs/graph-api/, retrieved 2026-09-29 for the
// read endpoints only).
package foundation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type mciGetOnly struct{ next http.RoundTripper }

func (g mciGetOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("MCI11 is read-only: refusing %s %s", r.Method, r.URL.Path)
	}
	return g.next.RoundTrip(r)
}

func TestMetaClaimsMCI11LiveReadOnlyProbes(t *testing.T) {
	page, token, version := os.Getenv("META_LIVE_PAGE_ID"), os.Getenv("META_LIVE_PAGE_TOKEN"), os.Getenv("META_LIVE_GRAPH_VERSION")
	if page == "" || token == "" {
		t.Skip("NOT_RUN: META_LIVE_PAGE_ID and META_LIVE_PAGE_TOKEN are not set (no Page token in the harness, owner input O4)")
	}
	if !regexp.MustCompile(`^[0-9]{1,40}$`).MatchString(page) || !regexp.MustCompile(`^v[0-9]{1,3}\.[0-9]{1,2}$`).MatchString(version) {
		t.Fatal("META_LIVE_PAGE_ID must be numeric and META_LIVE_GRAPH_VERSION (U5) must be an explicit vNN.N; no default is assumed")
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: mciGetOnly{http.DefaultTransport}}
	get := func(path string, query url.Values) (int, map[string]any) {
		t.Helper()
		u := url.URL{Scheme: "https", Host: "graph.facebook.com", Path: "/" + version + "/" + path, RawQuery: query.Encode()}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
		if err != nil {
			t.Fatal("build probe request")
		}
		req.Header.Set("Authorization", "Bearer "+token) // U6: header form; the token is never placed in the URL
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("probe %s failed at the transport (details withheld): %T", path, err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		if strings.Contains(string(body), token) {
			t.Fatalf("probe %s: Graph echoed the token; not printing it", path)
		}
		return res.StatusCode, out
	}
	report := map[string]any{"graph_version": version}

	// U6: does Graph accept the Page token in an Authorization: Bearer header?
	status, me := get("me", url.Values{"fields": {"id"}})
	report["U6_bearer_header_status"] = status
	if status != 200 || me["id"] != page {
		t.Errorf("U6: Bearer-header token read /me -> status %d (id matches page: %t); the JSON-body/form fallback stays the default", status, me["id"] == page)
	}

	// Permission listing (O2/O3): the scopes the operator attests must actually be granted.
	status, perms := get("me/permissions", nil)
	granted := map[string]bool{}
	if data, ok := perms["data"].([]any); ok {
		for _, item := range data {
			if m, ok := item.(map[string]any); ok && m["status"] == "granted" {
				if name, ok := m["permission"].(string); ok {
					granted[name] = true
				}
			}
		}
	}
	report["permissions_status"], report["granted_permissions"] = status, len(granted)
	for _, need := range []string{"pages_messaging", "pages_manage_metadata", "pages_show_list", "pages_read_engagement"} {
		report["has_"+need] = granted[need]
		if status == 200 && !granted[need] {
			t.Errorf("permission %s is not granted on the test Page token (owner input O2)", need)
		}
	}
	report["has_instagram_manage_messages"] = granted["instagram_manage_messages"] // U2: recorded, not asserted

	// Webhook subscription (F3): the app must be installed on the Page for `feed`.
	status, subs := get(page+"/subscribed_apps", nil)
	report["subscribed_apps_status"] = status
	fields := []string{}
	if data, ok := subs["data"].([]any); ok {
		for _, item := range data {
			if m, ok := item.(map[string]any); ok {
				if fs, ok := m["subscribed_fields"].([]any); ok {
					for _, f := range fs {
						fields = append(fields, fmt.Sprint(f))
					}
				}
			}
		}
	}
	report["subscribed_fields"] = fields
	if status == 200 {
		has := false
		for _, f := range fields {
			has = has || f == "feed"
		}
		if !has {
			t.Errorf("the app is not subscribed to the Page `feed` field (POST /{page-id}/subscribed_apps, F3)")
		}
	}

	// U4 (optional): can a read field prove a private reply was already sent?
	if comment := os.Getenv("META_LIVE_COMMENT_ID"); regexp.MustCompile(`^[0-9_]{1,80}$`).MatchString(comment) {
		status, c := get(comment, url.Values{"fields": {"id,can_reply_privately,created_time"}})
		_, has := c["can_reply_privately"]
		report["U4_comment_read_status"], report["U4_can_reply_privately_field_present"] = status, has
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("MCI11 probe report (redacted; GET only):\n%s", out)
}
