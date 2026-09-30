package checkout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/payuni"
	"livecommerce/internal/platform"
)

type HostedConfig = accounts.HostedConfig

type HostedInput struct {
	OrderID       string `json:"order_id"`
	MethodCode    string `json:"method_code"`
	MethodVersion int64  `json:"method_version"`
	Locale        string `json:"locale"`
}

type HostedForm struct {
	Action string            `json:"action"`
	Fields map[string]string `json:"fields"`
}

// HostedHandoff is the PAYUNi form release or the Stripe redirect release. A Stripe
// RedirectURL is a live Checkout capability: it is never logged or persisted here.
type HostedHandoff struct {
	OrderID     string      `json:"order_id"`
	Disposition string      `json:"disposition"`
	ExpiresAt   time.Time   `json:"expires_at"`
	Form        *HostedForm `json:"form,omitempty"`
	RedirectURL string      `json:"redirect_url,omitempty"`
}

type HostedPaymentStarter struct {
	starter      PaymentStarter
	keys         *accounts.Keyring
	config       HostedConfig
	configDigest [32]byte
	payuni       bool // PAYUNi configured; keys/config/configDigest are meaningful only then
	stripe       *StripeHostedConfig
	stripeDigest [32]byte
}

// HostedProviders selects which providers a hosted service admits; at least one is set.
type HostedProviders struct {
	PAYUNi *HostedConfig
	Stripe *StripeHostedConfig
}

// NewHostedPaymentStarter is the PAYUNi-only constructor kept for existing callers.
func NewHostedPaymentStarter(ctx context.Context, hostedPool *pgxpool.Pool, jobs *river.Client[pgx.Tx],
	profile string, keys *accounts.Keyring, config HostedConfig) (*HostedPaymentStarter, error) {
	return NewHostedPaymentService(ctx, hostedPool, jobs, profile, keys, HostedProviders{PAYUNi: &config})
}

func NewHostedPaymentService(ctx context.Context, hostedPool *pgxpool.Pool, jobs *river.Client[pgx.Tx],
	profile string, keys *accounts.Keyring, p HostedProviders) (*HostedPaymentStarter, error) {
	if ctx == nil || jobs == nil || !validPaymentProfile(profile) || (p.PAYUNi == nil && p.Stripe == nil) {
		return nil, command.ErrInvalid
	}
	out := &HostedPaymentStarter{starter: PaymentStarter{pool: hostedPool, jobs: jobs, profile: profile}}
	if p.PAYUNi != nil {
		canonical, digest, err := p.PAYUNi.CanonicalDigest()
		if keys == nil || err != nil {
			return nil, command.ErrInvalid
		}
		out.keys, out.config, out.configDigest, out.payuni = keys, canonical, digest, true
	}
	if p.Stripe != nil {
		canonical, digest, err := p.Stripe.CanonicalDigest()
		// stripe-live-enable-v1 §5.2: the Stripe branch admits every valid profile (validPaymentProfile above), LIVE
		// included. The owner's flag+reference pair is enforced by the caller (cmd/api loadBuyerPaymentConfig), and the
		// per-store gate is SQL: hosted_payment_view_v2 / start_stripe_payment need a REAL_LIVE, unrevoked qualification.
		if err != nil {
			return nil, command.ErrInvalid
		}
		out.stripe, out.stripeDigest = &canonical, digest
	}
	if err := platform.ValidateHostedPool(ctx, hostedPool); err != nil {
		return nil, err
	}
	return out, nil
}

// BeginHosted commits the original payment start and its sealed form together.
// The repeatable result contains no form or credential material.
func (s *HostedPaymentStarter) BeginHosted(ctx context.Context, token, storeID, key string, in HostedInput) (PaymentResult, error) {
	if ctx == nil || s == nil || s.starter.pool == nil || s.starter.jobs == nil ||
		!validPaymentProfile(s.starter.profile) || !checkoutKey.MatchString(key) || !validHostedInput(in) {
		return PaymentResult{}, command.ErrInvalid
	}
	if in.MethodCode == stripeMethodCode {
		return s.beginStripe(ctx, token, storeID, key, in)
	}
	if !s.payuni || s.keys == nil {
		return PaymentResult{}, command.ErrInvalid
	}
	request, _ := json.Marshal(struct {
		Input        HostedInput `json:"input"`
		Profile      string      `json:"profile"`
		ConfigDigest string      `json:"config_digest"`
	}{in, s.starter.profile, hex.EncodeToString(s.configDigest[:])})
	digest := sha256.Sum256(request)
	tokenHash := sha256.Sum256([]byte(token))
	var out PaymentResult
	err := buyer.WithScope(ctx, s.starter.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		payment := PaymentInput{OrderID: in.OrderID, MethodCode: in.MethodCode, MethodVersion: in.MethodVersion}
		var found bool
		var err error
		out, found, err = s.starter.startPaymentTx(callCtx, tx, scope, tokenHash[:], storeID, key, payment, digest)
		if err != nil || found {
			return err
		}
		built, err := s.keys.BuildPaymentHosted(callCtx, tx, tokenHash[:], storeID, in.OrderID,
			s.starter.profile, in.Locale, s.config)
		if err != nil {
			return err
		}
		form, err := boundedHostedForm(built.Form, s.starter.profile)
		if err != nil {
			return err
		}
		body, err := json.Marshal(form)
		if err != nil {
			return command.ErrConflict
		}
		if _, err = tx.Exec(callCtx, `SELECT checkout.save_hosted_page($1::bytea,$2::uuid,$3::uuid,$4::text,$5::text,$6::bytea,$7::jsonb)`,
			tokenHash[:], storeID, in.OrderID, s.starter.profile, in.Locale, s.configDigest[:], body); err != nil {
			return err
		}
		if err := checkCapability(callCtx, tx, tokenHash[:], storeID, scope); err != nil {
			return err
		}
		return checkHostedDeadline(callCtx, tx, built.ExpiresAt)
	})
	if err != nil {
		return PaymentResult{}, safeError(ctx, err)
	}
	return out, nil
}

// TakeHosted releases a form only after the database has committed the one-shot
// transition. Any uncertain response must be reconciled, never taken again.
func (s *HostedPaymentStarter) TakeHosted(ctx context.Context, token, storeID, orderID string) (HostedHandoff, error) {
	if ctx == nil || s == nil || s.starter.pool == nil || !validPaymentProfile(s.starter.profile) || !command.ValidID(orderID) {
		return HostedHandoff{}, command.ErrInvalid
	}
	tokenHash := sha256.Sum256([]byte(token))
	var out HostedHandoff
	err := buyer.WithScope(ctx, s.starter.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		if s.stripe != nil {
			stripeOrder, err := s.isStripeOrder(callCtx, tx, tokenHash[:], storeID, orderID)
			if err != nil {
				return err
			}
			if stripeOrder {
				return s.takeStripeTx(callCtx, tx, scope, tokenHash[:], storeID, orderID, &out)
			}
		}
		if !s.payuni {
			return command.ErrConflict
		}
		var body []byte
		if err := tx.QueryRow(callCtx, `SELECT checkout.take_hosted_page($1::bytea,$2::uuid,$3::uuid,$4::text,$5::bytea)`,
			tokenHash[:], storeID, orderID, s.starter.profile, s.configDigest[:]).Scan(&body); err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&out); err != nil || !validHostedHandoff(out, orderID, s.starter.profile) {
			return command.ErrConflict
		}
		if err := checkCapability(callCtx, tx, tokenHash[:], storeID, scope); err != nil {
			return err
		}
		if out.Disposition == "ISSUED" {
			return checkHostedDeadline(callCtx, tx, out.ExpiresAt)
		}
		return nil
	})
	if err != nil {
		return HostedHandoff{}, safeError(ctx, err)
	}
	return out, nil
}

func validHostedInput(in HostedInput) bool {
	// validPaymentInput is PAYUNi-only; Stripe reuses its id/version rules under the PAYUNi code.
	return (in.MethodCode == "payuni_credit" || in.MethodCode == stripeMethodCode) &&
		validPaymentInput(PaymentInput{OrderID: in.OrderID, MethodCode: "payuni_credit",
			MethodVersion: in.MethodVersion}) &&
		(in.Locale == "zh-CN" || in.Locale == "zh-TW" || in.Locale == "en")
}

func boundedHostedForm(raw payuni.HostedForm, profile string) (HostedForm, error) {
	form := HostedForm{Action: raw.Action, Fields: make(map[string]string, 4)}
	if len(raw.Fields) != 4 {
		return HostedForm{}, command.ErrConflict
	}
	for key, values := range raw.Fields {
		if len(values) != 1 {
			return HostedForm{}, command.ErrConflict
		}
		form.Fields[key] = values[0]
	}
	if !validHostedForm(form, profile) {
		return HostedForm{}, command.ErrConflict
	}
	return form, nil
}

func validHostedForm(form HostedForm, profile string) bool {
	if !validPaymentProfile(profile) {
		return false
	}
	action := "https://sandbox-api.payuni.com.tw/api/upp"
	if profile == "LIVE" {
		action = "https://api.payuni.com.tw/api/upp"
	}
	if form.Action != action || len(form.Fields) != 4 || form.Fields["Version"] != "2.0" {
		return false
	}
	merchant := form.Fields["MerID"]
	if len(merchant) < 1 || len(merchant) > 64 {
		return false
	}
	for _, ch := range merchant {
		if ch != '-' && ch != '_' && (ch < 'a' || ch > 'z') &&
			(ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return false
		}
	}
	encrypted := form.Fields["EncryptInfo"]
	if len(encrypted) < 16 || len(encrypted) > 24576 || len(encrypted)%2 != 0 {
		return false
	}
	for _, ch := range encrypted {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	hash := form.Fields["HashInfo"]
	if len(hash) != 64 {
		return false
	}
	for _, ch := range hash {
		if (ch < '0' || ch > '9') && (ch < 'A' || ch > 'F') {
			return false
		}
	}
	return true
}

func checkHostedDeadline(ctx context.Context, tx pgx.Tx, deadline time.Time) error {
	var current bool
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() < $1::timestamptz`, deadline).Scan(&current); err != nil {
		return err
	}
	if !current {
		return command.ErrConflict
	}
	return nil
}

func validHostedHandoff(out HostedHandoff, orderID, profile string) bool {
	if out.OrderID != orderID || out.ExpiresAt.IsZero() || out.RedirectURL != "" {
		return false
	}
	switch out.Disposition {
	case "ISSUED":
		return out.Form != nil && validHostedForm(*out.Form, profile)
	case "ALREADY_ISSUED":
		return out.Form == nil
	default:
		return false
	}
}
