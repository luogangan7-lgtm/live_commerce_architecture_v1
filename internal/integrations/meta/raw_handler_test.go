package meta

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthenticatedRawHandoff(t *testing.T) {
	v, err := NewVerifier(Config{AppID: "17", Object: "page", AppSecret: "synthetic-test-secret", VerifyToken: "synthetic-verify-token"})
	if err != nil {
		t.Fatal(err)
	}
	// Exact bytes include whitespace/escapes and more than one asset. This body
	// must never become one tenant's scoped payload; it is restricted raw evidence.
	raw := " {\n\"object\":\"page\",\"entry\":[{\"id\":\"100\",\"messaging\":[{\"sender\":{\"id\":\"200\"},\"recipient\":{\"id\":\"100\"},\"message\":{\"mid\":\"m-1\",\"text\":\"\\u4f60\"}}]},{\"id\":\"101\",\"changes\":[]}]} \n"
	mac := hmac.New(sha256.New, []byte("synthetic-test-secret"))
	_, _ = mac.Write([]byte(raw))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	calls := 0
	fail := false
	h, err := newRawHandler(v, func(_ context.Context, batch Batch, owned []byte) error {
		calls++
		if string(owned) != raw || batch.BodyHash != digest(owned) || len(batch.Events) != 2 {
			t.Fatal("raw envelope changed or siblings lost before persistence")
		}
		if batch.Events[0].AssetID == batch.Events[1].AssetID {
			t.Fatal("mixed assets were combined")
		}
		payload := append([]byte(nil), batch.Events[0].Payload...)
		owned[0] = '!'
		if !bytes.Equal(payload, batch.Events[0].Payload) {
			t.Fatal("canonical payload aliases raw buffer")
		}
		if fail {
			return errors.New("sensitive-storage-details-must-not-leak")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(signature string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Hub-Signature-256", signature)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(sig); w.Code != 200 || calls != 1 {
		t.Fatalf("valid handoff code=%d calls=%d", w.Code, calls)
	}
	if w := request("sha256=" + strings.Repeat("0", 64)); w.Code != 403 || calls != 1 {
		t.Fatalf("unverified raw reached callback code=%d calls=%d", w.Code, calls)
	}
	fail = true
	if w := request(sig); w.Code != 503 || calls != 2 || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatalf("failed raw persistence not sanitized code=%d calls=%d", w.Code, calls)
	}
	if _, err := newRawHandler(v, nil); err == nil {
		t.Fatal("nil raw callback admitted")
	}
	if _, err := newRawHandler(&Verifier{}, func(context.Context, Batch, []byte) error { return nil }); err == nil {
		t.Fatal("zero verifier admitted")
	}
}

// S3: the POST path has a bounded non-blocking in-flight admission and a body read deadline.
func TestPostAdmissionIsBounded(t *testing.T) {
	v, err := NewVerifier(Config{AppID: "17", Object: "page", AppSecret: "synthetic-test-secret", VerifyToken: "synthetic-verify-token"})
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"object":"page","entry":[{"id":"100","changes":[]}]}`
	mac := hmac.New(sha256.New, []byte("synthetic-test-secret"))
	_, _ = mac.Write([]byte(raw))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	block := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	var mu sync.Mutex
	commits := 0
	h, err := newLimitedRawHandler(v, func(context.Context, Batch, []byte) error {
		mu.Lock()
		commits++
		mu.Unlock()
		<-block
		return nil
	}, 2, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer unblock()                                  // runs before srv.Close so a failing run cannot hang on a blocked commit
	client := &http.Client{Timeout: 2 * time.Second} // a blocked commit must fail the test, not hang it
	post := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", sig)
		return client.Do(req)
	}

	// Stalled bodies (declared, never sent) neither hold a slot nor outlive the read deadline.
	var stalled []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nX-Hub-Signature-256: %s\r\nContent-Length: 4096\r\n\r\n", sig)
		stalled = append(stalled, c)
	}
	defer func() {
		for _, c := range stalled {
			_ = c.Close()
		}
	}()
	_ = stalled[0].SetReadDeadline(time.Now().Add(3 * time.Second))
	if st, err := bufio.NewReader(stalled[0]).ReadString('\n'); err != nil || !strings.Contains(st, "400") {
		t.Fatalf("stalled body not cut by the read deadline: %q %v", st, err)
	}

	// Fill both slots, then the third fully sent request is refused without reaching commit.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := post(); err == nil {
				resp.Body.Close()
			}
		}()
	}
	for start := time.Now(); time.Since(start) < 3*time.Second; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n := commits
		mu.Unlock()
		if n == 2 {
			break
		}
	}
	resp, err := post()
	if err != nil {
		t.Fatalf("third concurrent request was admitted and blocked in commit instead of 503: %v", err)
	}
	resp.Body.Close()
	mu.Lock()
	n := commits
	mu.Unlock()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "5" || n != 2 {
		t.Fatalf("third concurrent request: code=%d retry-after=%q commits=%d, want 503/5/2", resp.StatusCode, resp.Header.Get("Retry-After"), n)
	}
	unblock()
	wg.Wait()
}
