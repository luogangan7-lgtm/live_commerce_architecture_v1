// claims.go loads the keyword-claims merchant configuration for the API process: the
// COMMERCE_CLAIMS_ENABLED flag and the server-held manual-label HMAC key
// COMMERCE_CLAIMS_LABEL_KEY (contract contracts/live-keyword-claims-v1.md §4.2, §10).
//
// Non-goals: no claims route or rule (internal/httpapi M1–M7, internal/claims), no key
// ring or rotation (§12: rotating the key turns an in-flight same-key new-actor retry into
// 409), no database access, and no logging of the key. Buyer claim-link routes (B1–B2)
// need no key and are mounted with the buyer transport.

package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"livecommerce/internal/claims"
	"livecommerce/internal/identityhttp"
)

var errClaimsConfig = errors.New("claims_invalid_config")

// claimsConfig carries the decoded label key; labels is nil while claims are disabled.
type claimsConfig struct{ labels *claims.LabelKey }

// Every other configured secret the label key must not reuse (§4.2 "distinct from every
// other configured key"): scalar secrets compare by value, key-ring JSON documents by any
// common text encoding of the same 32 bytes appearing inside them.
var (
	claimsScalarSecrets  = []string{"COMMERCE_BFF_KEY", "COMMERCE_BUYER_BFF_KEY", "COMMERCE_ACCOUNT_REPLAY_KEY", "COMMERCE_OIDC_CLIENT_SECRET", "COMMERCE_IDENTITY_PROVIDER_KEY"}
	claimsKeyRingSecrets = []string{"COMMERCE_ACCOUNT_KEYS_JSON", "COMMERCE_META_PAYLOAD_KEYS_JSON", "COMMERCE_META_APPS_JSON", "COMMERCE_MEDIA_MATERIAL_KEYS_JSON"}
)

// loadClaimsConfig is called once by run() after the Studio configuration. Disabled mode
// ("" or "0") reads only its flag. Enabled mode requires Studio (which already requires
// signed identity and a literal loopback listener) and a canonical 32-byte key that no
// other configured secret reuses; anything else refuses API startup (KC13).
func loadClaimsConfig(getenv func(string) string, studioEnabled bool) (claimsConfig, error) {
	if getenv == nil {
		return claimsConfig{}, errClaimsConfig
	}
	enabled, err := flag(getenv("COMMERCE_CLAIMS_ENABLED"))
	if err != nil {
		return claimsConfig{}, errClaimsConfig
	}
	if !enabled {
		return claimsConfig{}, nil
	}
	if !studioEnabled {
		return claimsConfig{}, errClaimsConfig
	}
	encoded := getenv("COMMERCE_CLAIMS_LABEL_KEY")
	if !identityhttp.ValidSecret(encoded) {
		return claimsConfig{}, errClaimsConfig
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return claimsConfig{}, errClaimsConfig
	}
	for _, name := range claimsScalarSecrets {
		if getenv(name) == encoded {
			return claimsConfig{}, errClaimsConfig
		}
	}
	forms := []string{encoded, base64.URLEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw), hex.EncodeToString(raw)}
	for _, name := range claimsKeyRingSecrets {
		document := getenv(name)
		for _, form := range forms {
			if strings.Contains(document, form) {
				return claimsConfig{}, errClaimsConfig
			}
		}
	}
	labels, err := claims.NewLabelKey(raw)
	if err != nil {
		return claimsConfig{}, errClaimsConfig
	}
	return claimsConfig{labels: &labels}, nil
}
