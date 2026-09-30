package metareply

import (
	"errors"
	"strings"
	"testing"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
)

func testToken(t *testing.T) claims.LinkToken {
	t.Helper()
	k, _ := claims.NewReplyLinkKey(bytes32(5))
	tok, err := claims.SystemLinkToken(k, tenantT, storeT, bindingT, storeT)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func bytes32(fill byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = fill
	}
	return b
}

func TestRenderClaimLink(t *testing.T) {
	tok := testToken(t)
	want := map[string]string{
		"zh-TW": "感謝留言！點此確認你的喊單： https://shop.example.com/zh-TW/claim#t=" + string(tok),
		"zh-CN": "感谢留言！点此确认你的下单： https://shop.example.com/zh-CN/claim#t=" + string(tok),
		"en":    "Thanks for your comment! Confirm your claim here: https://shop.example.com/en/claim#t=" + string(tok),
	}
	for locale, text := range want {
		for _, origin := range []string{"https://shop.example.com", "shop.example.com"} {
			got, err := RenderClaimLink(locale, origin, tok)
			if err != nil || got != text {
				t.Fatalf("%s/%s: %q %v", locale, origin, got, err)
			}
		}
	}
	for name, args := range map[string][3]string{
		"locale":   {"fr", "https://shop.example.com", string(tok)},
		"http":     {"en", "http://shop.example.com", string(tok)},
		"path":     {"en", "https://shop.example.com/x", string(tok)},
		"port":     {"en", "https://shop.example.com:8443", string(tok)},
		"userinfo": {"en", "https://a@shop.example.com", string(tok)},
		"upper":    {"en", "https://Shop.example.com", string(tok)},
		"single":   {"en", "https://localhost", string(tok)},
		"empty":    {"en", "", string(tok)},
		"token":    {"en", "https://shop.example.com", "short"},
	} {
		if _, err := RenderClaimLink(args[0], args[1], claims.LinkToken(args[2])); !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	got, _ := RenderClaimLink("en", "https://shop.example.com", tok)
	if strings.Contains(got, "?") {
		t.Fatal("token must live in the fragment, never a query")
	}
}
