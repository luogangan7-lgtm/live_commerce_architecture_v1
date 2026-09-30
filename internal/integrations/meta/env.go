package meta

import (
	"encoding/base64"
	"errors"
)

var ErrRuntimeConfig = errors.New("meta: invalid runtime configuration")

type WebhookEndpoint struct {
	Path     string
	Verifier *Verifier
}

func (WebhookEndpoint) String() string     { return "meta.WebhookEndpoint{redacted}" }
func (e WebhookEndpoint) GoString() string { return e.String() }
func (WebhookEndpoint) MarshalJSON() ([]byte, error) {
	return []byte(`"meta.WebhookEndpoint{redacted}"`), nil
}

func exactFields(m map[string]any, fields ...string) bool {
	if len(m) != len(fields) {
		return false
	}
	for _, name := range fields {
		if _, ok := m[name]; !ok {
			return false
		}
	}
	return true
}

// LoadWebhookEndpoints accepts only exact, bounded configuration. The verifier
// retains credentials privately; paths contain only validated app identifiers.
func LoadWebhookEndpoints(getenv func(string) string) ([]WebhookEndpoint, error) {
	if getenv == nil {
		return nil, ErrRuntimeConfig
	}
	raw := getenv("COMMERCE_META_APPS_JSON")
	if len(raw) < 1 || len(raw) > 32768 {
		return nil, ErrRuntimeConfig
	}
	root, err := ParseStrict([]byte(raw))
	if err != nil || !exactFields(root, "apps") {
		return nil, ErrRuntimeConfig
	}
	items, ok := root["apps"].([]any)
	if !ok || len(items) < 1 || len(items) > 16 {
		return nil, ErrRuntimeConfig
	}
	endpoints := make([]WebhookEndpoint, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		m, ok := object(item)
		if !ok || !exactFields(m, "app_id", "object", "app_secret", "verify_token") {
			return nil, ErrRuntimeConfig
		}
		appID, appOK := m["app_id"].(string)
		objectName, objectOK := m["object"].(string)
		secret, secretOK := m["app_secret"].(string)
		token, tokenOK := m["verify_token"].(string)
		if !appOK || !objectOK || !secretOK || !tokenOK {
			return nil, ErrRuntimeConfig
		}
		verifier, err := NewVerifier(Config{AppID: appID, Object: objectName, AppSecret: secret, VerifyToken: token})
		if err != nil {
			return nil, ErrRuntimeConfig
		}
		path := "/v1/meta/webhooks/" + appID + "/" + objectName
		if seen[path] {
			return nil, ErrRuntimeConfig
		}
		seen[path] = true
		endpoints = append(endpoints, WebhookEndpoint{Path: path, Verifier: verifier})
	}
	return endpoints, nil
}

func LoadPayloadKeyring(getenv func(string) string) (*PayloadKeyring, error) {
	if getenv == nil {
		return nil, ErrRuntimeConfig
	}
	activeID := getenv("COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID")
	raw := getenv("COMMERCE_META_PAYLOAD_KEYS_JSON")
	if !validPayloadKeyID(activeID) || len(raw) < 1 || len(raw) > 8192 {
		return nil, ErrRuntimeConfig
	}
	root, err := ParseStrict([]byte(raw))
	if err != nil || !exactFields(root, "keys") {
		return nil, ErrRuntimeConfig
	}
	items, ok := root["keys"].([]any)
	if !ok || len(items) < 1 || len(items) > 16 {
		return nil, ErrRuntimeConfig
	}
	keys := make(map[string][]byte, len(items))
	for _, item := range items {
		m, ok := object(item)
		if !ok || !exactFields(m, "id", "key_base64") {
			return nil, ErrRuntimeConfig
		}
		id, idOK := m["id"].(string)
		encoded, keyOK := m["key_base64"].(string)
		if !idOK || !keyOK || !validPayloadKeyID(id) || len(encoded) != 44 {
			return nil, ErrRuntimeConfig
		}
		if _, duplicate := keys[id]; duplicate {
			return nil, ErrRuntimeConfig
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) != 32 || base64.StdEncoding.EncodeToString(decoded) != encoded {
			return nil, ErrRuntimeConfig
		}
		keys[id] = decoded
	}
	keyring, err := NewPayloadKeyring(activeID, keys)
	if err != nil {
		return nil, ErrRuntimeConfig
	}
	return keyring, nil
}
