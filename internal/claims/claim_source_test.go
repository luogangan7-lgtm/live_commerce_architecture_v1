package claims

import (
	"errors"
	"strings"
	"testing"

	"livecommerce/internal/command"
)

// TestParseClaimSourceInput is the frozen parser table of docs/delivery/units/claim-source.md.
func TestParseClaimSourceInput(t *testing.T) {
	ok := []struct {
		in   string
		want ClaimSourceRef
	}{
		{"1234567890", ClaimSourceRef{Item: "1234567890"}},
		{"  1234567890 \n", ClaimSourceRef{Item: "1234567890"}},
		{"111_222", ClaimSourceRef{Platform: "facebook", Page: "111", Item: "222"}}, // only Facebook delivers <page>_<post>
		{"https://www.facebook.com/somepage/posts/987654321", ClaimSourceRef{Platform: "facebook", Item: "987654321"}},
		{"https://facebook.com/111222333/posts/987654321?comment_id=5&fbclid=x#frag", ClaimSourceRef{Platform: "facebook", Page: "111222333", Item: "987654321"}},
		{"facebook.com/some.page-1/videos/555444333", ClaimSourceRef{Platform: "facebook", Item: "555444333"}},
		{"https://m.facebook.com/somepage/videos/555444333/", ClaimSourceRef{Platform: "facebook", Item: "555444333"}},
		{"https://web.facebook.com/somepage/posts/1", ClaimSourceRef{Platform: "facebook", Item: "1"}},
		{"HTTPS://WWW.FACEBOOK.COM/somepage/posts/2", ClaimSourceRef{Platform: "facebook", Item: "2"}},
	}
	for _, c := range ok {
		got, err := ParseClaimSourceInput(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: got %+v %v, want %+v", c.in, got, err, c.want)
		}
	}
	unresolvable := []string{
		"https://fb.watch/abcDEF123/",
		"fb.watch/x",
		"https://www.facebook.com/somepage/posts/pfbid02abcdefghijklmnopqrstuvwxyz",
		"https://www.instagram.com/p/CxYz123AbC/",
		"https://instagram.com/reel/CxYz123AbC",
		"https://www.instagram.com/someuser/p/CxYz123AbC/?igsh=1",
		"instagram.com/tv/CxYz123AbC",
	}
	for _, in := range unresolvable {
		if _, err := ParseClaimSourceInput(in); !errors.Is(err, ErrInputUnresolvable) {
			t.Errorf("%q: %v, want input_unresolvable", in, err)
		}
	}
	invalid := []string{
		"", "   ", "abc", "12 34", "12a", "_12", "12_", "1_2_3",
		strings.Repeat("1", 41), strings.Repeat("1", 1025),
		"http://www.facebook.com/somepage/posts/1",          // cleartext
		"https://user:pw@www.facebook.com/somepage/posts/1", // userinfo
		"https://www.facebook.com:8443/somepage/posts/1",    // port
		"https://www.facebook.com/somepage/photos/1",
		"https://www.facebook.com/somepage/posts/",
		"https://www.facebook.com/somepage/posts/12ab",
		"https://www.facebook.com/somepage",
		"https://www.facebook.com/groups/1/posts/2/extra",
		"https://www.facebook.com/bad%20page/posts/2",
		"https://evil.example.com/somepage/posts/1",
		"https://facebook.com.evil.example.com/somepage/posts/1",
		"https://www.instagram.com/someuser/",
		"https://www.instagram.com/p/",
		"javascript:alert(1)",
		"https://www.facebook.com/somepage/posts/" + strings.Repeat("1", 40),
		"\x00123",
		"123\xff",
	}
	for _, in := range invalid {
		if got, err := ParseClaimSourceInput(in); !errors.Is(err, ErrInputInvalid) {
			t.Errorf("%q: got %+v %v, want input_invalid", in, got, err)
		}
	}
}

func TestObjectFor(t *testing.T) {
	cases := []struct {
		name, provider, asset string
		ref                   ClaimSourceRef
		object, id            string
		err                   error
	}{
		{"fb bare item composes the delivered post id", "facebook", "111", ClaimSourceRef{Item: "222"}, "page", "111_222", nil},
		{"fb delivered post id of this page", "facebook", "111", ClaimSourceRef{Page: "111", Item: "222"}, "page", "111_222", nil},
		{"fb url of the same numeric page", "facebook", "111", ClaimSourceRef{Platform: "facebook", Page: "111", Item: "222"}, "page", "111_222", nil},
		{"fb other page is invalid", "facebook", "111", ClaimSourceRef{Page: "999", Item: "222"}, "", "", ErrInputInvalid},
		{"instagram url on a facebook binding", "facebook", "111", ClaimSourceRef{Platform: "instagram", Item: "2"}, "", "", ErrInputInvalid},
		{"ig bare media id", "instagram", "555", ClaimSourceRef{Item: "17900000000000001"}, "instagram", "17900000000000001", nil},
		{"ig with a page part is invalid", "instagram", "555", ClaimSourceRef{Page: "1", Item: "2"}, "", "", ErrInputInvalid},
		{"facebook url on an instagram binding", "instagram", "555", ClaimSourceRef{Platform: "facebook", Item: "2"}, "", "", ErrInputInvalid},
	}
	for _, c := range cases {
		object, id, err := objectFor(c.ref, c.provider, c.asset)
		if object != c.object || id != c.id || !errors.Is(err, c.err) && err != c.err {
			t.Errorf("%s: got (%q,%q,%v) want (%q,%q,%v)", c.name, object, id, err, c.object, c.id, c.err)
		}
	}
}

// Ruling p: the optional platform hint fills a bare id, must agree with a parsed platform, and has a closed set.
func TestWithPlatform(t *testing.T) {
	bare := ClaimSourceRef{Item: "17900000000000001"}
	fbURL := ClaimSourceRef{Platform: "facebook", Item: "2"}
	postID, _ := ParseClaimSourceInput("111_222") // a re-saved Facebook source_object_id
	cases := []struct {
		name, platform string
		ref, want      ClaimSourceRef
		err            error
	}{
		{"no hint keeps a bare id open", "", bare, bare, nil},
		{"instagram hint on a bare id", "instagram", bare, ClaimSourceRef{Platform: "instagram", Item: bare.Item}, nil},
		{"facebook hint on a bare id", "facebook", bare, ClaimSourceRef{Platform: "facebook", Item: bare.Item}, nil},
		{"agreeing hint", "facebook", fbURL, fbURL, nil},
		{"re-saved facebook post id with its platform", "facebook", postID, postID, nil},
		{"contradicting hint", "instagram", fbURL, ClaimSourceRef{}, ErrInputInvalid},
		{"instagram hint on a facebook post id", "instagram", postID, ClaimSourceRef{}, ErrInputInvalid},
		{"unknown platform", "tiktok", bare, ClaimSourceRef{}, command.ErrInvalid},
		{"case matters", "Facebook", bare, ClaimSourceRef{}, command.ErrInvalid},
	}
	for _, c := range cases {
		got, err := withPlatform(c.ref, c.platform)
		if got != c.want || !errors.Is(err, c.err) && err != c.err {
			t.Errorf("%s: got (%+v,%v) want (%+v,%v)", c.name, got, err, c.want, c.err)
		}
	}
}

// The stored id must satisfy live.claim_sources ^[0-9_]{1,80}$: 40 + "_" + 39 is the largest that fits.
func TestObjectForHonorsTheDatabaseBound(t *testing.T) {
	if _, id, err := objectFor(ClaimSourceRef{Item: strings.Repeat("2", 39)}, "facebook", strings.Repeat("1", 40)); err != nil || len(id) != 80 {
		t.Fatalf("%d %v", len(id), err)
	}
	if _, _, err := objectFor(ClaimSourceRef{Item: strings.Repeat("2", 40)}, "facebook", strings.Repeat("1", 40)); !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("81 characters accepted: %v", err)
	}
}
