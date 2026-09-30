// File: deploy/tools/lcentry/probe.go
// Purpose: `lcentry probe` — loopback-only HTTP health probe (deploy-design §6).
// Runs as/in: Docker healthcheck exec inside api (/readyz), admin (/) and
// storefront (/) containers; all share the edge netns loopback.
// Reads env: none (proxy variables are deliberately ignored).
// Reads secrets: none.
// Status: MODEL_ONLY; verified by smoke S04 (unit) and S15 (runtime).
// Change rules: never allow non-loopback targets — the probe must not become
// an SSRF/egress primitive inside the shared netns.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// probeCommand parses flags, validates the URL and performs one GET.
func probeCommand(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	maxStatus := fs.Int("max-status", 399, "highest HTTP status treated as healthy")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "probe: usage: lcentry probe [-timeout d] [-max-status n] http://127.0.0.1:<port>/path")
		return exitConfig
	}
	if *timeout <= 0 || *timeout > time.Minute || *maxStatus < 200 || *maxStatus > 599 {
		fmt.Fprintln(stderr, "probe: invalid flag value")
		return exitConfig
	}
	target, err := loopbackURL(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "probe: "+err.Error())
		return exitConfig
	}
	status, err := probeOnce(target, *timeout)
	if err != nil {
		fmt.Fprintln(stderr, "probe: dial error")
		return exitProbe
	}
	if status < 200 || status > *maxStatus {
		fmt.Fprintln(stderr, "probe: status "+strconv.Itoa(status))
		return exitProbe
	}
	return exitOK
}

// loopbackURL accepts only http://127.0.0.1:<port>/... or http://[::1]:<port>/...
func loopbackURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" {
		return "", errors.New("URL must be http://127.0.0.1:<port>/ or http://[::1]:<port>/")
	}
	host, port := u.Hostname(), u.Port()
	if host != "127.0.0.1" && host != "::1" {
		return "", errors.New("URL host must be literal loopback")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("URL needs an explicit port")
	}
	return u.String(), nil
}

// probeOnce performs a single GET without redirects or proxies and discards
// the body (bounded) so the connection closes cleanly.
func probeOnce(target string, timeout time.Duration) (int, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil, // never route a loopback probe through HTTP(S)_PROXY
			DialContext:       (&net.Dialer{Timeout: timeout}).DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(target)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}
