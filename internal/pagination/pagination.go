// Package pagination owns bounded, opaque keyset positions for scoped lists: Encode and Decode bind
// a cursor to a tenant, store, collection, parent and filter.
//
// It never authenticates (a cursor is untrusted input, re-checked with the caller's scope on every
// read), never offers offset paging, and never returns an unbounded page.
package pagination

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"livecommerce/internal/command"
)

var deliveryCode = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

const merchantOrderTime = "2006-01-02T15:04:05.000000Z"

const (
	defaultLimit = 50
	maxLimit     = 100
	maxCursor    = 1024
)

type Request struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}

type Binding struct {
	TenantID   string `json:"tenant_id"`
	StoreID    string `json:"store_id"`
	Collection string `json:"collection"`
	ParentID   string `json:"parent_id"`
	Filter     string `json:"filter,omitempty"`
}

type cursor struct {
	Version int      `json:"version"`
	Binding Binding  `json:"binding"`
	Keys    []string `json:"keys"`
}

func Decode(request Request, binding Binding, keyCount int) (int, []string, error) {
	if !validBinding(binding) || !validKeyCount(binding.Collection, keyCount) || request.Limit < 0 || request.Limit > maxLimit {
		return 0, nil, invalid("request")
	}
	limit := request.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if request.Cursor == "" {
		return limit, nil, nil
	}
	if len(request.Cursor) > maxCursor {
		return 0, nil, invalid("cursor")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(request.Cursor)
	if err != nil || len(raw) == 0 {
		return 0, nil, invalid("cursor")
	}
	if timeKeyed(binding.Collection) && base64.RawURLEncoding.EncodeToString(raw) != request.Cursor {
		return 0, nil, invalid("cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value cursor
	if err := decoder.Decode(&value); err != nil {
		return 0, nil, invalid("cursor")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return 0, nil, invalid("cursor")
	}
	if timeKeyed(binding.Collection) {
		canonical, err := json.Marshal(value)
		if err != nil || !bytes.Equal(raw, canonical) {
			return 0, nil, invalid("cursor")
		}
	}
	if value.Version != 1 || value.Binding != binding || len(value.Keys) != keyCount {
		return 0, nil, invalid("cursor")
	}
	for i, key := range value.Keys {
		if !validPositionKey(binding.Collection, i, key) {
			return 0, nil, invalid("cursor")
		}
	}
	return limit, value.Keys, nil
}

func Encode(binding Binding, keys []string) (string, error) {
	if !validBinding(binding) || !validKeyCount(binding.Collection, len(keys)) {
		return "", invalid("cursor")
	}
	for i, key := range keys {
		if !validPositionKey(binding.Collection, i, key) {
			return "", invalid("cursor")
		}
	}
	raw, err := json.Marshal(cursor{Version: 1, Binding: binding, Keys: keys})
	if err != nil || len(raw) > maxCursor {
		return "", invalid("cursor")
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > maxCursor {
		return "", invalid("cursor")
	}
	return encoded, nil
}

func validBinding(b Binding) bool {
	if !command.ValidID(b.TenantID) || !command.ValidID(b.StoreID) {
		return false
	}
	switch b.Collection {
	case "products", "warehouses", "inventory", "provider-accounts", "markets":
		return b.ParentID == "" && b.Filter == ""
	case "merchant-orders":
		return b.ParentID == "" && (b.Filter == "all" || b.Filter == "DRAFT" || b.Filter == "AWAITING_PAYMENT" || b.Filter == "CONFIRMED" || b.Filter == "CANCELLED" || b.Filter == "shipped" || b.Filter == "unshipped")
	case "live-sessions":
		return b.ParentID == "" && b.Filter == ""
	case "customers": // internal/customers.List: Filter is empty or the sha256 hex of the search text
		return b.ParentID == "" && (b.Filter == "" || len(b.Filter) == 64)
	case "delivery-services":
		return command.ValidID(b.ParentID) && len(b.Filter) == 2 &&
			b.Filter[0] >= 'A' && b.Filter[0] <= 'Z' && b.Filter[1] >= 'A' && b.Filter[1] <= 'Z'
	case "skus", "claim-bundles": // claim-bundles: ParentID is the live session (internal/claims.ListBundles)
		return command.ValidID(b.ParentID) && b.Filter == ""
	case "catalog-ledger":
		return command.ValidID(b.ParentID) && len(b.Filter) == 64
	default:
		return false
	}
}

func validPositionKey(collection string, position int, key string) bool {
	if timeKeyed(collection) {
		if position == 0 {
			parsed, err := time.Parse(merchantOrderTime, key)
			return err == nil && parsed.Format(merchantOrderTime) == key
		}
		return command.ValidID(key)
	}
	if collection == "delivery-services" {
		return deliveryCode.MatchString(key)
	}
	return command.ValidID(key)
}

func validKeyCount(collection string, count int) bool {
	if timeKeyed(collection) {
		return count == 2
	}
	if collection == "delivery-services" || collection == "markets" {
		return count == 1
	}
	return count == 1 || count == 2
}

// timeKeyed collections page by (created_at, id): exactly two keys, a microsecond UTC
// timestamp then a UUID, and only byte-canonical cursors are accepted.
func timeKeyed(collection string) bool {
	return collection == "merchant-orders" || collection == "live-sessions" || collection == "claim-bundles" || collection == "customers"
}

func invalid(what string) error { return fmt.Errorf("%w: invalid %s", command.ErrInvalid, what) }
