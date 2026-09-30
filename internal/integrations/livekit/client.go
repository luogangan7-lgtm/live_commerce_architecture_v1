// Package livekit owns the bounded LiveKit Cloud Egress and browser-input wire profiles: the HTTP
// client, response decoding, sealed project material and the media worker environment loader.
//
// It reports provider observations, not audience, billing, or resource closure; it never decides
// live session state (internal/live does), never logs a key or token, and never reaches a host
// outside the configured project URL. External host: the LiveKit Cloud project named in the worker
// environment.
package livekit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid     = errors.New("livekit: invalid input")
	ErrUnknown     = errors.New("livekit: outcome unknown")
	ErrUnavailable = errors.New("livekit: observation unavailable")
	ErrNotObserved = errors.New("livekit: not observed")
	roomPattern    = regexp.MustCompile(`^lc_[0-9a-f]{32}$`)
	idPattern      = regexp.MustCompile(`^EG_[A-Za-z0-9_-]{1,100}$`)
	keyPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

const maxResponse = 64 << 10

type Config struct {
	Environment, Endpoint, APIKey, APISecret string
	StreamHosts                              []string
}

func (Config) String() string               { return "livekit.Config{redacted}" }
func (c Config) GoString() string           { return c.String() }
func (Config) MarshalJSON() ([]byte, error) { return []byte(`"livekit.Config{redacted}"`), nil }

type Client struct {
	config     Config
	httpClient *http.Client
}

func (Client) String() string               { return "livekit.Client{redacted}" }
func (c Client) GoString() string           { return c.String() }
func (Client) MarshalJSON() ([]byte, error) { return []byte(`"livekit.Client{redacted}"`), nil }

type StartInput struct {
	RoomName, AspectRatio string
	StreamURLs            []string
}

func (StartInput) String() string               { return "livekit.StartInput{redacted}" }
func (s StartInput) GoString() string           { return s.String() }
func (StartInput) MarshalJSON() ([]byte, error) { return []byte(`"livekit.StartInput{redacted}"`), nil }

// Target must come from the trusted attempt record; syntax alone cannot prove ownership.
type Target struct{ RoomName, EgressID string }

type Observation struct {
	EgressID, RoomName, Status          string
	StartedAtNS, UpdatedAtNS, EndedAtNS int64
}

func New(config Config, transport ...http.RoundTripper) (*Client, error) {
	if !validConfig(config) {
		return nil, ErrInvalid
	}
	var rt http.RoundTripper
	if config.Environment == "MOCK" {
		if len(transport) != 1 || nilTransport(transport[0]) {
			return nil, ErrInvalid
		}
		rt = transport[0]
	} else {
		if len(transport) != 0 {
			return nil, ErrInvalid
		}
		rt = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true, Proxy: nil}
	}
	config.StreamHosts = append([]string(nil), config.StreamHosts...)
	return &Client{config: config, httpClient: &http.Client{Timeout: 10 * time.Second, Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func nilTransport(rt http.RoundTripper) bool {
	if rt == nil {
		return true
	}
	v := reflect.ValueOf(rt)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func validConfig(c Config) bool {
	if (c.Environment != "MOCK" && c.Environment != "LIVE") || !keyPattern.MatchString(c.APIKey) ||
		len(c.APISecret) < 32 || len(c.APISecret) > 256 || len(c.StreamHosts) == 0 || len(c.StreamHosts) > 16 {
		return false
	}
	for i := range c.APISecret {
		if c.APISecret[i] < '!' || c.APISecret[i] > '~' {
			return false
		}
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.Host != u.Hostname() {
		return false
	}
	label := strings.TrimSuffix(u.Hostname(), ".livekit.cloud")
	if label == u.Hostname() || len(label) == 0 || len(label) > 63 || !validLabel(label) {
		return false
	}
	seen := make(map[string]bool, len(c.StreamHosts))
	for _, host := range c.StreamHosts {
		if !validHost(host) || seen[host] {
			return false
		}
		seen[host] = true
	}
	return true
}

func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := range s {
		if !((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= '0' && s[i] <= '9') || s[i] == '-') {
			return false
		}
	}
	return true
}

func validHost(host string) bool {
	if len(host) > 253 || host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

func validTarget(t Target) bool {
	return roomPattern.MatchString(t.RoomName) && idPattern.MatchString(t.EgressID)
}

func (c *Client) validStart(in StartInput) bool {
	if !roomPattern.MatchString(in.RoomName) || (in.AspectRatio != "16:9" && in.AspectRatio != "9:16") || len(in.StreamURLs) < 1 || len(in.StreamURLs) > 2 {
		return false
	}
	seen := make(map[string]bool, len(in.StreamURLs))
	for _, raw := range in.StreamURLs {
		if len(raw) == 0 || len(raw) > 4096 || seen[raw] {
			return false
		}
		seen[raw] = true
		for i := range raw {
			if raw[i] <= ' ' || raw[i] >= 0x7f {
				return false
			}
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "rtmps" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.Path == "" || (u.Port() != "" && u.Port() != "443") {
			return false
		}
		allowed := false
		for _, host := range c.config.StreamHosts {
			if u.Hostname() == host {
				allowed = true
				break
			}
		}
		if !allowed || (u.Port() == "" && u.Host != u.Hostname()) || (u.Port() == "443" && u.Host != u.Hostname()+":443") {
			return false
		}
	}
	return true
}

func (c *Client) Start(ctx context.Context, in StartInput) (Observation, error) {
	if !c.ready() || ctx == nil || !c.validStart(in) {
		return Observation{}, ErrInvalid
	}
	preset := "H264_720P_30"
	if in.AspectRatio == "9:16" {
		preset = "PORTRAIT_H264_720P_30"
	}
	wire := struct {
		RoomName string `json:"room_name"`
		Template struct {
			Layout string `json:"layout"`
		} `json:"template"`
		Preset  string `json:"preset"`
		Outputs []struct {
			Stream struct {
				Protocol string   `json:"protocol"`
				URLs     []string `json:"urls"`
			} `json:"stream"`
		} `json:"outputs"`
	}{RoomName: in.RoomName, Preset: preset}
	wire.Template.Layout = "speaker"
	wire.Outputs = make([]struct {
		Stream struct {
			Protocol string   `json:"protocol"`
			URLs     []string `json:"urls"`
		} `json:"stream"`
	}, 1)
	wire.Outputs[0].Stream.Protocol = "RTMP"
	wire.Outputs[0].Stream.URLs = append([]string(nil), in.StreamURLs...)
	body, ok := c.call(ctx, in.RoomName, "StartEgress", wire)
	if !ok {
		return Observation{}, ErrUnknown
	}
	obs, err := decodeObservation(body)
	if err != nil || obs.RoomName != in.RoomName {
		return Observation{}, ErrUnknown
	}
	return obs, nil
}

func (c *Client) Query(ctx context.Context, target Target) (Observation, error) {
	if !c.ready() || ctx == nil || !validTarget(target) {
		return Observation{}, ErrInvalid
	}
	body, ok := c.call(ctx, target.RoomName, "ListEgress", struct {
		RoomName string `json:"room_name"`
		EgressID string `json:"egress_id"`
		Active   bool   `json:"active"`
	}{target.RoomName, target.EgressID, false})
	if !ok {
		return Observation{}, ErrUnavailable
	}
	return decodeList(body, target)
}

// FindByRoom reports a candidate observation for a trusted persisted attempt
// room. The caller must verify ownership before adopting its Egress ID.
func (c *Client) FindByRoom(ctx context.Context, roomName string) (Observation, error) {
	if !c.ready() || ctx == nil || !roomPattern.MatchString(roomName) {
		return Observation{}, ErrInvalid
	}
	body, ok := c.call(ctx, roomName, "ListEgress", struct {
		RoomName string `json:"room_name"`
		Active   bool   `json:"active"`
	}{roomName, false})
	if !ok {
		return Observation{}, ErrUnavailable
	}
	return decodeList(body, Target{RoomName: roomName})
}

func (c *Client) Stop(ctx context.Context, target Target) (Observation, error) {
	if !c.ready() || ctx == nil || !validTarget(target) {
		return Observation{}, ErrInvalid
	}
	body, ok := c.call(ctx, target.RoomName, "StopEgress", struct {
		EgressID string `json:"egress_id"`
	}{target.EgressID})
	if !ok {
		return Observation{}, ErrUnknown
	}
	obs, err := decodeObservation(body)
	if err != nil || obs.RoomName != target.RoomName || obs.EgressID != target.EgressID {
		return Observation{}, ErrUnknown
	}
	return obs, nil
}

func (c *Client) ready() bool { return c != nil && c.httpClient != nil }

func (c *Client) call(ctx context.Context, room, method string, payload any) ([]byte, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	token, err := c.token(room)
	if err != nil {
		return nil, false
	}
	return c.callAuthorized(ctx, "Egress", method, token, body)
}

// callAuthorized is private so callers cannot choose an endpoint or grant.
func (c *Client) callAuthorized(ctx context.Context, service, method, token string, body []byte) ([]byte, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	endpoint := strings.TrimSuffix(c.config.Endpoint, "/") + "/twirp/livekit." + service + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	var reader io.Reader = response.Body
	switch response.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(response.Body)
		if err != nil {
			return nil, false
		}
		defer gz.Close()
		reader = gz
	default:
		return nil, false
	}
	result, err := io.ReadAll(io.LimitReader(reader, maxResponse+1))
	return result, err == nil && len(result) <= maxResponse
}

func (c *Client) token(room string) (string, error) {
	now := time.Now().Unix()
	claims := struct {
		Issuer    string `json:"iss"`
		IssuedAt  int64  `json:"iat"`
		NotBefore int64  `json:"nbf"`
		Expires   int64  `json:"exp"`
		Video     struct {
			RoomRecord bool   `json:"roomRecord"`
			Room       string `json:"room"`
		} `json:"video"`
	}{Issuer: c.config.APIKey, IssuedAt: now, NotBefore: now, Expires: now + 60}
	claims.Video.RoomRecord, claims.Video.Room = true, room
	data, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return c.signClaims(data), nil
}

func (c *Client) signClaims(data []byte) string {
	header := []byte(`{"alg":"HS256","typ":"JWT"}`)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, []byte(c.config.APISecret))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
