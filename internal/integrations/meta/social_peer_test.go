package meta

import "testing"

// TestSocialPeerKeyVector pins SocialPeerKey to the tupleHash format the projection writes: a
// fixed literal (computed once from tupleHash) and the direct tupleHash call must both match.
func TestSocialPeerKeyVector(t *testing.T) {
	got := SocialPeerKey("1", "page", "55", "77")
	if want := tupleHash("meta-social-peer/v1", "1", "page", "55", "77"); got != want {
		t.Fatalf("SocialPeerKey %s != tupleHash %s", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("length %d", len(got))
	}
	if other := SocialPeerKey("1", "page", "55", "78"); other == got {
		t.Fatal("different sender, same key")
	}
	if other := SocialPeerKey("2", "page", "55", "77"); other == got {
		t.Fatal("different app, same key")
	}
	const literal = "37d6217ed00ead2c637b8ad1e58625d548715776d7171595b25e39244dbf34e5"
	if got != literal {
		t.Fatalf("vector drift: %s", got)
	}
}
