// manual.go owns RecordManualClaim, the MOCK operator ingress: a merchant records what a
// buyer commented, for an existing manual bundle or a new manual actor (contract §4.2).
//
// Non-goals: no automatic comment reading, no identity edge, consent or message window
// (a social actor is not a verified customer), no storage of the typed text anywhere, and
// no label outside claims.bundles.label.
//
// Privacy: the receipt request carries only principal, session, bundle id or a keyed
// label MAC, grammar version/kind, resolved offer id and quantity/explicit. Text, label
// and any unkeyed hash of either never reach the request digest, receipt, audit or logs.

package claims

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// ManualClaimInput is the M5 body (redacted: it carries comment text and a label).
type ManualClaimInput struct {
	BundleID   string `json:"bundle_id"`   // append to an existing manual bundle of this session, or
	ActorLabel string `json:"actor_label"` // a new manual actor; exactly one of the two is non-empty
	Text       string `json:"text"`        // valid UTF-8, 1..256 bytes, no control characters
}

// Redaction: ManualClaimInput carries comment text and a label (decoding is unaffected).
func (ManualClaimInput) String() string               { return redacted }
func (ManualClaimInput) GoString() string             { return redacted }
func (ManualClaimInput) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (ManualClaimInput) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// ManualClaimResult is also the command receipt, so it never holds text, label or actor key.
type ManualClaimResult struct {
	Outcome          string `json:"outcome"`           // ACCEPTED | REJECTED
	Reason           Reason `json:"reason"`            // "" when ACCEPTED
	OfferID          string `json:"offer_id"`          // "" unless offer-resolved
	Keyword          string `json:"keyword"`           // the offer's keyword; "" unless offer-resolved
	Quantity         int64  `json:"quantity"`          // 0 unless stored per §3.1
	PreviousQuantity int64  `json:"previous_quantity"` // 0 = new line or not accepted
	BundleID         string `json:"bundle_id"`         // "" unless ACCEPTED
	BundleVersion    int64  `json:"bundle_version"`
	LineVersion      int64  `json:"line_version"`
} // no label, text or actor key: this is the command receipt

// RecordManualClaim records one operator-attested comment (live:manage) in a
// platform.WithScope transaction: receipt live.claim.manual, audit
// live.claim.manual.recorded, then IngestParsed with a fresh random source event at the
// database clock. REJECTED and WINDOW_CLOSED are successful results, not errors. The same
// key with a different actor label, text parse or resolved offer is a different canonical
// request (ErrConflict). A label already used in this session is ErrConflict (pick the
// existing bundle). labels is the server-held COMMERCE_CLAIMS_LABEL_KEY. Called by M5.
func RecordManualClaim(ctx context.Context, tx pgx.Tx, scope platform.Scope, labels LabelKey, token, key, sessionID string, in ManualClaimInput) (ManualClaimResult, error) {
	label, err := validManual(labels, sessionID, in)
	if err != nil {
		return ManualClaimResult{}, err
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return ManualClaimResult{}, err
	}
	mac := ""
	if label != "" {
		mac = labelMAC(labels, scope.TenantID, scope.StoreID, sessionID, label)
	}
	p := grammar.Parse(in.Text)
	// The offer id is resolved only for the canonical request (keywords are immutable);
	// the ingest below re-reads and locks the offer itself.
	offerID := ""
	if p.Kind != grammar.NoMatch {
		err := tx.QueryRow(ctx, `SELECT id::text FROM live.offers WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND keyword=$4`,
			scope.TenantID, scope.StoreID, sessionID, p.Keyword).Scan(&offerID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ManualClaimResult{}, mapError(err)
		}
	}
	request := struct {
		PrincipalID    string `json:"principal_id"`
		SessionID      string `json:"session_id"`
		BundleID       string `json:"bundle_id"`
		LabelMAC       string `json:"label_mac"`
		GrammarVersion string `json:"grammar_version"`
		Kind           string `json:"kind"`
		OfferID        string `json:"offer_id"`
		Quantity       int64  `json:"quantity"`
		Explicit       bool   `json:"explicit"`
	}{scope.PrincipalID, sessionID, in.BundleID, mac, p.Version, string(p.Kind), offerID, p.Quantity, p.Explicit}
	var out ManualClaimResult
	err = command.Run(ctx, tx, scope, "live.claim.manual", key, request, &out, func() error {
		if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
			return err
		}
		if err := requireSession(ctx, tx, scope, sessionID); err != nil {
			return err
		}
		actorKey, err := manualActor(ctx, tx, scope, sessionID, in.BundleID, label)
		if err != nil {
			return err
		}
		var sourceEventID string
		var occurredAt time.Time
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text,clock_timestamp()`).Scan(&sourceEventID, &occurredAt); err != nil {
			return mapError(err)
		}
		result, err := IngestParsed(ctx, tx, IngestInput{TenantID: scope.TenantID, StoreID: scope.StoreID, SessionID: sessionID,
			SourceKind: manualSource, SourceEventID: sourceEventID, Platform: manualSource, ActorKey: actorKey,
			ActorLabel: label, PrincipalID: scope.PrincipalID, OccurredAt: occurredAt}, p)
		if err != nil {
			return err
		}
		out = ManualClaimResult{Outcome: result.Outcome, Reason: result.Reason, OfferID: result.OfferID, Keyword: result.Keyword,
			Quantity: result.Quantity, PreviousQuantity: result.PreviousQuantity, BundleID: result.BundleID,
			BundleVersion: result.BundleVersion, LineVersion: result.LineVersion}
		return command.Audit(ctx, tx, scope, "live.claim.manual.recorded")
	})
	if err != nil {
		return ManualClaimResult{}, mapError(err)
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return ManualClaimResult{}, err
	}
	return out, nil
}

// validManual checks the pure input rules and returns the normalized label ("" for the
// BundleID form): a usable LabelKey, a canonical session UUID, exactly one of
// bundle_id/actor_label, a NormalizeLabel-valid label, and text of 1..256 bytes of valid
// UTF-8 without control characters. Everything else is command.ErrInvalid.
func validManual(labels LabelKey, sessionID string, in ManualClaimInput) (string, error) {
	if !labels.set || !command.ValidID(sessionID) || (in.BundleID == "") == (in.ActorLabel == "") ||
		len(in.Text) < 1 || len(in.Text) > grammar.MaxTextBytes || !utf8.ValidString(in.Text) {
		return "", command.ErrInvalid
	}
	for _, r := range in.Text {
		if unicode.IsControl(r) {
			return "", command.ErrInvalid
		}
	}
	if in.BundleID != "" {
		if !command.ValidID(in.BundleID) {
			return "", command.ErrInvalid
		}
		return "", nil
	}
	label, ok := grammar.NormalizeLabel(in.ActorLabel)
	if !ok {
		return "", command.ErrInvalid
	}
	return label, nil
}

// manualActor resolves the actor key inside the receipt: an existing manual bundle of this
// session reuses its key (ErrNotFound otherwise); a new label gets a fresh server-random
// key unless the label is already used in the session (ErrConflict). claims.bundles is
// read without a lock here; IngestParsed locks the bundle.
func manualActor(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID, bundleID, label string) (string, error) {
	if bundleID != "" {
		var actorKey string
		err := tx.QueryRow(ctx, `SELECT actor_key FROM claims.bundles
			WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND id=$4 AND platform='manual'`,
			scope.TenantID, scope.StoreID, sessionID, bundleID).Scan(&actorKey)
		if err != nil {
			return "", mapError(err)
		}
		return actorKey, nil
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM claims.bundles
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND label=$4)`,
		scope.TenantID, scope.StoreID, sessionID, label).Scan(&used); err != nil {
		return "", mapError(err)
	}
	if used {
		return "", command.ErrConflict
	}
	return newActorKey(), nil
}
