package meta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func socialFixture(t *testing.T, object, asset, source string) (payloadContext, Event, []byte) {
	t.Helper()
	unit, err := ParseStrict([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{appID: "123", object: object}
	b := &Batch{}
	if _, ok := unit["sender"]; ok {
		err = v.message(b, asset, nil, unit, nil)
	} else {
		err = v.change(b, asset, nil, unit, nil)
	}
	if err != nil || len(b.Events) != 1 {
		t.Fatalf("fixture classification: %v", err)
	}
	event := b.Events[0]
	c := payloadContext{Class: "event", ID: payloadTestID, AppID: "123", Object: object,
		EventKey: event.Key, PayloadHash: event.PayloadHash, TenantID: payloadTestID,
		StoreID: payloadTestID, RouteID: payloadTestID, RouteEpoch: 1}
	return c, event, append([]byte(nil), event.Payload...)
}

func TestProjectSocialAllSevenKindsAndScopedIdentity(t *testing.T) {
	cases := []struct {
		name, object, asset, kind, source, identity string
	}{
		{"page add", "page", "9", "page_comment_add", `{"field":"feed","value":{"item":"comment","verb":"add","comment_id":"c1","message":"你好 😀","extra":{"media":"image"}}}`, "c1"},
		{"page edit", "page", "9", "page_comment_edit", `{"field":"feed","value":{"item":"comment","verb":"edit","comment_id":"c1","message":"edited"}}`, "c1"},
		{"page remove", "page", "9", "page_comment_remove", `{"field":"feed","value":{"item":"comment","verb":"remove","comment_id":"c1"}}`, "c1"},
		{"instagram comment", "instagram", "9", "instagram_comment", `{"field":"comments","value":{"id":"22","text":"你好 😀","attachments":[{"type":"image"}]}}`, "22"},
		{"instagram live comment", "instagram", "9", "instagram_live_comment", `{"field":"live_comments","value":{"id":"22","text":"live"}}`, "22"},
		{"page message", "page", "9", "page_message", `{"sender":{"id":"4"},"recipient":{"id":"9"},"message":{"mid":"m.1","text":"你好 😀","attachments":[{"type":"image"}]}}`, "4"},
		{"instagram message", "instagram", "9", "instagram_message", `{"sender":{"id":"4"},"recipient":{"id":"9"},"message":{"mid":"m.1","text":"hello"}}`, "4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, event, plaintext := socialFixture(t, tc.object, tc.asset, tc.source)
			if event.Kind != tc.kind || event.QuarantineReason != "" {
				t.Fatal("fixture did not produce requested event")
			}
			got, err := projectSocial(c, tc.asset, tc.kind, plaintext)
			if err != nil {
				t.Fatal(err)
			}
			wantFamily, namespace := "comment", "meta-social-comment/v1"
			if strings.Contains(tc.kind, "message") {
				wantFamily, namespace = "message", "meta-social-peer/v1"
			}
			if got.family != wantFamily || got.subjectKey != tupleHash(namespace, c.AppID, c.Object, tc.asset, tc.identity) {
				t.Fatal("family or scoped identity mismatch")
			}
			if again, err := projectSocial(c, tc.asset, tc.kind, plaintext); err != nil || again != got {
				t.Fatal("projection identity not stable")
			}
		})
	}
}

func TestProjectSocialRejectsWrongEvidenceAndQuarantine(t *testing.T) {
	c, event, plaintext := socialFixture(t, "page", "9", `{"sender":{"id":"4"},"recipient":{"id":"9"},"message":{"mid":"m.1","text":"private"}}`)
	mutations := []struct {
		name  string
		ctx   payloadContext
		asset string
		kind  string
		body  []byte
	}{
		{"class", func() payloadContext { x := c; x.Class = "quarantine"; return x }(), "9", event.Kind, plaintext},
		{"app", func() payloadContext { x := c; x.AppID = "124"; return x }(), "9", event.Kind, plaintext},
		{"object", func() payloadContext { x := c; x.Object = "instagram"; return x }(), "9", event.Kind, plaintext},
		{"asset", c, "10", event.Kind, plaintext},
		{"kind", c, "9", "instagram_message", plaintext},
		{"unknown kind", c, "9", "other", plaintext},
		{"key", func() payloadContext { x := c; x.EventKey = digest([]byte("other")); return x }(), "9", event.Kind, plaintext},
		{"hash", func() payloadContext { x := c; x.PayloadHash = digest([]byte("other")); return x }(), "9", event.Kind, plaintext},
		{"noncanonical", c, "9", event.Kind, append([]byte(" "), plaintext...)},
		{"echo", func() payloadContext {
			x := c
			x.PayloadHash = digest([]byte(`{"message":{"is_echo":true,"mid":"m.1"},"recipient":{"id":"9"},"sender":{"id":"4"}}`))
			return x
		}(), "9", event.Kind, []byte(`{"message":{"is_echo":true,"mid":"m.1"},"recipient":{"id":"9"},"sender":{"id":"4"}}`)},
		{"invalid json", c, "9", event.Kind, []byte(`{"sender":`)},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			got, err := projectSocial(tc.ctx, tc.asset, tc.kind, tc.body)
			if !errors.Is(err, ErrConsumerPayload) || got != (socialProjection{}) {
				t.Fatal("wrong evidence accepted")
			}
		})
	}
	// A different message with the same text but another MID keeps its peer
	// identity while retaining a different durable event identity.
	c2, e2, p2 := socialFixture(t, "page", "9", `{"sender":{"id":"4"},"recipient":{"id":"9"},"message":{"mid":"m.2","text":"private"}}`)
	a, _ := projectSocial(c, "9", event.Kind, plaintext)
	b, err := projectSocial(c2, "9", e2.Kind, p2)
	if err != nil || a.subjectKey != b.subjectKey || event.Key == e2.Key {
		t.Fatal("peer and message identity conflated")
	}
	if a.subjectKey == tupleHash("meta-social-peer/v1", c.AppID, c.Object, "10", "4") ||
		a.subjectKey == tupleHash("meta-social-peer/v1", "124", c.Object, "9", "4") {
		t.Fatal("scope was omitted from peer identity")
	}
}

func TestSocialProjectionRedaction(t *testing.T) {
	p := socialProjection{"message", strings.Repeat("a", 64)}
	for _, rendered := range []string{fmt.Sprint(p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p)} {
		if strings.Contains(rendered, p.subjectKey) || !strings.Contains(rendered, "redacted") {
			t.Fatal("projection formatting exposed identity")
		}
	}
	b, err := json.Marshal(p)
	if err != nil || bytes.Contains(b, []byte(p.subjectKey)) || !bytes.Contains(b, []byte("redacted")) {
		t.Fatal("projection JSON exposed identity")
	}
}
