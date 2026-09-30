// export.go owns the unshipped-orders CSV of contracts/manual-fulfilment-v1.md §5.3: row decoding,
// the phone/formula-injection rules and the RFC 4180 encoder. The file is built in memory per request,
// never written to disk, cache, log or object storage (MD9).
//
// Non-goals: no carrier-specific import formats (ruling M-6), no paging beyond the 1000-row cap
// (ruling M-5: the result flags truncation instead), no bulk tracking import (ruling M-7, R2).

package merchantorders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

const exportRowCap = 1000

// ExportRow is one CSV row; every text field is the frozen order value, formatted only by the writer.
type ExportRow struct {
	OrderID, CreatedAtUTC, ServiceCode, DestinationKind, RecipientName, Phone,
	Country, Region, City, PostalCode, Line1, Line2, PickupNamespace, PickupCode, PickupName,
	PickupAddress, Items string
	TotalMinor int64
	Currency   string
	// PickupSource is the LAST CSV column (ruling B19): ecpay_directory | buyer_entered | merchant_attested, empty for home.
	PickupSource string
}

// Export is the finished file. Truncated is true when more than 1000 orders were eligible.
type Export struct {
	Body      []byte
	Rows      int
	Truncated bool
}

var exportHeader = []string{"order_id", "created_at_utc", "service_code", "destination_kind", "recipient_name", "phone",
	"country", "region", "city", "postal_code", "line1", "line2", "pickup_namespace", "pickup_code", "pickup_name",
	"pickup_address", "items", "total_minor", "currency", "pickup_source"}

// WriteUnshippedCSV writes UTF-8 with BOM, CRLF line ends and RFC 4180 quoting. Not encoding/csv:
// with UseCRLF it drops a bare CR inside a field, which would silently rewrite recipient text.
func WriteUnshippedCSV(w io.Writer, rows []ExportRow) error {
	var b bytes.Buffer
	b.WriteString("\xEF\xBB\xBF")
	writeCSVLine(&b, exportHeader)
	for _, r := range rows {
		writeCSVLine(&b, []string{r.OrderID, r.CreatedAtUTC, r.ServiceCode, r.DestinationKind, r.RecipientName,
			exportPhone(r.Phone), r.Country, r.Region, r.City, r.PostalCode, r.Line1, r.Line2, r.PickupNamespace,
			r.PickupCode, r.PickupName, r.PickupAddress, r.Items, strconv.FormatInt(r.TotalMinor, 10), r.Currency, r.PickupSource})
	}
	_, err := w.Write(b.Bytes())
	return err
}

func writeCSVLine(b *bytes.Buffer, cells []string) {
	for i, cell := range cells {
		if i > 0 {
			b.WriteByte(',')
		}
		cell = guardFormula(cell)
		if strings.ContainsAny(cell, ",\"\r\n") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(cell, `"`, `""`))
			b.WriteByte('"')
		} else {
			b.WriteString(cell)
		}
	}
	b.WriteString("\r\n")
}

// guardFormula prefixes a cell a spreadsheet could execute (first non-space character = + - @, or a first
// character of TAB, CR or LF) with an apostrophe (contract §5.3, A1). Numeric and phone cells never start
// with these characters after formatting, so they pass unchanged.
func guardFormula(cell string) string {
	if cell == "" {
		return cell
	}
	if t := strings.TrimLeft(cell, " \t\r\n"); cell[0] == '\t' || cell[0] == '\r' || cell[0] == '\n' ||
		(t != "" && strings.IndexByte("=+-@", t[0]) >= 0) {
		return "'" + cell
	}
	return cell
}

// exportPhone: digits only; a leading +886 becomes 0 (Taiwan national form); any other country code keeps
// its digits without "+". Export formatting only: the frozen destination value is untouched.
func exportPhone(v string) string {
	v = strings.TrimSpace(v)
	national := strings.HasPrefix(v, "+886")
	if national {
		v = v[len("+886"):]
	}
	var digits strings.Builder
	for _, c := range v {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	out := digits.String()
	if national && !strings.HasPrefix(out, "0") {
		out = "0" + out
	}
	return out
}

// ExportUnshipped asks SQL for 1001 rows to detect truncation, keeps 1000 and renders the CSV.
func ExportUnshipped(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (Export, error) {
	if tx == nil || !validAuthorityInput(scope, token) {
		return Export{}, command.ErrInvalid
	}
	started := time.Now()
	auth := sha256.Sum256([]byte(token))
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT identity.export_unshipped_orders($1,$2::uuid,$3)`, auth[:], scope.StoreID, exportRowCap+1).Scan(&raw); err != nil {
		return Export{}, mapError(err)
	}
	// Both permissions again in Go: the export is bulk PII and needs no single fence.
	for _, permission := range []string{"orders:export", "orders:read"} {
		if err := platform.RequirePermission(ctx, tx, scope, token, permission); err != nil {
			return Export{}, mapError(err)
		}
	}
	rows, err := decodeExportRows(raw)
	if err != nil {
		return Export{}, err
	}
	out := Export{Truncated: len(rows) > exportRowCap}
	if out.Truncated {
		rows = rows[:exportRowCap]
	}
	var body bytes.Buffer
	if err := WriteUnshippedCSV(&body, rows); err != nil {
		return Export{}, ErrUnavailable
	}
	out.Body, out.Rows = body.Bytes(), len(rows)
	// PII rule: store UUID, row count and duration only.
	slog.Info("orders.export_unshipped", "store_id", scope.StoreID, "rows", out.Rows, "truncated", out.Truncated,
		"duration_ms", time.Since(started).Milliseconds())
	return out, nil
}

type exportItem struct {
	Code     string `json:"code"`
	Quantity int64  `json:"quantity"`
}

// exportJSON is one identity.export_unshipped_orders row (same keys as the CSV columns).
type exportJSON struct {
	OrderID         string       `json:"order_id"`
	CreatedAtUTC    string       `json:"created_at_utc"`
	ServiceCode     string       `json:"service_code"`
	DestinationKind string       `json:"destination_kind"`
	RecipientName   string       `json:"recipient_name"`
	Phone           string       `json:"phone"`
	Country         string       `json:"country"`
	Region          string       `json:"region"`
	City            string       `json:"city"`
	PostalCode      string       `json:"postal_code"`
	Line1           string       `json:"line1"`
	Line2           string       `json:"line2"`
	PickupNamespace string       `json:"pickup_namespace"`
	PickupCode      string       `json:"pickup_code"`
	PickupName      string       `json:"pickup_name"`
	PickupAddress   string       `json:"pickup_address"`
	Items           []exportItem `json:"items"`
	TotalMinor      int64        `json:"total_minor"`
	Currency        string       `json:"currency"`
	PickupSource    string       `json:"pickup_source"`
}

func decodeExportRows(raw []byte) ([]ExportRow, error) {
	var objects []json.RawMessage
	if len(raw) > 8<<20 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &objects) != nil || len(objects) > exportRowCap+1 {
		return nil, ErrUnavailable
	}
	rows := make([]ExportRow, 0, len(objects))
	for _, object := range objects {
		// The JSON keys are exactly the CSV column names.
		if _, err := exact(object, exportHeader...); err != nil {
			return nil, err
		}
		var v exportJSON
		if json.Unmarshal(object, &v) != nil || !command.ValidID(v.OrderID) || !money(v.TotalMinor) || !currency(v.Currency) ||
			len(v.Items) == 0 || len(v.Items) > 50 ||
			(v.PickupSource != "" && v.PickupSource != "ecpay_directory" && v.PickupSource != "buyer_entered" && v.PickupSource != "merchant_attested") {
			return nil, ErrUnavailable
		}
		parts := make([]string, len(v.Items))
		for i, item := range v.Items {
			if !skuCode.MatchString(item.Code) || item.Quantity < 1 {
				return nil, ErrUnavailable
			}
			parts[i] = item.Code + "\u00d7" + strconv.FormatInt(item.Quantity, 10)
		}
		rows = append(rows, ExportRow{OrderID: v.OrderID, CreatedAtUTC: v.CreatedAtUTC, ServiceCode: v.ServiceCode,
			DestinationKind: v.DestinationKind, RecipientName: v.RecipientName, Phone: v.Phone, Country: v.Country,
			Region: v.Region, City: v.City, PostalCode: v.PostalCode, Line1: v.Line1, Line2: v.Line2,
			PickupNamespace: v.PickupNamespace, PickupCode: v.PickupCode, PickupName: v.PickupName,
			PickupAddress: v.PickupAddress, Items: strings.Join(parts, "; "), TotalMinor: v.TotalMinor, Currency: v.Currency,
			PickupSource: v.PickupSource})
	}
	return rows, nil
}
