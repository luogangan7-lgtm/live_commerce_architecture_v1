package livekit

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic DSN sentinels live in their own constants so no source line looks like a
// credential to secret scanners (GitGuardian false positives 2026-09-29); they are test sentinels.
const (
	dsnSentinel1 = "secret"
)

func workerTestVars(ca, dial string) map[string]string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	return map[string]string{
		"COMMERCE_MEDIA_WORKER_ENABLED": "1", "COMMERCE_MEDIA_WORKER_DATABASE_URL": "postgres://worker:" + dsnSentinel1 + "@localhost/db",
		"COMMERCE_MEDIA_EXECUTOR_DATABASE_URL": "postgres://executor:" + dsnSentinel1 + "@localhost/db", "COMMERCE_MEDIA_WORKER_CONCURRENCY": "32",
		"COMMERCE_MEDIA_MATERIAL_ACTIVE_KEY_ID": "k1",
		"COMMERCE_MEDIA_MATERIAL_KEYS_JSON":     fmt.Sprintf(`{"keys":[{"id":"k1","key_base64":%q}]}`, key),
		"COMMERCE_MEDIA_PROJECTS_JSON":          fmt.Sprintf(`{"projects":[{"project_id":"p1","credential_version":1,"endpoint":"https://unit.livekit.cloud","api_key":"test_key","api_secret":%q,"stream_hosts":["ingest.example.com"],"mock_dial_address":%q,"mock_ca_pem":%q}]}`, strings.Repeat("s", 40), dial, ca),
	}
}

func workerLoad(v map[string]string) (WorkerEnvironment, error) {
	return LoadWorkerEnvironment(func(k string) string { return v[k] })
}

func TestWorkerEnvironmentLMW01StrictBoundaryAndRedaction(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	// A fixture server's DER leaf is a valid nonempty PEM trust root for parsing.
	ca := string(pemCert(server.TLS.Certificates[0].Certificate[0]))
	v := workerTestVars(ca, server.Listener.Addr().String())
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	duplicateKey := strings.Replace(v["COMMERCE_MEDIA_MATERIAL_KEYS_JSON"], `]}`, `,{"id":"k1","key_base64":"`+key+`"}]}`, 1)
	var projectDoc map[string]any
	if err := json.Unmarshal([]byte(v["COMMERCE_MEDIA_PROJECTS_JSON"]), &projectDoc); err != nil {
		t.Fatal(err)
	}
	item := projectDoc["projects"].([]any)[0]
	projectDoc["projects"] = []any{item, item}
	duplicateProjectBytes, _ := json.Marshal(projectDoc)
	duplicateJSONKey := strings.TrimSuffix(v["COMMERCE_MEDIA_MATERIAL_KEYS_JSON"], "}") + `,"keys":[]}`
	good, err := workerLoad(v)
	if err != nil || !good.Enabled || good.Concurrency != 32 || len(good.Projects) != 1 || good.Projects[0].Config.Environment != "MOCK" {
		t.Fatalf("valid frozen config: %v", err)
	}
	for _, value := range []any{good, good.Projects[0], good.Keys, good.Projects[0].Config} {
		for _, rendered := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), func() string { b, _ := json.Marshal(value); return string(b) }()} {
			if strings.Contains(rendered, "secret") || strings.Contains(rendered, "test_key") || strings.Contains(rendered, "p1") {
				t.Fatalf("secret-bearing value rendered raw: %s", rendered)
			}
		}
	}
	bad := []struct{ name, key, value string }{
		{"zero concurrency", "COMMERCE_MEDIA_WORKER_CONCURRENCY", "0"},
		{"noncanonical concurrency", "COMMERCE_MEDIA_WORKER_CONCURRENCY", "01"},
		{"over concurrency", "COMMERCE_MEDIA_WORKER_CONCURRENCY", "33"},
		{"oversize dsn", "COMMERCE_MEDIA_WORKER_DATABASE_URL", strings.Repeat("x", 8193)},
		{"duplicate key", "COMMERCE_MEDIA_MATERIAL_KEYS_JSON", duplicateKey},
		{"duplicate JSON key", "COMMERCE_MEDIA_MATERIAL_KEYS_JSON", duplicateJSONKey},
		{"unknown key field", "COMMERCE_MEDIA_MATERIAL_KEYS_JSON", strings.TrimSuffix(v["COMMERCE_MEDIA_MATERIAL_KEYS_JSON"], "}") + `,"other":1}`},
		{"trailing bytes", "COMMERCE_MEDIA_PROJECTS_JSON", v["COMMERCE_MEDIA_PROJECTS_JSON"] + `{}`},
		{"fractional version", "COMMERCE_MEDIA_PROJECTS_JSON", strings.Replace(v["COMMERCE_MEDIA_PROJECTS_JSON"], `"credential_version":1`, `"credential_version":1.0`, 1)},
		{"exponent version", "COMMERCE_MEDIA_PROJECTS_JSON", strings.Replace(v["COMMERCE_MEDIA_PROJECTS_JSON"], `"credential_version":1`, `"credential_version":1e0`, 1)},
		{"duplicate project", "COMMERCE_MEDIA_PROJECTS_JSON", string(duplicateProjectBytes)},
		{"remote dial", "COMMERCE_MEDIA_PROJECTS_JSON", strings.Replace(v["COMMERCE_MEDIA_PROJECTS_JSON"], server.Listener.Addr().String(), "192.0.2.1:443", 1)},
		{"DNS dial", "COMMERCE_MEDIA_PROJECTS_JSON", strings.Replace(v["COMMERCE_MEDIA_PROJECTS_JSON"], server.Listener.Addr().String(), "localhost:443", 1)},
		{"wildcard dial", "COMMERCE_MEDIA_PROJECTS_JSON", strings.Replace(v["COMMERCE_MEDIA_PROJECTS_JSON"], server.Listener.Addr().String(), "0.0.0.0:443", 1)},
		{"bad CA", "COMMERCE_MEDIA_PROJECTS_JSON", strings.Replace(v["COMMERCE_MEDIA_PROJECTS_JSON"], strconv.Quote(ca), `"not-a-cert"`, 1)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			copy := map[string]string{}
			for k, val := range v {
				copy[k] = val
			}
			copy[tc.key] = tc.value
			if _, err := workerLoad(copy); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("accepted malformed %s: %v", tc.name, err)
			}
		})
	}
	rotated := map[string]string{}
	for k, val := range v {
		rotated[k] = val
	}
	rotated["COMMERCE_MEDIA_MATERIAL_KEYS_JSON"] = strings.Replace(v["COMMERCE_MEDIA_MATERIAL_KEYS_JSON"], `]}`, `,{"id":"k2","key_base64":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))+`"}]}`, 1)
	rotated["COMMERCE_MEDIA_MATERIAL_ACTIVE_KEY_ID"] = "k2"
	if env, err := workerLoad(rotated); err != nil || env.Keys == nil {
		t.Fatalf("rotation preserving prior key: %v", err)
	}
}

func pemCert(der []byte) []byte {
	return []byte("-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END CERTIFICATE-----\n")
}

type workerRoundTripCount struct {
	base  http.RoundTripper
	count atomic.Int32
}

func (c *workerRoundTripCount) RoundTrip(r *http.Request) (*http.Response, error) {
	c.count.Add(1)
	return c.base.RoundTrip(r)
}

func workerTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unit.livekit.cloud"}, DNSNames: []string{"unit.livekit.cloud"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pemCert(der), append([]byte("-----BEGIN EC PRIVATE KEY-----\n"), append([]byte(base64.StdEncoding.EncodeToString(private)), []byte("\n-----END EC PRIVATE KEY-----\n")...)...))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, string(pemCert(der))
}

func TestWorkerEnvironmentLMW02PinnedTLSAndNoProxy(t *testing.T) {
	var hits, proxyHits atomic.Int32
	server, ca := workerTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Host != "unit.livekit.cloud" {
			t.Error("endpoint host was rewritten")
		}
		w.Header().Set("Location", "https://escape.livekit.cloud/")
		w.WriteHeader(http.StatusFound)
	}))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxyHits.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	defaultTransport := http.DefaultTransport
	v := workerTestVars(ca, server.Listener.Addr().String())
	env, err := workerLoad(v)
	if err != nil {
		t.Fatal(err)
	}
	tr := env.Projects[0].Transport.(*http.Transport)
	if tr.Proxy != nil || !tr.DisableKeepAlives || tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("transport bypassed frozen security profile")
	}
	counted := &workerRoundTripCount{base: env.Projects[0].Transport}
	client, err := New(env.Projects[0].Config, counted)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Query(t.Context(), Target{RoomName: "lc_0123456789abcdef0123456789abcdef", EgressID: "EG_test"})
	if err != ErrUnavailable || counted.count.Load() != 1 || hits.Load() != 1 || proxyHits.Load() != 0 || http.DefaultTransport != defaultTransport {
		t.Fatalf("pinned TLS/redirect/proxy result: err=%v roundtrips=%d hits=%d proxy=%d", err, counted.count.Load(), hits.Load(), proxyHits.Load())
	}
	// A well-formed but unrelated root cannot authenticate the same server.
	_, otherCA := workerTLSServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	wrongCA := workerTestVars(otherCA, server.Listener.Addr().String())
	otherEnv, err := workerLoad(wrongCA)
	if err != nil {
		t.Fatal(err)
	}
	otherClient, err := New(otherEnv.Projects[0].Config, otherEnv.Projects[0].Transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = otherClient.Query(t.Context(), Target{RoomName: "lc_0123456789abcdef0123456789abcdef", EgressID: "EG_test"})
	if err == nil || hits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatalf("wrong CA reached provider: %v hits=%d proxy=%d", err, hits.Load(), proxyHits.Load())
	}
	// Trusted leaf with a different hostname must fail before HTTP handling.
	wrongHost := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer wrongHost.Close()
	wrongHostEnv, err := workerLoad(workerTestVars(string(pemCert(wrongHost.TLS.Certificates[0].Certificate[0])), wrongHost.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	wrongHostClient, err := New(wrongHostEnv.Projects[0].Config, wrongHostEnv.Projects[0].Transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrongHostClient.Query(t.Context(), Target{RoomName: "lc_0123456789abcdef0123456789abcdef", EgressID: "EG_test"})
	if err == nil || hits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatalf("wrong hostname reached provider: %v hits=%d proxy=%d", err, hits.Load(), proxyHits.Load())
	}
}
