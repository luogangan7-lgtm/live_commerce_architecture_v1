package storefront

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
)

type HomeAddress struct {
	Region     string `json:"region"`
	City       string `json:"city"`
	PostalCode string `json:"postal_code"`
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
}

type DestinationInput struct {
	ExpectedVersion int64       `json:"expected_version"`
	CartVersion     int64       `json:"cart_version"`
	Kind            string      `json:"kind"`
	Country         string      `json:"country"`
	RecipientName   string      `json:"recipient_name"`
	Phone           string      `json:"phone"`
	HomeAddress     HomeAddress `json:"home_address"`
	PickupID        string      `json:"pickup_id"`
}

type Destination struct {
	ID            string              `json:"id"`
	Version       int64               `json:"version"`
	CartID        string              `json:"cart_id"`
	CartVersion   int64               `json:"cart_version"`
	Kind          string              `json:"kind"`
	Country       string              `json:"country"`
	RecipientName string              `json:"recipient_name"`
	Phone         string              `json:"phone"`
	HomeAddress   HomeAddress         `json:"home_address"`
	Pickup        *fulfillment.Pickup `json:"pickup,omitempty"`
	SelectedAt    time.Time           `json:"selected_at"`
	ExpiresAt     time.Time           `json:"expires_at"`
}

type destinationReceipt struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// SetDestination appends an owned intent. The transaction opened by buyer.WithScope
// must be rolled back on every error, including an event or receipt insert error.
func SetDestination(ctx context.Context, tx pgx.Tx, s buyer.Scope, key string, in DestinationInput) (Destination, error) {
	var receipt destinationReceipt
	err := buyer.RunCommand(ctx, tx, s, "destination.set", key, in, &receipt, func() error {
		if !validDestinationInput(in) {
			return command.ErrInvalid
		}
		cart, err := readCart(ctx, tx, s, false)
		if err != nil {
			return err
		}
		if cart.ID == "" || cart.Version != in.CartVersion || len(cart.Items) == 0 {
			return command.ErrConflict
		}
		var pickup *fulfillment.Pickup
		if in.Kind != "home" {
			p, err := fulfillment.LockPickup(ctx, tx, s, in.PickupID)
			if err != nil {
				return err
			}
			if p.ID != in.PickupID || p.Kind != in.Kind || p.Country != in.Country {
				return command.ErrConflict
			}
			pickup = &p
		}
		// A missing head has no row to lock. The owner/cart advisory lock
		// serializes creation and delayed callbacks while preserving source->head order.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "destination|"+s.TenantID+"|"+s.StoreID+"|"+s.OwnerID+"|"+cart.ID); err != nil {
			return err
		}
		var current int64
		err = tx.QueryRow(ctx, `SELECT current_version FROM storefront.destination_heads
			WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND cart_id=$4 FOR UPDATE`,
			s.TenantID, s.StoreID, s.OwnerID, cart.ID).Scan(&current)
		absent := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !absent {
			return err
		}
		if current != in.ExpectedVersion || current == math.MaxInt64 {
			return command.ErrConflict
		}
		var selected time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&selected); err != nil {
			return err
		}
		if pickup != nil && (pickup.AttestedAt.After(selected) || !pickup.ValidUntil.After(selected)) {
			return command.ErrConflict
		}
		expires := selected.Add(30 * time.Minute)
		if pickup != nil && pickup.ValidUntil.Before(expires) {
			expires = pickup.ValidUntil
		}
		var pickupID any
		if pickup != nil {
			pickupID = pickup.ID
		}
		receipt.Version = current + 1
		err = tx.QueryRow(ctx, `INSERT INTO storefront.destination_snapshots
			(tenant_id,store_id,owner_id,cart_id,cart_version,creator_session_id,version,kind,country,recipient_name,phone,
			region,city,postal_code,line1,line2,pickup_id,selected_at,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) RETURNING id::text`,
			s.TenantID, s.StoreID, s.OwnerID, cart.ID, cart.Version, s.SessionID, receipt.Version,
			in.Kind, in.Country, in.RecipientName, in.Phone, in.HomeAddress.Region, in.HomeAddress.City,
			in.HomeAddress.PostalCode, in.HomeAddress.Line1, in.HomeAddress.Line2, pickupID, selected, expires).Scan(&receipt.ID)
		if err != nil {
			return err
		}
		if absent {
			_, err = tx.Exec(ctx, `INSERT INTO storefront.destination_heads
				(tenant_id,store_id,owner_id,cart_id,current_version,destination_id) VALUES($1,$2,$3,$4,$5,$6)`,
				s.TenantID, s.StoreID, s.OwnerID, cart.ID, receipt.Version, receipt.ID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE storefront.destination_heads SET current_version=$5,destination_id=$6
				WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND cart_id=$4`,
				s.TenantID, s.StoreID, s.OwnerID, cart.ID, receipt.Version, receipt.ID)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO storefront.destination_events
			(tenant_id,store_id,owner_id,destination_id,session_id,action) VALUES($1,$2,$3,$4,$5,'destination.selected')`,
			s.TenantID, s.StoreID, s.OwnerID, receipt.ID, s.SessionID)
		return err
	})
	if err != nil {
		return Destination{}, err
	}
	if !command.ValidID(receipt.ID) || receipt.Version < 1 {
		return Destination{}, command.ErrConflict
	}
	out, err := readDestination(ctx, tx, s, receipt.ID)
	if err != nil {
		return Destination{}, err
	}
	if out.Version != receipt.Version || out.CartVersion != in.CartVersion || out.Kind != in.Kind || out.Country != in.Country ||
		out.RecipientName != in.RecipientName || out.Phone != in.Phone || out.HomeAddress != in.HomeAddress ||
		destinationPickupID(out.Pickup) != in.PickupID {
		return Destination{}, command.ErrConflict
	}
	return out, nil
}

// GetDestination returns an immutable historical selection, including a revoked
// or expired pickup revision. It is not a checkout eligibility decision.
func GetDestination(ctx context.Context, tx pgx.Tx, s buyer.Scope, id string) (Destination, error) {
	if err := buyer.CheckScope(ctx, tx, s); err != nil {
		return Destination{}, err
	}
	if !command.ValidID(id) {
		return Destination{}, command.ErrInvalid
	}
	return readDestination(ctx, tx, s, id)
}

// CurrentDestination discovers the owned cart's current immutable selection.
// It deliberately includes expired/stale selections: the head version is needed
// for the next CAS write. This read neither renews validity nor proves that a
// particular idempotency key committed. Checkout still revalidates every fact.
func CurrentDestination(ctx context.Context, tx pgx.Tx, s buyer.Scope) (*Destination, error) {
	if err := buyer.CheckScope(ctx, tx, s); err != nil {
		return nil, err
	}
	var id string
	var version int64
	err := tx.QueryRow(ctx, `SELECT h.destination_id::text,h.current_version
		FROM storefront.destination_heads h JOIN storefront.carts c
		ON c.tenant_id=h.tenant_id AND c.store_id=h.store_id AND c.owner_id=h.owner_id AND c.id=h.cart_id
		WHERE h.tenant_id=$1 AND h.store_id=$2 AND h.owner_id=$3`,
		s.TenantID, s.StoreID, s.OwnerID).Scan(&id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := readDestination(ctx, tx, s, id)
	if err != nil {
		return nil, err
	}
	if out.Version != version {
		return nil, command.ErrConflict
	}
	return &out, nil
}

// RevalidateDestination locks current inputs in checkout order. Callers with
// later lock waits must check final database time again before a durable hold.
func RevalidateDestination(ctx context.Context, tx pgx.Tx, s buyer.Scope, id string, cartVersion int64, country, kind string) (Destination, error) {
	if err := buyer.CheckScope(ctx, tx, s); err != nil {
		return Destination{}, err
	}
	if !command.ValidID(id) || cartVersion < 1 || !validCountry(country) || !validDestinationKind(kind) {
		return Destination{}, command.ErrInvalid
	}
	out, err := readDestination(ctx, tx, s, id)
	if err != nil {
		return Destination{}, err
	}
	cart, err := readCart(ctx, tx, s, false)
	if err != nil {
		return Destination{}, err
	}
	if cart.ID == "" || cart.Version != cartVersion || len(cart.Items) == 0 ||
		out.CartID != cart.ID || out.CartVersion != cartVersion || out.Country != country || out.Kind != kind ||
		!validDestinationInput(DestinationInput{ExpectedVersion: out.Version - 1, CartVersion: out.CartVersion,
			Kind: out.Kind, Country: out.Country, RecipientName: out.RecipientName, Phone: out.Phone,
			HomeAddress: out.HomeAddress, PickupID: destinationPickupID(out.Pickup)}) {
		return Destination{}, command.ErrConflict
	}
	if out.Pickup != nil {
		current, err := fulfillment.LockPickup(ctx, tx, s, out.Pickup.ID)
		if err != nil {
			return Destination{}, err
		}
		if current.ID != out.Pickup.ID || current.Kind != out.Pickup.Kind || current.Namespace != out.Pickup.Namespace ||
			current.Code != out.Pickup.Code || current.Version != out.Pickup.Version || current.Country != out.Pickup.Country ||
			current.Name != out.Pickup.Name || current.Address != out.Pickup.Address ||
			current.VerificationKind != out.Pickup.VerificationKind || !current.AttestedAt.Equal(out.Pickup.AttestedAt) ||
			!current.ValidUntil.Equal(out.Pickup.ValidUntil) {
			return Destination{}, command.ErrConflict
		}
	}
	var headID string
	var headVersion int64
	err = tx.QueryRow(ctx, `SELECT destination_id::text,current_version FROM storefront.destination_heads
		WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND cart_id=$4 FOR SHARE`,
		s.TenantID, s.StoreID, s.OwnerID, cart.ID).Scan(&headID, &headVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, command.ErrConflict
	}
	if err != nil {
		return Destination{}, err
	}
	if headID != out.ID || headVersion != out.Version {
		return Destination{}, command.ErrConflict
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Destination{}, err
	}
	if out.SelectedAt.After(now) || !out.ExpiresAt.After(now) || !out.ExpiresAt.After(out.SelectedAt) ||
		out.ExpiresAt.After(out.SelectedAt.Add(30*time.Minute)) {
		return Destination{}, command.ErrConflict
	}
	if out.Pickup != nil && (out.Pickup.AttestedAt.After(out.SelectedAt) ||
		!out.Pickup.ValidUntil.After(now) || out.ExpiresAt.After(out.Pickup.ValidUntil)) {
		return Destination{}, command.ErrConflict
	}
	return out, nil
}

func readDestination(ctx context.Context, tx pgx.Tx, s buyer.Scope, id string) (Destination, error) {
	var out Destination
	var pickupID string
	err := tx.QueryRow(ctx, `SELECT id::text,version,cart_id::text,cart_version,kind,country,recipient_name,phone,
		region,city,postal_code,line1,line2,coalesce(pickup_id::text,''),selected_at,expires_at
		FROM storefront.destination_snapshots WHERE tenant_id=$1 AND store_id=$2 AND owner_id=$3 AND id=$4`,
		s.TenantID, s.StoreID, s.OwnerID, id).Scan(&out.ID, &out.Version, &out.CartID, &out.CartVersion,
		&out.Kind, &out.Country, &out.RecipientName, &out.Phone, &out.HomeAddress.Region, &out.HomeAddress.City,
		&out.HomeAddress.PostalCode, &out.HomeAddress.Line1, &out.HomeAddress.Line2, &pickupID, &out.SelectedAt, &out.ExpiresAt)
	if err != nil {
		return Destination{}, notFound(err)
	}
	if pickupID != "" {
		pickup, err := fulfillment.ReadPickup(ctx, tx, s, pickupID)
		if err != nil {
			return Destination{}, err
		}
		out.Pickup = &pickup
	}
	return out, nil
}

func destinationPickupID(p *fulfillment.Pickup) string {
	if p != nil {
		return p.ID
	}
	return ""
}

func validDestinationInput(in DestinationInput) bool {
	if in.ExpectedVersion < 0 || in.ExpectedVersion == math.MaxInt64 || in.CartVersion < 1 ||
		!validDestinationKind(in.Kind) || !validCountry(in.Country) || !validDestinationText(in.RecipientName, 120, true) ||
		!validDestinationPhone(in.Phone) {
		return false
	}
	if in.Kind == "home" {
		return in.PickupID == "" && validDestinationText(in.HomeAddress.Region, 100, false) &&
			validDestinationText(in.HomeAddress.City, 100, true) && validDestinationText(in.HomeAddress.PostalCode, 20, false) &&
			validDestinationText(in.HomeAddress.Line1, 200, true) && validDestinationText(in.HomeAddress.Line2, 200, false)
	}
	return in.Country == "TW" && command.ValidID(in.PickupID) && in.HomeAddress == (HomeAddress{})
}

func validDestinationKind(kind string) bool {
	return kind == "home" || isCVSKind(kind)
}

func validCountry(country string) bool {
	return len(country) == 2 && country[0] >= 'A' && country[0] <= 'Z' && country[1] >= 'A' && country[1] <= 'Z'
}

func validDestinationText(value string, max int, required bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > max || strings.TrimSpace(value) != value {
		return false
	}
	if required && value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validDestinationPhone(phone string) bool {
	if len(phone) < 6 || len(phone) > 32 || strings.TrimSpace(phone) != phone {
		return false
	}
	digits := 0
	for _, c := range phone {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '+' || c == '(' || c == ')' || c == ' ' || c == '-':
		default:
			return false
		}
	}
	return digits >= 6 && digits <= 20
}

// isCVSKind reports the four Taiwan convenience-store pickup kinds (taiwan-cvs-logistics-v1 TD6); the SQL twin is the widened
// destination_snapshots kind CHECK of migrations/0072.
func isCVSKind(kind string) bool {
	return kind == "cvs_711" || kind == "cvs_familymart" || kind == "cvs_hilife" || kind == "cvs_okmart"
}
