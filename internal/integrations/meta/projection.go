package meta

import (
	"bytes"
	"errors"
)

var ErrConsumerPayload = errors.New("meta: consumer payload unavailable")

// A projection contains scoped indexes only. It must never print provider IDs.
type socialProjection struct{ family, subjectKey string }

func (socialProjection) String() string     { return "meta.socialProjection{redacted}" }
func (p socialProjection) GoString() string { return p.String() }
func (socialProjection) MarshalJSON() ([]byte, error) {
	return []byte(`"meta.socialProjection{redacted}"`), nil
}

// projectSocial replays the already authenticated single unit through the
// original classifier. The persisted event identity must agree byte for byte.
func projectSocial(c payloadContext, assetID, kind string, plaintext []byte) (socialProjection, error) {
	if c.Class != "event" || !c.valid() || !digits(assetID) || len(plaintext) < 1 ||
		len(plaintext) > maxPayload || digest(plaintext) != c.PayloadHash {
		return socialProjection{}, ErrConsumerPayload
	}
	var family string
	switch kind {
	case "page_comment_add", "page_comment_edit", "page_comment_remove":
		if c.Object != "page" {
			return socialProjection{}, ErrConsumerPayload
		}
		family = "comment"
	case "instagram_comment", "instagram_live_comment":
		if c.Object != "instagram" {
			return socialProjection{}, ErrConsumerPayload
		}
		family = "comment"
	case "page_message":
		if c.Object != "page" {
			return socialProjection{}, ErrConsumerPayload
		}
		family = "message"
	case "instagram_message":
		if c.Object != "instagram" {
			return socialProjection{}, ErrConsumerPayload
		}
		family = "message"
	default:
		return socialProjection{}, ErrConsumerPayload
	}
	unit, err := ParseStrict(plaintext)
	if err != nil || !bytes.Equal(canonical(unit), plaintext) {
		return socialProjection{}, ErrConsumerPayload
	}
	v := &Verifier{appID: c.AppID, object: c.Object}
	b := &Batch{}
	entry := map[string]any{"id": assetID}
	if family == "comment" {
		err = v.change(b, assetID, entry, unit, nil)
	} else {
		err = v.message(b, assetID, entry, unit, nil)
	}
	if err != nil || len(b.Events) != 1 {
		return socialProjection{}, ErrConsumerPayload
	}
	event := b.Events[0]
	if event.QuarantineReason != "" || event.AssetID != assetID || event.Kind != kind ||
		event.Key != c.EventKey || event.PayloadHash != c.PayloadHash ||
		!bytes.Equal(event.Payload, plaintext) {
		return socialProjection{}, ErrConsumerPayload
	}
	if family == "comment" {
		return socialProjection{family, tupleHash("meta-social-comment/v1", c.AppID, c.Object, assetID, event.ExternalID)}, nil
	}
	sender, ok := object(unit["sender"])
	if !ok {
		return socialProjection{}, ErrConsumerPayload
	}
	return socialProjection{family, tupleHash("meta-social-peer/v1", c.AppID, c.Object, assetID, stringField(sender, "id"))}, nil
}
