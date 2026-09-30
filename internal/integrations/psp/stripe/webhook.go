// webhook.go: Stripe-Signature verification and strict event projection
// (contracts/stripe-psp-v1.md §5.8). It never touches PG, never decides admission
// (livemode/account/type/probe outcomes belong to stripewebhook, §9.1) and never
// logs or returns event bytes.
//
// Ownership: integration_worker. Dependencies: crypto/hmac, crypto/sha256 and
// strictjson.go. The refund and charge event types are projected by the same code path (stripe-refund-v1 §3). Callers: internal/payments/stripewebhook (the API handler) only.
// Signature scheme: https://docs.stripe.com/webhooks/signature and
// https://docs.stripe.com/webhooks (retrieved 2026-09-28, F5).

package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxSignatureHeader   = 2048
	maxSignatureElements = 16
	maxSignedAt          = int64(1) << 40
	// signatureTolerance is fixed (never 0, never configurable); both directions (F5).
	signatureTolerance = 300
)

var (
	webhookSecretPattern = regexp.MustCompile(`^whsec_[!-~]{16,249}$`)
	signedAtPattern      = regexp.MustCompile(`^[1-9][0-9]{0,12}$`)
	v1Pattern            = regexp.MustCompile(`^[0-9a-f]{64}$`)
	eventTypePattern     = regexp.MustCompile(`^[a-z0-9_.]{1,128}$`)
	apiVersionPattern    = regexp.MustCompile(`^[A-Za-z0-9._-]{0,64}$`)
)

// WebhookConfig holds the endpoint secrets. The API is the only process that reads them.
type WebhookConfig struct {
	Secrets     []string // 1..2 distinct: STRIPE_WEBHOOK_SECRET [, STRIPE_WEBHOOK_SECRET_NEXT]
	AccountID   string
	Environment string // SANDBOX ⇒ livemode=false; LIVE ⇒ true
}

func (WebhookConfig) String() string     { return "stripe.WebhookConfig{redacted}" }
func (c WebhookConfig) GoString() string { return c.String() }
func (WebhookConfig) MarshalJSON() ([]byte, error) {
	return []byte(`"stripe.WebhookConfig{redacted}"`), nil
}

// WebhookVerifier checks signatures against one or two secrets (a ≤24 h roll, F5).
type WebhookVerifier struct {
	secrets [][]byte
	cfg     WebhookConfig
}

func (WebhookVerifier) String() string     { return "stripe.WebhookVerifier{redacted}" }
func (v WebhookVerifier) GoString() string { return v.String() }
func (WebhookVerifier) MarshalJSON() ([]byte, error) {
	return []byte(`"stripe.WebhookVerifier{redacted}"`), nil
}

// Event is the only projection of a verified delivery. When the payload is signed but
// unusable, Malformed is true and only BodySHA256 and SignedAt are meaningful, so the
// caller can quarantine it by body hash (§9.1); it is never dropped.
type Event struct {
	ID, Type, APIVersion, ObjectType, SessionID, ClientReferenceID, MetadataAttempt string
	// PaymentIntentID and MetadataRefund are projected only from refund and charge objects
	// (stripe-refund-v1 §3): the PaymentIntent maps a charge, lc_refund maps an unpinned refund.
	PaymentIntentID, MetadataRefund                   string
	Livemode, AccountPresent, ProbeSession, Malformed bool
	Created, SignedAt                                 int64
	BodySHA256                                        [32]byte
}

func (Event) String() string               { return "stripe.Event{redacted}" }
func (e Event) GoString() string           { return e.String() }
func (Event) MarshalJSON() ([]byte, error) { return []byte(`"stripe.Event{redacted}"`), nil }

// NewWebhookVerifier validates cfg: 1..2 distinct secrets matching ^whsec_[!-~]{16,249}$,
// an acct_ id and Environment SANDBOX or LIVE. Otherwise ErrInvalid.
func NewWebhookVerifier(cfg WebhookConfig) (*WebhookVerifier, error) {
	if len(cfg.Secrets) < 1 || len(cfg.Secrets) > 2 ||
		(len(cfg.Secrets) == 2 && cfg.Secrets[0] == cfg.Secrets[1]) ||
		!accountPattern.MatchString(cfg.AccountID) ||
		(cfg.Environment != envSandbox && cfg.Environment != envLive) {
		return nil, ErrInvalid
	}
	v := &WebhookVerifier{cfg: WebhookConfig{AccountID: cfg.AccountID, Environment: cfg.Environment}}
	for _, s := range cfg.Secrets {
		if !webhookSecretPattern.MatchString(s) {
			return nil, ErrInvalid
		}
		v.secrets = append(v.secrets, []byte(s))
	}
	return v, nil
}

// parseSignatureHeader applies §5.8 rules 1–3 and returns t and the decoded v1 values.
func parseSignatureHeader(header string) (int64, [][32]byte, bool) {
	if header == "" || len(header) > maxSignatureHeader {
		return 0, nil, false
	}
	elements := strings.Split(header, ",")
	if len(elements) > maxSignatureElements {
		return 0, nil, false
	}
	var (
		signedAt int64
		seenT    bool
		v1s      [][32]byte
	)
	for _, el := range elements {
		k, v, ok := strings.Cut(el, "=")
		if !ok || k == "" {
			return 0, nil, false
		}
		switch k {
		case "t":
			if seenT || !signedAtPattern.MatchString(v) {
				return 0, nil, false
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 || n > maxSignedAt {
				return 0, nil, false
			}
			signedAt, seenT = n, true
		case "v1":
			if !v1Pattern.MatchString(v) {
				return 0, nil, false
			}
			var sig [32]byte
			if _, err := hex.Decode(sig[:], []byte(v)); err != nil {
				return 0, nil, false
			}
			v1s = append(v1s, sig)
		default:
			// F5: v0 (test) and any other scheme are ignored.
		}
	}
	if !seenT || len(v1s) == 0 {
		return 0, nil, false
	}
	return signedAt, v1s, true
}

// Verify authenticates raw against signatureHeader at time now (§5.8), then projects
// the event with strict JSON. Any signature failure is ErrSignature and happens
// before any JSON parsing. A signed but unusable payload returns
// Event{Malformed:true, BodySHA256, SignedAt} with a nil error.
func (v *WebhookVerifier) Verify(raw []byte, signatureHeader string, now time.Time) (Event, error) {
	if v == nil || len(v.secrets) == 0 {
		return Event{}, ErrInvalid
	}
	signedAt, v1s, ok := parseSignatureHeader(signatureHeader)
	if !ok {
		return Event{}, ErrSignature
	}
	delta := now.Unix() - signedAt
	if delta > signatureTolerance || delta < -signatureTolerance {
		return Event{}, ErrSignature
	}
	matched := false
	for _, secret := range v.secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(strconv.FormatInt(signedAt, 10)))
		mac.Write([]byte{'.'})
		mac.Write(raw)
		expected := mac.Sum(nil)
		for i := range v1s {
			// Constant-time comparison over fixed-width decoded bytes (SP05 source guard).
			if hmac.Equal(expected, v1s[i][:]) {
				matched = true
			}
		}
	}
	if !matched {
		return Event{}, ErrSignature
	}
	ev := Event{SignedAt: signedAt, BodySHA256: sha256.Sum256(raw)}
	projected, ok := projectEvent(raw)
	if !ok {
		ev.Malformed = true
		return ev, nil
	}
	projected.SignedAt, projected.BodySHA256 = ev.SignedAt, ev.BodySHA256
	return projected, nil
}

// projectEvent reads only the §5.8 fields from an authenticated payload.
func projectEvent(raw []byte) (Event, bool) {
	root, err := decodeStrict(raw)
	if err != nil {
		return Event{}, false
	}
	m, ok := root.(map[string]any)
	if !ok {
		return Event{}, false
	}
	o := jsonObject(m)
	object, _, ok1 := o.str("object")
	id, _, ok2 := o.str("id")
	typ, _, ok3 := o.str("type")
	live, livePresent, ok4 := o.boolean("livemode")
	created, ok5 := o.integer("created")
	apiVersion, _, ok6 := o.str("api_version")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || object != "event" ||
		!stripeIDPattern.MatchString(id) || !eventTypePattern.MatchString(typ) ||
		!livePresent || created == nil || *created <= 0 || !apiVersionPattern.MatchString(apiVersion) {
		return Event{}, false
	}
	ev := Event{ID: id, Type: typ, APIVersion: apiVersion, Livemode: live, Created: *created}
	if acct, exists := m["account"]; exists && acct != nil {
		ev.AccountPresent = true
	}
	data, _, ok := o.object("data")
	if !ok {
		return Event{}, false
	}
	if data == nil {
		return ev, true
	}
	obj, _, ok := data.object("object")
	if !ok {
		return Event{}, false
	}
	if obj == nil {
		return ev, true
	}
	objType, _, okT := obj.str("object")
	objID, _, okI := obj.str("id")
	ref, _, okR := obj.str("client_reference_id")
	md, _, okM := obj.object("metadata")
	if !okT || !okI || !okR || !okM || (objID != "" && !stripeIDPattern.MatchString(objID)) ||
		(objType != "" && !eventTypePattern.MatchString(objType)) {
		return Event{}, false
	}
	// boundedRef maps an unbounded reference to "?", which can never match an attempt.
	ev.ObjectType, ev.SessionID, ev.ClientReferenceID = objType, objID, boundedRef(ref)
	if md != nil {
		attempt, _, okA := md.str("lc_attempt")
		probe, probePresent, okP := md.str("lc_probe")
		if !okA || !okP {
			return Event{}, false
		}
		ev.MetadataAttempt = boundedRef(attempt)
		ev.ProbeSession = probePresent && probe != ""
		if objType == "refund" {
			refund, _, okF := md.str("lc_refund")
			if !okF {
				return Event{}, false
			}
			ev.MetadataRefund = boundedRef(refund)
		}
	}
	if objType == "refund" || objType == "charge" {
		// payment_intent is an id string on these objects; another type is an unusable payload.
		pi, _, okPI := obj.str("payment_intent")
		if !okPI || (pi != "" && !stripeIDPattern.MatchString(pi)) {
			return Event{}, false
		}
		ev.PaymentIntentID = pi
	}
	return ev, true
}
