//go:build sandbox

package ecpay_test

// TCV07 (contracts/taiwan-cvs-logistics-v1.md §10 TCV07, F1-F11, F17-F19, §7.2 cache-key assumption). Tier SANDBOX, build tag `sandbox`, env
// ECPAY_LOGISTICS_SANDBOX=1 + stage keys (see sandbox_helpers_test.go). The status notification leg is NOT_RUN in SANDBOX (F7: the stage has no
// simulated status notifications; it is covered by MOCK in TCV06).
// Exercises the real adapter (ecpay.NewClient with the default TLS transport) against https://logistics-stage.ecpay.com.tw.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/shipping/ecpay"
)

func TestEcpaySandbox(t *testing.T) {
	c2c, b2c := sandboxAccounts(t)
	client, err := ecpay.NewClient(ecpay.EnvSandbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ev := map[string]any{}

	t.Run("GetStoreList returns the documented stage stores; both stage merchants agree (7.2 cache-key assumption)", func(t *testing.T) {
		for _, cvs := range []string{"UNIMART", "FAMI"} {
			a, err := client.StoreDirectory(ctx, cvs, c2c.Creds)
			if err != nil {
				t.Fatalf("C2C %s: %v", cvs, err)
			}
			b, err := client.StoreDirectory(ctx, cvs, b2c.Creds)
			if err != nil {
				t.Fatalf("B2C %s: %v", cvs, err)
			}
			ev["directory_"+cvs] = map[string]any{"c2c_rows": len(a), "b2c_rows": len(b)}
			if len(a) != len(b) {
				t.Errorf("%s: the two stage merchants return different lists (%d vs %d): the cache key must become (connection, CvsType)", cvs, len(a), len(b))
			}
			for id := range a {
				if _, ok := b[id]; !ok {
					t.Errorf("%s: store %s is listed for C2C only", cvs, id)
					break
				}
			}
		}
		u, _ := client.StoreDirectory(ctx, "UNIMART", c2c.Creds)
		f, _ := client.StoreDirectory(ctx, "FAMI", c2c.Creds)
		if _, ok := u["131386"]; !ok {
			t.Error("the documented 7-ELEVEN stage store 131386 is not in the UNIMART directory")
		}
		if _, ok := f["006598"]; !ok {
			t.Error("the documented FamilyMart stage store 006598 is not in the FAMI directory")
		}
		if err := client.Probe(ctx, c2c.Creds); err != nil {
			t.Errorf("the connect probe (GetStoreList UNIMART) must pass for the stage C2C merchant: %v", err)
		}
		hl, herr := client.StoreDirectory(ctx, "HILIFE", c2c.Creds)
		ev["hilife_directory_rows"], ev["hilife_directory_error"] = len(hl), herr != nil
	})

	t.Run("Express/map accepts UNIMARTC2C, FAMIC2C, HILIFEC2C (stage fixed store, F3)", func(t *testing.T) {
		for _, sub := range []string{"UNIMARTC2C", "FAMIC2C", "HILIFEC2C"} {
			action, fields := ecpay.MapForm(ecpay.EnvSandbox, c2c.Creds.MerchantID, "TCV07"+strings.ToUpper(sub[:3])+"0000000001", sub, "https://hooks.example.com/v1/cvs/ecpay/map-return/x", false)
			resp, err := http.PostForm(action, form(flatten(fields)...))
			if err != nil {
				t.Fatalf("%s: %v", sub, err)
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			ev["map_"+sub] = map[string]any{"status": resp.StatusCode, "has_store_field": strings.Contains(string(body), "CVSStoreID"), "key_error": strings.Contains(string(body), "找不到加密金鑰")}
			if resp.StatusCode != 200 || strings.Contains(string(body), "找不到加密金鑰") {
				t.Errorf("%s map: status %d (subtype not enabled for the stage merchant?)", sub, resp.StatusCode)
			}
		}
	})

	t.Run("Create (stage stores 131386 / 006598), Query V5, print form, created-only status (F19)", func(t *testing.T) {
		payload := ecpay.Payload{HashKey: c2c.Creds.HashKey, HashIV: c2c.Creds.HashIV, SenderName: "測試寄件人", SenderCellPhone: "0911222333"}
		for _, tc := range []struct{ sub, store string }{{"UNIMARTC2C", "131386"}, {"FAMIC2C", "006598"}} {
			tn := ecpay.MerchantTradeNo(randUUID())
			res := client.Create(ctx, c2c.Creds, payload, ecpay.CreateRequest{SubType: tc.sub, MerchantTradeNo: tn, MerchantTradeDate: time.Now().In(taipei()).Format("2006/01/02 15:04:05"),
				ReceiverStoreID: tc.store, ReceiverName: "測試收件人", ReceiverPhone: "0912345678", GoodsName: "商品", ServerReplyURL: "https://hooks.example.com/v1/cvs/ecpay/status/x", GoodsAmount: 100})
			ev["create_"+tc.sub] = map[string]any{"outcome": res.Outcome, "code": res.Code, "logistics_id_len": len(res.LogisticsID), "payment_no_len": len(res.PaymentNo), "validation_no_len": len(res.ValidationNo)}
			if res.Outcome != "SUCCEEDED" {
				t.Errorf("%s Create with stage store %s: %+v (MAC-valid 1|... expected)", tc.sub, tc.store, res)
				continue
			}
			q := client.Query(ctx, c2c.Creds, tn)
			ev["query_"+tc.sub] = map[string]any{"outcome": q.Outcome, "status_code_right_after_create": q.StatusCode, "same_logistics_id": q.LogisticsID == res.LogisticsID}
			if q.Outcome != "SUCCEEDED" || q.LogisticsID != res.LogisticsID {
				t.Errorf("%s Query V5 by MerchantTradeNo must return the same AllPayLogisticsID: %+v vs %+v", tc.sub, q, res)
			}
			t.Logf("%s: LogisticsStatus right after create = %q (F19 created-only set candidate; recorded as evidence)", tc.sub, q.StatusCode)
			action, fields, perr := ecpay.PrintForm(ecpay.EnvSandbox, c2c.Creds, tc.sub, res.LogisticsID, res.PaymentNo, res.ValidationNo, false)
			if perr != nil {
				t.Errorf("%s print form: %v", tc.sub, perr)
				continue
			}
			resp, err := http.PostForm(action, form(flatten(fields)...))
			if err != nil {
				t.Errorf("%s print: %v", tc.sub, err)
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			ev["print_"+tc.sub] = map[string]any{"status": resp.StatusCode, "html": strings.Contains(strings.ToLower(string(b)), "<html")}
			if resp.StatusCode != 200 {
				t.Errorf("%s print form: status %d", tc.sub, resp.StatusCode)
			}
		}
	})

	t.Run("Hi-Life create with a directory StoreId (evidence for hilife_verified, F17)", func(t *testing.T) {
		hl, err := client.StoreDirectory(ctx, "HILIFE", c2c.Creds)
		if err != nil || len(hl) == 0 {
			t.Skip("NOT_RUN: the stage HILIFE directory is empty or unavailable, so no Hi-Life store id can be used")
		}
		var id string
		for k := range hl {
			id = k
			break
		}
		payload := ecpay.Payload{HashKey: c2c.Creds.HashKey, HashIV: c2c.Creds.HashIV, SenderName: "測試寄件人", SenderCellPhone: "0911222333"}
		res := client.Create(ctx, c2c.Creds, payload, ecpay.CreateRequest{SubType: "HILIFEC2C", MerchantTradeNo: ecpay.MerchantTradeNo(randUUID()), MerchantTradeDate: time.Now().In(taipei()).Format("2006/01/02 15:04:05"),
			ReceiverStoreID: id, ReceiverName: "測試收件人", ReceiverPhone: "0912345678", GoodsName: "商品", ServerReplyURL: "https://hooks.example.com/v1/cvs/ecpay/status/x", GoodsAmount: 100})
		ev["hilife_create"] = map[string]any{"store_id_len": len(id), "outcome": res.Outcome, "code": res.Code}
		if res.Outcome != "SUCCEEDED" {
			t.Errorf("Hi-Life create with directory StoreId (len %d): %+v", len(id), res)
		}
	})

	t.Run("status notification", func(t *testing.T) {
		t.Skip("NOT_RUN: the stage has no simulated status notifications (F7); covered by MOCK in TCV06, and by the owner-approved LIVE parcel TCV13")
	})
	evidence(t, "tcv07-sandbox", ev)
}
