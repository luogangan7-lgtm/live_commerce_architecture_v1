package metaads

// config.go: Graph host guard, version pin and the shared bounded HTTP helper. Every call of this
// package (routes, CAPI, OAuth) goes through graph.do so that the host rule, the no-redirect rule,
// the body cap and the "an error never carries a URL" rule live in one place.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// GraphHost is the only non-loopback base URL Config accepts (meta-ads-v1 §3).
const GraphHost = "https://graph.facebook.com"

const (
	// maxBody caps every Graph response (brief: bounded bodies <= 1 MiB).
	maxBody = 1 << 20
)

var (
	// ErrConfig is every constructor/config failure; it never carries a secret.
	ErrConfig = errors.New("metaads: invalid configuration")

	// errTransport is every failed exchange (dial, timeout, TLS, redirect, oversize body). It is
	// deliberately fixed text: a *url.Error would echo the request URL, which for the OAuth code
	// exchange contains client_secret.
	errTransport = errors.New("metaads: graph exchange failed")

	versionPattern   = regexp.MustCompile(`^v[0-9]{1,3}\.[0-9]{1,2}$`)
	loopbackPattern  = regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]{1,5}$`)
	partnerPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,50}$`)
	numericIDPattern = regexp.MustCompile(`^[0-9]{1,40}$`)
)

// Config is the Graph adapter configuration. GraphBaseURL is "" (= GraphHost), GraphHost or a
// loopback http://127.0.0.1:<port> (MOCK). GraphVersion is required (A-9, v26.0 in production).
// PartnerAgent is the CAPI `partner_agent` constant (F7); it is only needed by Client.PostEvent.
// HTTPClient is optional and copied; redirects are never followed (a 307 would replay a POST body
// that carries the token).
type Config struct {
	GraphBaseURL string
	GraphVersion string
	PartnerAgent string
	HTTPClient   *http.Client
}

func (c Config) validate() error {
	base := c.GraphBaseURL
	if base == "" {
		base = GraphHost
	}
	if !versionPattern.MatchString(c.GraphVersion) || !(base == GraphHost || loopbackPattern.MatchString(base)) {
		return ErrConfig
	}
	if c.PartnerAgent != "" && !partnerPattern.MatchString(c.PartnerAgent) {
		return ErrConfig
	}
	return nil
}

// graph is the shared Graph transport. It holds no credential.
type graph struct {
	base, version string
	hc            *http.Client
}

func newGraph(cfg Config) (*graph, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	base := cfg.GraphBaseURL
	if base == "" {
		base = GraphHost
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &graph{base: base, version: cfg.GraphVersion, hc: hc}, nil
}

// reply is one bounded Graph response.
type reply struct {
	status int
	body   []byte
}

func (r reply) ok() bool { return r.status >= 200 && r.status <= 299 }

// do performs one Graph call: path is relative to /{version}/ (no leading slash). A GET carries the
// token in the Authorization header (never the URL); a POST carries it as `access_token` in the JSON
// body (the documented POST form; metareply precedent) and never in the URL. // UNKNOWN until MA-S1:
// that Graph accepts `Authorization: Bearer` on GET reads for BISU tokens; fallback is the documented
// access_token query parameter, which would need a reviewed change because it puts the token in a URL.
// The only secret allowed in a URL is the OAuth exchange's client_secret (contract F3, GET form); its
// errors are flattened to errTransport. A non-nil error means "no usable response": the request may
// or may not have been processed, so callers of mutating calls must treat it as UNKNOWN.
func (g *graph) do(ctx context.Context, method, path string, query url.Values, token []byte, payload map[string]any) (reply, error) {
	target := g.base + "/" + g.version + "/" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	if method == http.MethodPost {
		if payload == nil {
			payload = map[string]any{}
		}
		if len(token) > 0 {
			payload["access_token"] = string(token) // ponytail: Go strings cannot be zeroed; process-memory only, short-lived
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return reply{}, errTransport
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return reply{}, errTransport
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	} else if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return reply{}, errTransport
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return reply{}, errTransport
	}
	return reply{status: resp.StatusCode, body: raw}, nil
}

// SecretFromEnv returns the secret named name (for example COMMERCE_META_ADS_APP_SECRET) from either
// form the deployment can deliver (O-D: secrets are files, never chat, never argv):
//   - NAME_FILE=/run/secrets/x is expanded by deploy/tools/lcentry into NAME=<contents> before the
//     process starts (and NAME_FILE is dropped), so in a container only NAME is visible;
//   - NAME_FILE=<path> read directly when the binary runs without lcentry (dev, tests, operators).
//
// Setting both, neither, a non-regular file or a value outside 1..max bytes is ErrConfig with no detail
// (never the path or the contents). One trailing CR/LF run is trimmed. The caller clears the result.
func SecretFromEnv(getenv func(string) string, name string, max int) ([]byte, error) {
	if getenv == nil || max < 1 {
		return nil, ErrConfig
	}
	inline, path := getenv(name), getenv(name+"_FILE")
	if (inline == "") == (path == "") {
		return nil, ErrConfig
	}
	raw := []byte(inline)
	if path != "" {
		info, err := os.Stat(path)
		if err != nil || len(path) > 4096 || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > int64(max) {
			return nil, ErrConfig
		}
		if raw, err = os.ReadFile(path); err != nil {
			return nil, ErrConfig
		}
	}
	trimmed := []byte(strings.TrimRight(string(raw), "\r\n"))
	clear(raw)
	if len(trimmed) < 1 || len(trimmed) > max {
		clear(trimmed)
		return nil, ErrConfig
	}
	return trimmed, nil
}
