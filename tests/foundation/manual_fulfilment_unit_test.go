package foundation_test

// MF01 (contracts/manual-fulfilment-v1.md §6): pure validation and CSV encoding, no PG.
// Written from §3.1/§3.2/§5.1/§5.3 and the frozen signatures of fulfilment-core
// (merchantorders.NormalizeShipment, WriteUnshippedCSV); never from the implementation.
// Tier: UNIT. Evidence label: MODEL_ONLY until the cores are merged, then REAL (unit) run.

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"testing"

	"livecommerce/internal/merchantorders"
)

func mfuPtr(s string) *string { return &s }

// mfuShip is a valid SHIPPED input the cases mutate one field at a time.
func mfuShip(mutate func(*merchantorders.ShipmentInput)) merchantorders.ShipmentInput {
	in := merchantorders.ShipmentInput{ExpectedVersion: 0, Status: "SHIPPED", CarrierCode: mfuPtr("seven_eleven_cvs"), TrackingNumber: mfuPtr("0012345678")}
	if mutate != nil {
		mutate(&in)
	}
	return in
}

func mfuVoid(mutate func(*merchantorders.ShipmentInput)) merchantorders.ShipmentInput {
	in := merchantorders.ShipmentInput{ExpectedVersion: 1, Status: "VOIDED", VoidReason: mfuPtr("wrong_tracking")}
	if mutate != nil {
		mutate(&in)
	}
	return in
}

func TestManualFulfilmentMF01Validation(t *testing.T) {
	t.Run("csv", mfuCSVCases)
	t.Run("carrier codes", func(t *testing.T) {
		for _, code := range []string{"seven_eleven_cvs", "familymart_cvs", "hilife_cvs", "okmart_cvs", "sf_express", "chunghwa_post"} {
			out, err := merchantorders.NormalizeShipment(mfuShip(func(in *merchantorders.ShipmentInput) { in.CarrierCode = mfuPtr(code) }))
			if err != nil || out.CarrierCode == nil || *out.CarrierCode != code {
				t.Fatalf("carrier %s refused: %v", code, err)
			}
			// §3.2: built-in URL templates are EMPTY in v1: no tracking_url may appear from a carrier code alone.
			if out.TrackingURL != nil {
				t.Fatalf("carrier %s produced a built-in tracking URL %q", code, *out.TrackingURL)
			}
		}
		out, err := merchantorders.NormalizeShipment(mfuShip(func(in *merchantorders.ShipmentInput) {
			in.CarrierCode, in.CarrierName = mfuPtr("other"), mfuPtr("黑貓宅急便")
		}))
		if err != nil || out.CarrierName == nil || *out.CarrierName != "黑貓宅急便" || out.TrackingURL != nil {
			t.Fatalf("other carrier with a name: %+v %v", out, err)
		}
		bad := map[string]func(*merchantorders.ShipmentInput){
			"unknown code":         func(in *merchantorders.ShipmentInput) { in.CarrierCode = mfuPtr("fedex") },
			"upper-case code":      func(in *merchantorders.ShipmentInput) { in.CarrierCode = mfuPtr("SF_EXPRESS") },
			"empty code":           func(in *merchantorders.ShipmentInput) { in.CarrierCode = mfuPtr("") },
			"missing code":         func(in *merchantorders.ShipmentInput) { in.CarrierCode = nil },
			"other without a name": func(in *merchantorders.ShipmentInput) { in.CarrierCode = mfuPtr("other") },
			"other with blank name": func(in *merchantorders.ShipmentInput) {
				in.CarrierCode, in.CarrierName = mfuPtr("other"), mfuPtr("   ")
			},
			"name over 80 characters": func(in *merchantorders.ShipmentInput) { in.CarrierName = mfuPtr(strings.Repeat("a", 81)) },
			"name with control char":  func(in *merchantorders.ShipmentInput) { in.CarrierName = mfuPtr("Black\x07Cat") },
			"name with newline":       func(in *merchantorders.ShipmentInput) { in.CarrierName = mfuPtr("Black\nCat") },
		}
		for name, mutate := range bad {
			if _, err := merchantorders.NormalizeShipment(mfuShip(mutate)); !errors.Is(err, merchantorders.ErrInvalidCarrier) {
				t.Fatalf("%s: want ErrInvalidCarrier, got %v", name, err)
			}
		}
	})

	t.Run("carrier name unicode", func(t *testing.T) {
		// 80 characters (not bytes) is the limit: 80 CJK characters are 240 bytes and must pass.
		long := strings.Repeat("郵", 80)
		out, err := merchantorders.NormalizeShipment(mfuShip(func(in *merchantorders.ShipmentInput) { in.CarrierName = mfuPtr(long) }))
		if err != nil || out.CarrierName == nil || *out.CarrierName != long {
			t.Fatalf("80 CJK characters refused: %v", err)
		}
		if _, err := merchantorders.NormalizeShipment(mfuShip(func(in *merchantorders.ShipmentInput) { in.CarrierName = mfuPtr(long + "郵") })); !errors.Is(err, merchantorders.ErrInvalidCarrier) {
			t.Fatalf("81 CJK characters accepted: %v", err)
		}
		// NFC (§3.1 "free text, NFC"): e + combining acute must never be stored decomposed. The contract does not say
		// whether non-NFC input is normalized or refused; either is acceptable, storing it as given is not.
		out, err = merchantorders.NormalizeShipment(mfuShip(func(in *merchantorders.ShipmentInput) { in.CarrierName = mfuPtr("Cafe\u0301 Express") }))
		if err == nil && (out.CarrierName == nil || *out.CarrierName != "Caf\u00e9 Express") {
			t.Fatalf("carrier_name stored non-NFC: %v", out.CarrierName)
		}
		if err != nil && !errors.Is(err, merchantorders.ErrInvalidCarrier) {
			t.Fatalf("non-NFC carrier_name refused with %v, want ErrInvalidCarrier", err)
		}
	})

	t.Run("tracking number", func(t *testing.T) {
		keep := map[string]string{
			"0012345678":            "0012345678",  // leading zeroes kept
			"abC-123 xyz":           "abC-123 xyz", // no case folding, inner space and hyphen allowed
			"  SF123456  ":          "SF123456",    // trimmed
			"A":                     "A",
			strings.Repeat("9", 64): strings.Repeat("9", 64),
		}
		for in, want := range keep {
			out, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.TrackingNumber = mfuPtr(in) }))
			if err != nil || out.TrackingNumber == nil || *out.TrackingNumber != want {
				t.Fatalf("tracking %q -> %v err=%v want %q", in, out.TrackingNumber, err, want)
			}
		}
		for name, in := range map[string]*string{
			"missing":            nil,
			"empty":              mfuPtr(""),
			"spaces only":        mfuPtr("   "),
			"65 characters":      mfuPtr(strings.Repeat("9", 65)),
			"leading hyphen":     mfuPtr("-12345"),
			"underscore":         mfuPtr("AB_123"),
			"slash":              mfuPtr("AB/123"),
			"inner tab":          mfuPtr("AB\t123"),
			"inner newline":      mfuPtr("AB\n123"),
			"arabic-indic digit": mfuPtr("١٢٣"),
			"fullwidth digit":    mfuPtr("１２３４"),
			"emoji":              mfuPtr("AB📦"),
		} {
			if _, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.TrackingNumber = in })); !errors.Is(err, merchantorders.ErrInvalidTracking) {
				t.Fatalf("tracking %s: want ErrInvalidTracking, got %v", name, err)
			}
		}
	})

	t.Run("tracking url", func(t *testing.T) {
		for in, want := range map[string]string{
			"https://track.example.com/t?n=0012345678": "https://track.example.com/t?n=0012345678",
			"HTTPS://Track.Example.COM/Path":           "https://track.example.com/Path", // scheme + host lower-cased, path kept
		} {
			out, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.TrackingURL = mfuPtr(in) }))
			if err != nil || out.TrackingURL == nil || *out.TrackingURL != want {
				t.Fatalf("url %q -> %v err=%v want %q", in, out.TrackingURL, err, want)
			}
		}
		long := "https://track.example.com/" + strings.Repeat("a", 512-len("https://track.example.com/"))
		if out, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.TrackingURL = mfuPtr(long) })); err != nil || out.TrackingURL == nil || len(*out.TrackingURL) != 512 {
			t.Fatalf("512-byte URL refused: %v", err)
		}
		for name, in := range map[string]string{
			"http":            "http://track.example.com/t",
			"ftp":             "ftp://track.example.com/t",
			"javascript":      "javascript:alert(1)",
			"userinfo":        "https://user:pw@track.example.com/t",
			"user only":       "https://user@track.example.com/t",
			"explicit port":   "https://track.example.com:8443/t",
			"default port":    "https://track.example.com:443/t",
			"fragment":        "https://track.example.com/t#top",
			"empty fragment":  "https://track.example.com/t#",
			"no dot in host":  "https://localhost/t",
			"relative":        "/track/123",
			"scheme relative": "//track.example.com/t",
			"space":           "https://track.example.com/a b",
			"tab":             "https://track.example.com/a\tb",
			"newline":         "https://track.example.com/a\nb",
			"513 bytes":       "https://track.example.com/" + strings.Repeat("a", 513-len("https://track.example.com/")),
			"non-ASCII":       "https://track.example.com/追蹤",
			"host with <":     "https://a<b.example.com/x", // S6: WHATWG rejects it; the admin/storefront parse would fail
			"host with >":     "https://a>b.example.com/x",
			"host with quote": "https://a\"b.example.com/x",
			"host is IPv4":    "https://1.2.3.4/x",
		} {
			if _, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.TrackingURL = mfuPtr(in) })); !errors.Is(err, merchantorders.ErrInvalidURL) {
				t.Fatalf("url %s (%q): want ErrInvalidURL, got %v", name, in, err)
			}
		}
	})

	t.Run("note", func(t *testing.T) {
		if _, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.Note = mfuPtr(strings.Repeat("註", 200)) })); err != nil {
			t.Fatalf("200-character note refused: %v", err)
		}
		// The contract names no dedicated error for a bad note; it must still be refused.
		for name, note := range map[string]string{"201 characters": strings.Repeat("註", 201), "control": "a\x00b", "newline": "a\nb"} {
			if _, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.Note = mfuPtr(note) })); err == nil {
				t.Fatalf("note %s accepted", name)
			}
		}
	})

	t.Run("status and void body", func(t *testing.T) {
		if _, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.Status = "DELIVERED" })); err == nil {
			t.Fatal("status DELIVERED accepted (I13: never a delivery claim)")
		}
		if _, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.Status = "IN_TRANSIT" })); err == nil {
			t.Fatal("status IN_TRANSIT accepted")
		}
		// A SHIPPED body must carry void_reason = null.
		if _, err := merchantorders.NormalizeShipment(mfuShip(func(s *merchantorders.ShipmentInput) { s.VoidReason = mfuPtr("wrong_order") })); !errors.Is(err, merchantorders.ErrInvalidVoid) {
			t.Fatalf("SHIPPED with void_reason: %v", err)
		}
		for _, reason := range []string{"wrong_order", "wrong_tracking", "not_dispatched", "other"} {
			out, err := merchantorders.NormalizeShipment(mfuVoid(func(s *merchantorders.ShipmentInput) { s.VoidReason = mfuPtr(reason) }))
			if err != nil || out.Status != "VOIDED" || out.VoidReason == nil || *out.VoidReason != reason {
				t.Fatalf("void %s: %+v %v", reason, out, err)
			}
			if out.CarrierCode != nil || out.CarrierName != nil || out.TrackingNumber != nil || out.TrackingURL != nil || out.Note != nil {
				t.Fatalf("void body must stay all-null for the request hash, got %+v", out)
			}
		}
		// Any non-null carrier/tracking/note value in a void body is invalid_void.
		for name, mutate := range map[string]func(*merchantorders.ShipmentInput){
			"carrier_code":    func(s *merchantorders.ShipmentInput) { s.CarrierCode = mfuPtr("sf_express") },
			"carrier_name":    func(s *merchantorders.ShipmentInput) { s.CarrierName = mfuPtr("x") },
			"tracking_number": func(s *merchantorders.ShipmentInput) { s.TrackingNumber = mfuPtr("123") },
			"tracking_url":    func(s *merchantorders.ShipmentInput) { s.TrackingURL = mfuPtr("https://a.example.com/") },
			"note":            func(s *merchantorders.ShipmentInput) { s.Note = mfuPtr("oops") },
			"empty note":      func(s *merchantorders.ShipmentInput) { s.Note = mfuPtr("") },
		} {
			if _, err := merchantorders.NormalizeShipment(mfuVoid(mutate)); !errors.Is(err, merchantorders.ErrInvalidVoid) {
				t.Fatalf("void with %s: want ErrInvalidVoid, got %v", name, err)
			}
		}
		for name, mutate := range map[string]func(*merchantorders.ShipmentInput){
			"missing reason": func(s *merchantorders.ShipmentInput) { s.VoidReason = nil },
			"unknown reason": func(s *merchantorders.ShipmentInput) { s.VoidReason = mfuPtr("changed_mind") },
			"empty reason":   func(s *merchantorders.ShipmentInput) { s.VoidReason = mfuPtr("") },
		} {
			if _, err := merchantorders.NormalizeShipment(mfuVoid(mutate)); err == nil {
				t.Fatalf("void %s accepted", name)
			}
		}
	})
}

// mfuCSV renders rows and returns the raw bytes.
func mfuCSV(t *testing.T, rows []merchantorders.ExportRow) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := merchantorders.WriteUnshippedCSV(&b, rows); err != nil {
		t.Fatalf("WriteUnshippedCSV: %v", err)
	}
	return b.Bytes()
}

// mfuRecords parses the CSV as RFC 4180 after removing the BOM; CRLF is enforced separately.
func mfuRecords(t *testing.T, raw []byte) [][]string {
	t.Helper()
	recs, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))).ReadAll()
	if err != nil {
		t.Fatalf("output is not RFC 4180: %v\n%q", err, raw)
	}
	return recs
}

const mfuHeader = "order_id,created_at_utc,service_code,destination_kind,recipient_name,phone,country,region,city,postal_code,line1,line2,pickup_namespace,pickup_code,pickup_name,pickup_address,items,total_minor,currency,pickup_source"

// mfuCSVCases is the CSV-encoder half of MF01 (subtest "csv" of TestManualFulfilmentMF01Validation).
func mfuCSVCases(t *testing.T) {
	base := merchantorders.ExportRow{OrderID: "11111111-1111-4111-8111-111111111111", CreatedAtUTC: "2026-09-29T01:02:03Z", ServiceCode: "home", DestinationKind: "home",
		RecipientName: "王小明", Phone: "0912345678", Country: "TW", City: "台北市", PostalCode: "100", Line1: "中正路 1 號", Items: "SKU-1×2; SKU-2×1", TotalMinor: 12300, Currency: "TWD"}
	raw := mfuCSV(t, []merchantorders.ExportRow{base})
	if !bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("no UTF-8 BOM")
	}
	if !bytes.HasSuffix(raw, []byte("\r\n")) || bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) || bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\r")) {
		t.Fatalf("line ends must be CRLF only: %q", raw)
	}
	if !strings.HasPrefix(string(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})), mfuHeader+"\r\n") {
		t.Fatalf("header mismatch: %q", raw)
	}
	recs := mfuRecords(t, raw)
	if len(recs) != 2 || len(recs[1]) != 20 {
		t.Fatalf("shape: %d records, %d columns", len(recs), len(recs[len(recs)-1]))
	}
	want := []string{base.OrderID, base.CreatedAtUTC, "home", "home", "王小明", "0912345678", "TW", "", "台北市", "100", "中正路 1 號", "", "", "", "", "", "SKU-1×2; SKU-2×1", "12300", "TWD", ""} // + pickup_source (0073, taiwan-cvs C4): empty for a home row
	for i, w := range want {
		if recs[1][i] != w {
			t.Fatalf("column %d (%s) = %q want %q", i, strings.Split(mfuHeader, ",")[i], recs[1][i], w)
		}
	}

	t.Run("header only for zero rows", func(t *testing.T) {
		recs := mfuRecords(t, mfuCSV(t, nil))
		if len(recs) != 1 || strings.Join(recs[0], ",") != mfuHeader {
			t.Fatalf("empty export: %v", recs)
		}
	})

	t.Run("RFC 4180 quoting round trip", func(t *testing.T) {
		r := base
		r.RecipientName = `Wang, "Xiao" Ming`
		r.Line1 = "line one\r\nline two" // embedded CRLF must be quoted, not a record break
		r.Line2 = `say ""hi"" , twice`
		if !bytes.Contains(mfuCSV(t, []merchantorders.ExportRow{r}), []byte("\"line one\r\nline two\"")) {
			t.Fatal("embedded CRLF cell not quoted with its CRLF intact")
		}
		// a lone CR (no LF) inside a cell must be quoted too, or a spreadsheet reads a phantom record break
		lone := base
		lone.Line1 = "a\rb"
		if !bytes.Contains(mfuCSV(t, []merchantorders.ExportRow{lone}), []byte("\"a\rb\"")) {
			t.Fatal("a cell with a lone CR is not quoted")
		}
		got := mfuRecords(t, mfuCSV(t, []merchantorders.ExportRow{r}))
		// encoding/csv reports CRLF inside a quoted field as LF; the bytes are checked below.
		if len(got) != 2 || got[1][4] != r.RecipientName || got[1][10] != "line one\nline two" || got[1][11] != r.Line2 {
			t.Fatalf("round trip lost data: %q", got[1])
		}
	})

	t.Run("formula injection guard", func(t *testing.T) {
		// §5.3 (A1): first non-space char in = + - @, or first char TAB/CR/LF -> prefix an apostrophe.
		guarded := []string{"=1+1", "@SUM(A1)", "-2", " =cmd", "   @x", "\t=x", "\tabc", "\nabc", "\rabc", "\r\n=x", "  -1"}
		for _, v := range guarded {
			r := base
			r.RecipientName = v
			got := mfuRecords(t, mfuCSV(t, []merchantorders.ExportRow{r}))
			if want := strings.ReplaceAll("'"+v, "\r\n", "\n"); len(got) != 2 || got[1][4] != want {
				t.Fatalf("injection %q exported as %q, want %q", v, got[1][4], want)
			}
		}
		// '+' is guarded everywhere except the phone column (its own rule).
		r := base
		r.Line1 = "+886 hello"
		if got := mfuRecords(t, mfuCSV(t, []merchantorders.ExportRow{r})); got[1][10] != "'+886 hello" {
			t.Fatalf("plus in a text cell not guarded: %q", got[1][10])
		}
		for _, v := range []string{"a=b", "1+1", "x-y", "user@example.invalid", "'already", "SKU=1", " leading space only"} {
			r := base
			r.RecipientName = v
			if got := mfuRecords(t, mfuCSV(t, []merchantorders.ExportRow{r})); got[1][4] != v {
				t.Fatalf("benign %q was altered to %q", v, got[1][4])
			}
		}
		// Every text column is guarded, not only the recipient.
		r = base
		r.Line1, r.Line2, r.City, r.Region, r.PickupName, r.PickupAddress = "=a", "+a", "-a", "@a", "=b", "@b"
		got := mfuRecords(t, mfuCSV(t, []merchantorders.ExportRow{r}))[1]
		for _, i := range []int{7, 8, 10, 11, 14, 15} {
			if !strings.HasPrefix(got[i], "'") {
				t.Fatalf("column %d (%s) unguarded: %q", i, strings.Split(mfuHeader, ",")[i], got[i])
			}
		}
	})

	t.Run("phone", func(t *testing.T) {
		for in, want := range map[string]string{
			"+886912345678":     "0912345678", // Taiwan national form; the '+' guard must NOT add an apostrophe
			"+886 912 345 678":  "0912345678",
			"+886-912-345-678":  "0912345678",
			"0912345678":        "0912345678",
			"+81312345678":      "81312345678", // other country code: digits without '+'
			"+1 (415) 555-0100": "14155550100",
			"(02) 2345-6789":    "0223456789",
		} {
			r := base
			r.Phone = in
			if got := mfuRecords(t, mfuCSV(t, []merchantorders.ExportRow{r})); got[1][5] != want {
				t.Fatalf("phone %q -> %q want %q", in, got[1][5], want)
			}
		}
	})

	t.Run("pickup code keeps leading zeroes in the file bytes", func(t *testing.T) {
		r := base
		r.DestinationKind, r.PickupNamespace, r.PickupCode, r.PickupName, r.PickupAddress = "pickup", "seven_eleven", "007123", "門市", "台北市某路 2 號"
		out := mfuCSV(t, []merchantorders.ExportRow{r})
		if !bytes.Contains(out, []byte("007123")) {
			t.Fatalf("leading zeroes lost in bytes: %q", out)
		}
		if got := mfuRecords(t, out); got[1][12] != "seven_eleven" || got[1][13] != "007123" {
			t.Fatalf("pickup columns: %q", got[1])
		}
	})

	t.Run("1000 rows", func(t *testing.T) {
		rows := make([]merchantorders.ExportRow, 1000)
		for i := range rows {
			rows[i] = base
			rows[i].OrderID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		}
		recs := mfuRecords(t, mfuCSV(t, rows))
		if len(recs) != 1001 || recs[1000][0] != rows[999].OrderID || recs[1][0] != rows[0].OrderID {
			t.Fatalf("1000 rows -> %d records (header + 1000 expected), order preserved=%v", len(recs), recs[1][0] == rows[0].OrderID)
		}
	})
}
