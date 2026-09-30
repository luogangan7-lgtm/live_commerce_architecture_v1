//go:build sandbox

package ecpay_test

// TCV10 (contracts/taiwan-cvs-logistics-v1.md §10 TCV10, F2/F5/F10/F18, §15 Q4). Tier SANDBOX, build tag `sandbox`. Records OK mart evidence. The
// directory leg is EXPECTED TO FAIL (GetStoreList has no OK CvsType, checked 2026-09-29): the Q4 default (OK shown disabled "coming soon", no second
// provider in R2) is then the plan. The other legs are evidence probes only. ok_verified may be set only if a later ECPay doc/API adds an OK
// directory and this gate is re-run green.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/shipping/ecpay"
)

func TestEcpayOkmartProbe(t *testing.T) {
	c2c, _ := sandboxAccounts(t)
	client, err := ecpay.NewClient(ecpay.EnvSandbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ev := map[string]any{}
	defer func() { evidence(t, "tcv10-okmart-probe", ev) }()

	t.Run("directory leg (expected FAIL)", func(t *testing.T) {
		if _, cerr := ecpay.CVSType("OKMARTC2C"); cerr == nil {
			t.Fatal("the adapter must report no CvsType for OKMARTC2C (ErrNoDirectory)")
		}
		v := form("MerchantID", c2c.Creds.MerchantID, "CvsType", "OKMART")
		v.Set("CheckMacValue", ecpay.CheckMac(v, c2c.Creds.HashKey, c2c.Creds.HashIV))
		resp, err := http.PostForm("https://logistics-stage.ecpay.com.tw/Helper/GetStoreList", v)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		okDirectory := resp.StatusCode == 200 && strings.Contains(string(body), `"RtnCode":1`) && strings.Contains(string(body), "StoreId")
		ev["directory_okmart"] = map[string]any{"status": resp.StatusCode, "has_rows": okDirectory}
		if !okDirectory {
			t.Errorf("EXPECTED FAIL (F10/F18): ECPay has no GetStoreList directory for OK mart, so ok_verified stays false and OK is shown as coming soon")
		}
	})

	t.Run("map with OKMARTC2C (evidence)", func(t *testing.T) {
		action, fields := ecpay.MapForm(ecpay.EnvSandbox, c2c.Creds.MerchantID, "TCV10OK000000000001", "OKMARTC2C", "https://hooks.example.com/v1/cvs/ecpay/map-return/x", false)
		resp, err := http.PostForm(action, form(flatten(fields)...))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		ev["map_okmartc2c"] = map[string]any{"status": resp.StatusCode, "has_store_field": strings.Contains(string(body), "CVSStoreID"), "key_error": strings.Contains(string(body), "找不到加密金鑰")}
	})

	t.Run("Create with the stage OK store 1328, query, print (evidence)", func(t *testing.T) {
		payload := ecpay.Payload{HashKey: c2c.Creds.HashKey, HashIV: c2c.Creds.HashIV, SenderName: "測試寄件人", SenderCellPhone: "0911222333"}
		tn := ecpay.MerchantTradeNo(randUUID())
		res := client.Create(ctx, c2c.Creds, payload, ecpay.CreateRequest{SubType: "OKMARTC2C", MerchantTradeNo: tn, MerchantTradeDate: time.Now().In(taipei()).Format("2006/01/02 15:04:05"),
			ReceiverStoreID: "1328", ReceiverName: "測試收件人", ReceiverPhone: "0912345678", GoodsName: "商品", ServerReplyURL: "https://hooks.example.com/v1/cvs/ecpay/status/x", GoodsAmount: 100})
		ev["create_okmartc2c"] = map[string]any{"outcome": res.Outcome, "code": res.Code}
		if res.Outcome == "SUCCEEDED" {
			q := client.Query(ctx, c2c.Creds, tn)
			ev["query_okmartc2c"] = map[string]any{"outcome": q.Outcome, "status": q.StatusCode}
		}
		if _, _, perr := ecpay.PrintForm(ecpay.EnvSandbox, c2c.Creds, "OKMARTC2C", "1", "1", "1", false); perr == nil {
			t.Error("there is no OK mart print API (F8): PrintForm must refuse OKMARTC2C")
		}
	})
}
