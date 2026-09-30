//go:build sandbox

package ecpay_test

// Shared helpers of the SANDBOX gates TCV07/TCV10 (build tag sandbox). They talk to the public ECPay stage host only when
// ECPAY_LOGISTICS_SANDBOX=1 and the stage keys are in the environment (source ~/.config/livecommerce/secrets.env); otherwise every test SKIPs with
// "NOT_RUN: ..." (a SKIP is never PASS). Keys are read from the environment and never printed, logged, stored or written to evidence.
// Evidence files go to $ECPAY_EVIDENCE_DIR (the main checkout's output/taiwan-cvs) and contain codes, sizes and statuses only.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"livecommerce/internal/integrations/shipping/ecpay"
)

type stageAccount struct {
	Creds ecpay.Credentials
	Mode  string
}

// sandboxAccounts returns the stage C2C and B2C accounts or skips the test.
func sandboxAccounts(t *testing.T) (c2c, b2c stageAccount) {
	t.Helper()
	if os.Getenv("ECPAY_LOGISTICS_SANDBOX") != "1" {
		t.Skip("NOT_RUN: ECPAY_LOGISTICS_SANDBOX=1 is not set (SANDBOX gate; no ECPay stage traffic without it)")
	}
	get := func(prefix string) (ecpay.Credentials, bool) {
		c := ecpay.Credentials{MerchantID: os.Getenv(prefix + "_MERCHANT_ID"), HashKey: os.Getenv(prefix + "_HASH_KEY"), HashIV: os.Getenv(prefix + "_HASH_IV")}
		return c, c.MerchantID != "" && c.HashKey != "" && c.HashIV != ""
	}
	c, ok1 := get("ECPAY_STAGE_C2C")
	b, ok2 := get("ECPAY_STAGE_B2C")
	if !ok1 || !ok2 {
		t.Skip("NOT_RUN: ECPAY_STAGE_C2C_* / ECPAY_STAGE_B2C_* (MERCHANT_ID, HASH_KEY, HASH_IV) are not in the environment")
	}
	return stageAccount{c, "C2C"}, stageAccount{b, "B2C"}
}

// evidence writes one JSON evidence file (no keys, no recipient data) when ECPAY_EVIDENCE_DIR is set.
func evidence(t *testing.T, name string, v any) {
	t.Helper()
	dir := os.Getenv("ECPAY_EVIDENCE_DIR")
	if dir == "" {
		t.Logf("evidence %s (ECPAY_EVIDENCE_DIR unset, not written): %v", name, v)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(map[string]any{"gate": name, "recorded_at": time.Now().UTC().Format(time.RFC3339), "data": v}, "", " ")
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func flatten(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k, v)
	}
	return out
}

func randUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func taipei() *time.Location {
	if l, err := time.LoadLocation("Asia/Taipei"); err == nil {
		return l
	}
	return time.FixedZone("Asia/Taipei", 8*3600)
}
