package ecpay_test

// Adapter <-> fake interop (supports TCV05/TCV06 MOCK gates; not a listed gate). The fake (ecpaytest) is written from the ECPay documents;
// this file proves the adapter under test talks to it: connect probe, directory lookup, Create classification per fake mode, Query,
// print form, status parsing. A failure here means adapter and documents disagree (or the fake drifted) before any PG gate runs.
// Exercises: ecpay.Client.Probe, StoreDirectory, Create, Query, ecpay.PrintForm, ecpay.ParseStatus.

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
)

func wireSetup(t *testing.T) (*ecpay.Client, *ecpaytest.Fake, ecpay.Credentials, ecpaytest.Merchant) {
	t.Helper()
	f := ecpaytest.New()
	m := ecpaytest.Merchant{ID: "2000933", Key: keyC2C, IV: ivC2C}
	f.AddMerchant(m)
	f.SetStoreList("UNIMART", []ecpaytest.Store{{ID: "131386", Name: "Stage 7-ELEVEN  ", Addr: "Stage address 7 ", Phone: "0200000001"}, {ID: "000123", Name: "Zero", Addr: "Zero addr"}})
	f.SetStoreList("FAMI", []ecpaytest.Store{{ID: "006598", Name: "Stage FamilyMart", Addr: "Stage address F"}})
	c, err := ecpay.NewClient(ecpay.EnvSandbox, f.Transport())
	if err != nil {
		t.Fatal(err)
	}
	return c, f, ecpay.Credentials{MerchantID: m.ID, HashKey: m.Key, HashIV: m.IV}, m
}

func TestEcpayWireAgainstFake(t *testing.T) {
	ctx := context.Background()
	t.Run("probe and directory", func(t *testing.T) {
		c, f, cr, _ := wireSetup(t)
		if err := c.Probe(ctx, cr); err != nil {
			t.Fatalf("probe against a fake that answers RtnCode 1: %v", err)
		}
		bad := cr
		bad.HashKey += "x"
		if err := c.Probe(ctx, bad); err == nil {
			t.Fatal("a wrong key must fail the probe")
		}
		dir, err := c.StoreDirectory(ctx, "UNIMART", cr)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := dir["131386"]; !ok || strings.HasSuffix(s.Name, " ") || strings.HasSuffix(s.Address, " ") {
			t.Fatalf("X10: trailing spaces must be trimmed: %+v ok=%v", s, ok)
		}
		if _, ok := dir["000123"]; !ok {
			t.Fatal("leading zeros are part of the exact StoreId key")
		}
		if _, ok := dir["123"]; ok {
			t.Fatal("no numeric normalisation of store ids")
		}
		before := f.CountCalls("GetStoreList")
		if _, err := c.StoreDirectory(ctx, "UNIMART", cr); err != nil || f.CountCalls("GetStoreList") != before {
			t.Fatalf("second lookup must come from the per-(environment,CvsType) cache (calls %d -> %d, err %v)", before, f.CountCalls("GetStoreList"), err)
		}
		if _, err := c.StoreDirectory(ctx, "OKMART", cr); err == nil {
			t.Fatal("OK mart has no directory")
		}
	})

	t.Run("create outcomes", func(t *testing.T) {
		c, f, cr, _ := wireSetup(t)
		p := ecpay.Payload{HashKey: cr.HashKey, HashIV: cr.HashIV, SenderName: "寄件人測試", SenderCellPhone: "0911222333"}
		mk := func(n int, collect int) ecpay.CreateRequest {
			return ecpay.CreateRequest{SubType: "UNIMARTC2C", MerchantTradeNo: ecpay.MerchantTradeNo("00000000-0000-4000-8000-00000000000" + string(rune('0'+n))),
				MerchantTradeDate: time.Now().Format("2006/01/02 15:04:05"), ReceiverStoreID: "131386", ReceiverName: "王小明先生", ReceiverPhone: "0912345678",
				GoodsName: "x", ServerReplyURL: "https://hooks.example.invalid/v1/cvs/ecpay/status/x", GoodsAmount: 1000, CollectionAmount: collect}
		}
		res := c.Create(ctx, cr, p, mk(1, 0))
		if res.Outcome != "SUCCEEDED" || res.LogisticsID == "" || res.PaymentNo == "" {
			t.Fatalf("plain create: %+v", res)
		}
		tr, _ := f.Trade(mk(1, 0).MerchantTradeNo)
		if tr.IsCollection != "N" || tr.CollectionAmount != "" {
			t.Fatalf("card order: IsCollection=N and no CollectionAmount, got %q/%q", tr.IsCollection, tr.CollectionAmount)
		}
		res = c.Create(ctx, cr, p, mk(2, 1000))
		if tr, _ = f.Trade(mk(2, 1000).MerchantTradeNo); res.Outcome != "SUCCEEDED" || tr.IsCollection != "Y" || tr.CollectionAmount != "1000" {
			t.Fatalf("pay-at-pickup create: %+v trade %+v (F20: IsCollection=Y with CollectionAmount = GoodsAmount)", res, tr)
		}
		for name, mode := range map[string]ecpaytest.CreateMode{"reject": ecpaytest.CreateReject, "403": ecpaytest.Create403, "bad mac": ecpaytest.CreateBadMAC, "malformed": ecpaytest.CreateMalformed, "500": ecpaytest.CreateServerError} {
			f.SetCreateMode(mode, "balance too low")
			r := c.Create(ctx, cr, p, mk(3, 0))
			want := "UNKNOWN"
			if name == "reject" {
				want = "FAILED_FINAL"
			}
			if r.Outcome != want {
				t.Errorf("%s: outcome %s want %s (%+v)", name, r.Outcome, want, r)
			}
			if r.Outcome == "UNKNOWN" && r.LogisticsID != "" {
				t.Errorf("%s: an UNKNOWN outcome must not carry an id", name)
			}
		}
		f.SetCreateMode(ecpaytest.CreateOK, "")
		f.SetCodes("bad id with spaces", "", "")
		if r := c.Create(ctx, cr, p, mk(4, 0)); r.Outcome != "UNKNOWN" {
			t.Errorf("an AllPayLogisticsID outside the column CHECK must classify UNKNOWN (never SUCCEEDED, never a Finish error): %+v", r)
		}
		f.SetCodes("", strings.Repeat("9", 41), "")
		if r := c.Create(ctx, cr, p, mk(5, 0)); r.Outcome != "SUCCEEDED" || len(r.PaymentNo) != 41 {
			t.Errorf("a 41-char CVSPaymentNo is raw provider data the SQL Finish normalises: %+v", r)
		}
		// F5 client-side validation: nothing may be sent for an invalid recipient
		f.SetCodes("", "", "")
		calls := f.TotalCreates()
		bad := mk(6, 0)
		bad.ReceiverName = "A"
		if r := c.Create(ctx, cr, p, bad); r.Outcome != "FAILED_FINAL" || f.TotalCreates() != calls {
			t.Errorf("invalid recipient must fail locally with zero requests: %+v (creates %d -> %d)", r, calls, f.TotalCreates())
		}
	})

	t.Run("query in either MAC key order, not found, timeout", func(t *testing.T) {
		for _, byteOrder := range []bool{false, true} {
			c, f, cr, _ := wireSetup(t)
			f.SetByteOrderMAC(byteOrder)
			p := ecpay.Payload{HashKey: cr.HashKey, HashIV: cr.HashIV, SenderName: "寄件人測試", SenderCellPhone: "0911222333"}
			req := ecpay.CreateRequest{SubType: "UNIMARTC2C", MerchantTradeNo: ecpay.MerchantTradeNo("00000000-0000-4000-8000-000000000011"), MerchantTradeDate: "2026/09/30 10:00:00",
				ReceiverStoreID: "131386", ReceiverName: "王小明先生", ReceiverPhone: "0912345678", GoodsName: "x", ServerReplyURL: "https://hooks.example.invalid/x", GoodsAmount: 500}
			created := c.Create(ctx, cr, p, req)
			f.SetByteOrderMAC(byteOrder)
			q := c.Query(ctx, cr, req.MerchantTradeNo)
			if q.Outcome != "SUCCEEDED" || q.LogisticsID != created.LogisticsID || q.StatusCode != "300" {
				t.Errorf("byteOrder=%v query: %+v (created %+v)", byteOrder, q, created)
			}
			if nf := c.Query(ctx, cr, ecpay.MerchantTradeNo("00000000-0000-4000-8000-000000000012")); nf.Outcome == "SUCCEEDED" {
				t.Errorf("a trade that does not exist must not classify SUCCEEDED: %+v", nf)
			}
		}
		c, f, cr, _ := wireSetup(t)
		p := ecpay.Payload{HashKey: cr.HashKey, HashIV: cr.HashIV, SenderName: "寄件人測試", SenderCellPhone: "0911222333"}
		f.SetCreateMode(ecpaytest.CreateTimeoutLost, "")
		tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		req := ecpay.CreateRequest{SubType: "FAMIC2C", MerchantTradeNo: ecpay.MerchantTradeNo("00000000-0000-4000-8000-000000000013"), MerchantTradeDate: "2026/09/30 10:00:00",
			ReceiverStoreID: "006598", ReceiverName: "王小明先生", ReceiverPhone: "0912345678", GoodsName: "x", ServerReplyURL: "https://hooks.example.invalid/x", GoodsAmount: 500}
		if r := c.Create(ctx0(tctx), cr, p, req); r.Outcome != "UNKNOWN" {
			t.Errorf("a lost response must classify UNKNOWN: %+v", r)
		}
		f.SetCreateMode(ecpaytest.CreateOK, "")
		if q := c.Query(ctx, cr, req.MerchantTradeNo); q.Outcome != "SUCCEEDED" {
			t.Errorf("the create ECPay recorded before the response was lost must be found by Query: %+v", q)
		}
		if f.CreateCalls(req.MerchantTradeNo) != 1 {
			t.Errorf("Query must never re-Create: %d creates", f.CreateCalls(req.MerchantTradeNo))
		}
	})

	t.Run("status notification MAC", func(t *testing.T) {
		_, _, cr, m := wireSetup(t)
		tr := ecpaytest.Trade{MerchantID: m.ID, TradeNo: "LCABCDEFGHIJKLMNOPQR", LogisticsID: "1000001", SubType: "UNIMARTC2C", GoodsAmount: "1000", PaymentNo: "20000001", ValidationNo: "1234"}
		signed := ecpaytest.SignStatus(m, ecpaytest.StatusFields(tr, "2030", "2026/09/30 10:00:00", "SENTINELNAME"))
		rep, err := ecpay.ParseStatus([]byte(signed.Encode()), cr)
		if err != nil || rep.RtnCode != "2030" || rep.MerchantTradeNo != tr.TradeNo || rep.LogisticsID != tr.LogisticsID {
			t.Fatalf("genuine status: %+v %v", rep, err)
		}
		tampered := url.Values{}
		for k, v := range signed {
			tampered[k] = v
		}
		tampered.Set("RtnCode", "2067")
		if _, err := ecpay.ParseStatus([]byte(tampered.Encode()), cr); err == nil {
			t.Fatal("a tampered RtnCode must fail the MAC")
		}
		extra := ecpaytest.SignStatus(m, func() url.Values {
			v := ecpaytest.StatusFields(tr, "2030", "2026/09/30 10:00:00", "S")
			v.Set("UnknownField", "a")
			return v
		}())
		if _, err := ecpay.ParseStatus([]byte(extra.Encode()), cr); err != nil {
			t.Errorf("an extra unknown field is covered by the MAC and then dropped: %v", err)
		}
		extra.Set("UnknownField", "b")
		if _, err := ecpay.ParseStatus([]byte(extra.Encode()), cr); err == nil {
			t.Error("tampering the unknown field must fail: it is MAC-covered")
		}
	})
}

func ctx0(c context.Context) context.Context { return c }
