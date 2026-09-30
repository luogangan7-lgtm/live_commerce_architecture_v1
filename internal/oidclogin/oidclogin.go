// Package oidclogin owns verification of an external OIDC login (discovery, PKCE S256, one-use code,
// ID-token signature and nonce) without exposing provider tokens.
//
// It never derives application authorization from identity claims (internal/identity maps a verified
// issuer and subject to a principal), never stores a provider token, and never accepts a non-
// loopback plain-HTTP issuer outside the explicit test switch. External host: the configured OIDC
// issuer.
package oidclogin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	requestTimeout = 5 * time.Second
	maxURLLength   = 2048
	maxInputLength = 4096
	maxValueLength = 512
	maxSubjectSize = 255
)

type Config struct {
	Issuer                string
	ClientID              string
	ClientSecret          string
	RedirectURL           string
	AllowLoopbackForTests bool
}

type Identity struct {
	Issuer  string
	Subject string
}

type Provider struct {
	clientID string
	client   *http.Client
	oauth2   oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// New discovers an OIDC provider using a bounded HTTP client. Redirects may
// not change origin, preventing discovery, key, and token fetches from being
// silently redirected to a different network destination.
func New(ctx context.Context, config Config) (*Provider, error) {
	if ctx == nil {
		return nil, errors.New("oidc context required")
	}
	config.Issuer = strings.TrimSpace(config.Issuer)
	config.ClientID = strings.TrimSpace(config.ClientID)
	config.RedirectURL = strings.TrimSpace(config.RedirectURL)
	if config.ClientID == "" || len(config.ClientID) > maxValueLength {
		return nil, errors.New("oidc client id invalid")
	}
	issuer, err := validateConfiguredURL(config.Issuer, config.AllowLoopbackForTests, false)
	if err != nil {
		return nil, errors.New("oidc issuer url invalid")
	}
	redirect, err := validateConfiguredURL(config.RedirectURL, config.AllowLoopbackForTests, true)
	if err != nil {
		return nil, errors.New("oidc redirect url invalid")
	}

	client := &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 || len(via) == 0 || !safeEndpointURL(request.URL, config.AllowLoopbackForTests) || !sameOrigin(via[len(via)-1].URL, request.URL) {
				return errors.New("oidc redirect rejected")
			}
			return nil
		},
	}
	discoveryCtx, cancel := context.WithTimeout(oidc.ClientContext(ctx, client), requestTimeout)
	defer cancel()
	discovered, err := oidc.NewProvider(discoveryCtx, issuer.String())
	if err != nil {
		return nil, errors.New("oidc discovery failed")
	}
	endpoint := discovered.Endpoint()
	var metadata struct {
		JWKSURL                           string   `json:"jwks_uri"`
		TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	}
	if err := discovered.Claims(&metadata); err != nil || !safeEndpoint(metadata.JWKSURL, config.AllowLoopbackForTests) ||
		!safeEndpoint(endpoint.AuthURL, config.AllowLoopbackForTests) || !safeEndpoint(endpoint.TokenURL, config.AllowLoopbackForTests) {
		return nil, errors.New("oidc provider endpoint invalid")
	}
	endpoint.AuthStyle, err = tokenAuthStyle(config.ClientSecret, metadata.TokenEndpointAuthMethodsSupported)
	if err != nil {
		return nil, err
	}

	return &Provider{
		clientID: config.ClientID,
		client:   client,
		oauth2: oauth2.Config{
			ClientID:     config.ClientID,
			ClientSecret: config.ClientSecret,
			RedirectURL:  redirect.String(),
			Endpoint:     endpoint,
			Scopes:       []string{oidc.ScopeOpenID},
		},
		verifier: discovered.Verifier(&oidc.Config{ClientID: config.ClientID}),
	}, nil
}

func (provider *Provider) AuthorizationURL(state, nonce, verifier string) (string, error) {
	if provider == nil || !validOpaqueValue(state) || !validOpaqueValue(nonce) || !validVerifier(verifier) {
		return "", errors.New("oidc authorization input invalid")
	}
	challenge := sha256.Sum256([]byte(verifier))
	return provider.oauth2.AuthCodeURL(state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:])),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	), nil
}

func (provider *Provider) Exchange(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	if provider == nil || ctx == nil || code == "" || len(code) > maxInputLength || !validOpaqueValue(nonce) || !validVerifier(verifier) {
		return Identity{}, errors.New("oidc exchange input invalid")
	}
	exchangeCtx, cancel := context.WithTimeout(oidc.ClientContext(ctx, provider.client), requestTimeout)
	defer cancel()
	token, err := provider.oauth2.Exchange(exchangeCtx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, errors.New("oidc token exchange failed")
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" || len(rawIDToken) > 32*1024 {
		return Identity{}, errors.New("oidc id token missing")
	}
	idToken, err := provider.verifier.Verify(exchangeCtx, rawIDToken)
	if err != nil {
		return Identity{}, errors.New("oidc id token invalid")
	}
	var claims struct {
		Nonce           string `json:"nonce"`
		AuthorizedParty string `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, errors.New("oidc id token claims invalid")
	}
	if !constantTimeEqual(claims.Nonce, nonce) {
		return Identity{}, errors.New("oidc nonce invalid")
	}
	if len(idToken.Audience) > 1 && claims.AuthorizedParty == "" {
		return Identity{}, errors.New("oidc authorized party missing")
	}
	if claims.AuthorizedParty != "" && !constantTimeEqual(claims.AuthorizedParty, provider.clientID) {
		return Identity{}, errors.New("oidc authorized party invalid")
	}
	if !validSubject(idToken.Subject) {
		return Identity{}, errors.New("oidc subject invalid")
	}
	return Identity{Issuer: idToken.Issuer, Subject: idToken.Subject}, nil
}

func validateConfiguredURL(raw string, allowLoopback bool, callback bool) (*url.URL, error) {
	if raw == "" || len(raw) > maxURLLength {
		return nil, errors.New("empty url")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("malformed url")
	}
	if !callback && parsed.RawQuery != "" {
		return nil, errors.New("issuer url invalid")
	}
	if !safeEndpointURL(parsed, allowLoopback) {
		return nil, errors.New("unsafe url")
	}
	return parsed, nil
}

func safeEndpoint(raw string, allowLoopback bool) bool {
	if raw == "" || len(raw) > maxURLLength {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && safeEndpointURL(parsed, allowLoopback)
}

func safeEndpointURL(parsed *url.URL, allowLoopback bool) bool {
	if parsed == nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return allowLoopback && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func validOpaqueValue(value string) bool {
	return value != "" && len(value) <= maxValueLength
}

func validVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, character := range verifier {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-._~", character) {
			continue
		}
		return false
	}
	return true
}

func validSubject(subject string) bool {
	if subject == "" || len(subject) > maxSubjectSize {
		return false
	}
	for index := 0; index < len(subject); index++ {
		if subject[index] < 0x21 || subject[index] > 0x7e {
			return false
		}
	}
	return true
}

func constantTimeEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func tokenAuthStyle(clientSecret string, supported []string) (oauth2.AuthStyle, error) {
	if clientSecret == "" {
		return oauth2.AuthStyleInParams, nil
	}
	if len(supported) == 0 {
		return oauth2.AuthStyleInHeader, nil
	}
	for _, method := range supported {
		if method == "client_secret_basic" {
			return oauth2.AuthStyleInHeader, nil
		}
	}
	for _, method := range supported {
		if method == "client_secret_post" {
			return oauth2.AuthStyleInParams, nil
		}
	}
	return 0, errors.New("oidc token authentication unsupported")
}
