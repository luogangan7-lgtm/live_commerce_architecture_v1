// merchant.go owns the merchant claim configuration and read models: the Studio board
// (window, offers, per-reason stats), SetWindow, CreateOffer, UpdateOffer and ListBundles
// (contract §4.2).
//
// Non-goals: no manual ingest (manual.go), no link issue (link.go), no HTTP decoding, no
// session lifecycle (package live owns live.sessions/programs; claims only reads a
// session's existence and never locks it).

package claims

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/claims/grammar"
	"livecommerce/internal/command"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
)

// cursorTime is the claim-bundles position format (the live-sessions key format).
const cursorTime = "2006-01-02T15:04:05.000000Z"

// Frozen merchant DTOs (contract §4.2): field names and json tags are the HTTP wire shape.
type OfferInput struct {
	Keyword             string `json:"keyword"`
	SKUID               string `json:"sku_id"`
	MaxQuantityPerClaim int64  `json:"max_quantity_per_claim"`
}
type OfferUpdate struct {
	ExpectedVersion     int64 `json:"expected_version"`
	MaxQuantityPerClaim int64 `json:"max_quantity_per_claim"`
	Active              bool  `json:"active"`
}
type Offer struct {
	ID                  string    `json:"offer_id"`
	SessionID           string    `json:"session_id"`
	Keyword             string    `json:"keyword"`
	SKUID               string    `json:"sku_id"`
	SKUCode             string    `json:"sku_code"`
	ProductName         string    `json:"product_name"`
	MaxQuantityPerClaim int64     `json:"max_quantity_per_claim"`
	Active              bool      `json:"active"`
	Version             int64     `json:"version"`
	ActivatedAt         time.Time `json:"activated_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}
type WindowInput struct {
	ExpectedVersion int64     `json:"expected_version"`
	State           string    `json:"state"` // OPEN | CLOSED
	MatchMode       MatchMode `json:"match_mode"`
}
type Window struct {
	SessionID  string     `json:"session_id"`
	State      string     `json:"state"`
	MatchMode  MatchMode  `json:"match_mode"`
	Generation int64      `json:"generation"`
	Version    int64      `json:"version"` // 0 = no row yet (reported CLOSED/EXACT)
	OpenedAt   *time.Time `json:"opened_at"`
	ClosedAt   *time.Time `json:"closed_at"`
}
type Stats struct {
	Generation int64            `json:"generation"`
	Accepted   int64            `json:"accepted"`
	Rejected   map[Reason]int64 `json:"rejected"` // exactly the 7 boardReasons present, 0 when none
}
type Board struct {
	Window Window  `json:"window"`
	Offers []Offer `json:"offers"` // ≤200, ORDER BY keyword
	Stats  Stats   `json:"stats"`
}
type Line struct {
	OfferID  string `json:"offer_id"`
	Keyword  string `json:"keyword"`
	SKUID    string `json:"sku_id"`
	Quantity int64  `json:"quantity"`
	Version  int64  `json:"version"`
	Applied  bool   `json:"applied"` // applied_version = version
}
type LinkState struct {
	State      string     `json:"state"` // NONE | ACTIVE | EXPIRED
	Generation int64      `json:"generation"`
	ExpiresAt  *time.Time `json:"expires_at"`
}
type Bundle struct {
	ID        string    `json:"bundle_id"`
	Ref       string    `json:"ref"` // upper(first 8 hex of ID), display only
	Platform  string    `json:"platform"`
	Label     string    `json:"label"` // manual only; merchant-only personal data
	Bound     bool      `json:"bound"` // bound_at IS NOT NULL; owner never exposed
	Version   int64     `json:"version"`
	Link      LinkState `json:"link"`
	Lines     []Line    `json:"lines"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GetBoard is the Studio claims panel read (live:read): the session's window (CLOSED/EXACT
// version 0 when no row exists), its offers ORDER BY keyword and per-reason counts for the
// current window generation. Read-only, no locks; statements may observe different
// committed snapshots under READ COMMITTED, but Stats always matches the generation it
// reports. Called by the merchant HTTP adapter M1.
func GetBoard(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, sessionID string) (Board, error) {
	if !command.ValidID(sessionID) {
		return Board{}, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return Board{}, err
	}
	if err := requireSession(ctx, tx, scope, sessionID); err != nil {
		return Board{}, err
	}
	window, err := readWindow(ctx, tx, scope, sessionID, "")
	if err != nil {
		return Board{}, err
	}
	offers, err := readOffers(ctx, tx, scope, sessionID, "")
	if err != nil {
		return Board{}, err
	}
	stats, err := readStats(ctx, tx, scope, sessionID, window.Generation)
	if err != nil {
		return Board{}, err
	}
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return Board{}, err
	}
	return Board{Window: window, Offers: offers, Stats: stats}, nil
}

// SetWindow opens, closes or re-modes the session's claim window (live:manage) with a
// version CAS, one receipt (live.claim.window.set) and one audit row. It locks the window
// row FOR NO KEY UPDATE, so a close waits for in-flight ingests holding it FOR SHARE and
// any later ingest sees CLOSED. Transition rules are planWindow's. Called by M2.
func SetWindow(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID string, in WindowInput) (Window, error) {
	if !command.ValidID(sessionID) || in.ExpectedVersion < 0 || in.ExpectedVersion == math.MaxInt64 ||
		(in.State != WindowOpen && in.State != WindowClosed) || !validMode(in.MatchMode) {
		return Window{}, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Window{}, err
	}
	request := struct {
		PrincipalID     string    `json:"principal_id"`
		SessionID       string    `json:"session_id"`
		ExpectedVersion int64     `json:"expected_version"`
		State           string    `json:"state"`
		MatchMode       MatchMode `json:"match_mode"`
	}{scope.PrincipalID, sessionID, in.ExpectedVersion, in.State, in.MatchMode}
	var out Window
	err := command.Run(ctx, tx, scope, "live.claim.window.set", key, request, &out, func() error {
		if err := requireSession(ctx, tx, scope, sessionID); err != nil {
			return err
		}
		current, err := readWindow(ctx, tx, scope, sessionID, " FOR NO KEY UPDATE")
		if err != nil {
			return err
		}
		if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
			return err
		}
		if current.Version != in.ExpectedVersion {
			return command.ErrConflict
		}
		action, err := planWindow(current, in)
		if err != nil {
			return err
		}
		out, err = writeWindow(ctx, tx, scope, sessionID, current.Version, in)
		if err != nil {
			return err
		}
		return command.Audit(ctx, tx, scope, "live.claim.window."+action)
	})
	if err != nil {
		return Window{}, mapError(err)
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Window{}, err
	}
	return out, nil
}

// planWindow is the pure §4.2 transition table (mode changes only while CLOSED, and a
// state transition must carry the current mode; §0.1 P2(e)). It returns the audit action
// suffix: "opened", "closed" or "mode_set" (also used for inserting a CLOSED row).
func planWindow(current Window, in WindowInput) (string, error) {
	if current.Version == 0 {
		if in.State == WindowOpen {
			return "opened", nil
		}
		return "mode_set", nil
	}
	switch {
	case current.State == WindowClosed && in.State == WindowOpen && in.MatchMode == current.MatchMode:
		return "opened", nil
	case current.State == WindowOpen && in.State == WindowClosed && in.MatchMode == current.MatchMode:
		return "closed", nil
	case current.State == WindowClosed && in.State == WindowClosed && in.MatchMode != current.MatchMode:
		return "mode_set", nil
	}
	// OPEN→OPEN, a mode change while OPEN or on a transition, identical CLOSED→CLOSED.
	return "", command.ErrConflict
}

// writeWindow applies a transition planWindow accepted. Version 0 inserts the row (OPEN:
// generation 1; CLOSED: generation 0); otherwise one UPDATE covers all three actions:
// opening bumps generation and resets opened_at/closed_at, closing stamps closed_at, and a
// CLOSED→CLOSED mode change touches only match_mode. A second OPEN window in the store
// fails live_claim_window_one_open (23505 → ErrConflict).
func writeWindow(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID string, version int64, in WindowInput) (Window, error) {
	out := Window{SessionID: sessionID}
	var row pgx.Row
	if version == 0 {
		row = tx.QueryRow(ctx, `INSERT INTO live.claim_windows(tenant_id,store_id,session_id,state,match_mode,generation,opened_at,principal_id)
			VALUES($1,$2,$3,$4::text,$5,CASE WHEN $4::text='OPEN' THEN 1 ELSE 0 END,CASE WHEN $4::text='OPEN' THEN clock_timestamp() END,$6)
			RETURNING state,match_mode,generation,version,opened_at,closed_at`,
			scope.TenantID, scope.StoreID, sessionID, in.State, string(in.MatchMode), scope.PrincipalID)
	} else {
		row = tx.QueryRow(ctx, `UPDATE live.claim_windows SET state=$4::text,match_mode=$5,
			generation=generation+CASE WHEN $4::text='OPEN' THEN 1 ELSE 0 END,
			opened_at=CASE WHEN $4::text='OPEN' THEN clock_timestamp() ELSE opened_at END,
			closed_at=CASE WHEN $4::text='OPEN' THEN NULL WHEN state='OPEN' THEN clock_timestamp() ELSE closed_at END,
			version=version+1,principal_id=$6,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND version=$7
			RETURNING state,match_mode,generation,version,opened_at,closed_at`,
			scope.TenantID, scope.StoreID, sessionID, in.State, string(in.MatchMode), scope.PrincipalID, version)
	}
	if err := row.Scan(&out.State, &out.MatchMode, &out.Generation, &out.Version, &out.OpenedAt, &out.ClosedAt); err != nil {
		return Window{}, mapError(err)
	}
	return out, nil
}

// readWindow returns the window row or the version-0 CLOSED/EXACT placeholder. lock is ""
// or " FOR NO KEY UPDATE" (a constant chosen by the caller, never input).
func readWindow(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID, lock string) (Window, error) {
	out := Window{SessionID: sessionID}
	err := tx.QueryRow(ctx, `SELECT state,match_mode,generation,version,opened_at,closed_at FROM live.claim_windows
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`+lock, scope.TenantID, scope.StoreID, sessionID).
		Scan(&out.State, &out.MatchMode, &out.Generation, &out.Version, &out.OpenedAt, &out.ClosedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Window{SessionID: sessionID, State: WindowClosed, MatchMode: MatchExact}, nil
	}
	if err != nil {
		return Window{}, mapError(err)
	}
	return out, nil
}

func validMode(mode MatchMode) bool { return mode == MatchExact || mode == MatchKeywordQtyOnly }

// CreateOffer binds a canonical keyword to an active in-currency SKU of the store
// (live:manage), with receipt live.claim.offer.create and audit live.claim.offer.created.
// Inside the receipt it takes the 'claims-offers|t|s|session' advisory lock to enforce
// ≤200 offers per session without locking live.sessions (the FK's KEY SHARE proves
// existence). Duplicate keyword or a second active offer on the SKU → ErrConflict.
// Called by M3.
func CreateOffer(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID string, in OfferInput) (Offer, error) {
	keyword, ok := grammar.NormalizeKeyword(in.Keyword)
	if !ok || !command.ValidID(sessionID) || !command.ValidID(in.SKUID) ||
		in.MaxQuantityPerClaim < 1 || in.MaxQuantityPerClaim > grammar.MaxQuantity {
		return Offer{}, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Offer{}, err
	}
	// The canonical keyword (not the typed form) enters the request hash, so "ａ１" and
	// "A1" under one key replay the same receipt.
	request := struct {
		PrincipalID         string `json:"principal_id"`
		SessionID           string `json:"session_id"`
		Keyword             string `json:"keyword"`
		SKUID               string `json:"sku_id"`
		MaxQuantityPerClaim int64  `json:"max_quantity_per_claim"`
	}{scope.PrincipalID, sessionID, keyword, in.SKUID, in.MaxQuantityPerClaim}
	var out Offer
	err := command.Run(ctx, tx, scope, "live.claim.offer.create", key, request, &out, func() error {
		if err := waitAdvisory(ctx, tx, "claims-offers|"+scope.TenantID+"|"+scope.StoreID+"|"+sessionID); err != nil {
			return err
		}
		if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
			return err
		}
		if err := requireSession(ctx, tx, scope, sessionID); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM live.offers WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3`,
			scope.TenantID, scope.StoreID, sessionID).Scan(&count); err != nil {
			return mapError(err)
		}
		if count >= maxOffersPerSession {
			return command.ErrConflict
		}
		// catalog.skus/products/control.stores (catalog package tables): read-only check
		// that the SKU sells now in the store currency; redeem re-checks availability.
		var sellable bool
		err := tx.QueryRow(ctx, `SELECT s.status='active' AND p.status='active' AND s.currency=st.currency
			FROM catalog.skus s
			JOIN catalog.products p ON p.tenant_id=s.tenant_id AND p.store_id=s.store_id AND p.id=s.product_id
			JOIN control.stores st ON st.tenant_id=s.tenant_id AND st.id=s.store_id
			WHERE s.tenant_id=$1 AND s.store_id=$2 AND s.id=$3`, scope.TenantID, scope.StoreID, in.SKUID).Scan(&sellable)
		if err != nil {
			return mapError(err)
		}
		if !sellable {
			return command.ErrConflict
		}
		var offerID string
		err = tx.QueryRow(ctx, `INSERT INTO live.offers(tenant_id,store_id,session_id,keyword,sku_id,max_quantity_per_claim,principal_id)
			VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`,
			scope.TenantID, scope.StoreID, sessionID, keyword, in.SKUID, in.MaxQuantityPerClaim, scope.PrincipalID).Scan(&offerID)
		if err != nil {
			return mapError(err)
		}
		offers, err := readOffers(ctx, tx, scope, sessionID, offerID)
		if err != nil {
			return err
		}
		out = offers[0]
		return command.Audit(ctx, tx, scope, "live.claim.offer.created")
	})
	if err != nil {
		return Offer{}, mapError(err)
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Offer{}, err
	}
	return out, nil
}

// UpdateOffer changes max_quantity_per_claim and active with a version CAS (live:manage),
// receipt live.claim.offer.update, audit live.claim.offer.updated. It locks the offer FOR
// NO KEY UPDATE, so deactivation waits for in-flight ingests holding it FOR SHARE.
// Reactivation moves activated_at (earlier comments stay OFFER_INACTIVE) and fails with
// ErrConflict while another active offer holds the SKU. Lowering max never rewrites
// accepted lines. Called by M4.
func UpdateOffer(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID, offerID string, in OfferUpdate) (Offer, error) {
	if !command.ValidID(sessionID) || !command.ValidID(offerID) || in.ExpectedVersion < 1 || in.ExpectedVersion == math.MaxInt64 ||
		in.MaxQuantityPerClaim < 1 || in.MaxQuantityPerClaim > grammar.MaxQuantity {
		return Offer{}, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Offer{}, err
	}
	request := struct {
		PrincipalID         string `json:"principal_id"`
		SessionID           string `json:"session_id"`
		OfferID             string `json:"offer_id"`
		ExpectedVersion     int64  `json:"expected_version"`
		MaxQuantityPerClaim int64  `json:"max_quantity_per_claim"`
		Active              bool   `json:"active"`
	}{scope.PrincipalID, sessionID, offerID, in.ExpectedVersion, in.MaxQuantityPerClaim, in.Active}
	var out Offer
	err := command.Run(ctx, tx, scope, "live.claim.offer.update", key, request, &out, func() error {
		var version int64
		if err := tx.QueryRow(ctx, `SELECT version FROM live.offers
			WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND id=$4 FOR NO KEY UPDATE`,
			scope.TenantID, scope.StoreID, sessionID, offerID).Scan(&version); err != nil {
			return mapError(err)
		}
		if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
			return err
		}
		if version != in.ExpectedVersion {
			return command.ErrConflict
		}
		// In SET, "active" on the right-hand side is the pre-update value.
		if _, err := tx.Exec(ctx, `UPDATE live.offers SET max_quantity_per_claim=$5,active=$6,
			activated_at=CASE WHEN $6 AND NOT active THEN clock_timestamp() ELSE activated_at END,
			version=version+1,updated_at=clock_timestamp()
			WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND id=$4`,
			scope.TenantID, scope.StoreID, sessionID, offerID, in.MaxQuantityPerClaim, in.Active); err != nil {
			return mapError(err)
		}
		offers, err := readOffers(ctx, tx, scope, sessionID, offerID)
		if err != nil {
			return err
		}
		out = offers[0]
		return command.Audit(ctx, tx, scope, "live.claim.offer.updated")
	})
	if err != nil {
		return Offer{}, mapError(err)
	}
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return Offer{}, err
	}
	return out, nil
}

// readOffers returns the session's offers ORDER BY keyword, or exactly one offer when
// offerID is set (ErrNotFound if absent). SKU code and product name come from the catalog
// package's tables (read-only display join, merchant RLS).
func readOffers(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID, offerID string) ([]Offer, error) {
	rows, err := tx.Query(ctx, `SELECT o.id::text,o.session_id::text,o.keyword,o.sku_id::text,s.code,p.name,
		o.max_quantity_per_claim,o.active,o.version,o.activated_at,o.updated_at
		FROM live.offers o
		JOIN catalog.skus s ON s.tenant_id=o.tenant_id AND s.store_id=o.store_id AND s.id=o.sku_id
		JOIN catalog.products p ON p.tenant_id=s.tenant_id AND p.store_id=s.store_id AND p.id=s.product_id
		WHERE o.tenant_id=$1 AND o.store_id=$2 AND o.session_id=$3 AND ($4='' OR o.id=nullif($4,'')::uuid)
		ORDER BY o.keyword`, scope.TenantID, scope.StoreID, sessionID, offerID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	offers := []Offer{}
	for rows.Next() {
		var o Offer
		if err := rows.Scan(&o.ID, &o.SessionID, &o.Keyword, &o.SKUID, &o.SKUCode, &o.ProductName,
			&o.MaxQuantityPerClaim, &o.Active, &o.Version, &o.ActivatedAt, &o.UpdatedAt); err != nil {
			return nil, mapError(err)
		}
		offers = append(offers, o)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	if offerID != "" && len(offers) != 1 {
		return nil, command.ErrNotFound
	}
	return offers, nil
}

// readStats counts claims.events of one window generation by outcome and reason. Every
// boardReasons key is present (0 when none) and no other key is ever added (RATE_LIMITED rows are
// persisted but not reported yet); generation 0 (never opened) is all zero.
func readStats(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID string, generation int64) (Stats, error) {
	stats := Stats{Generation: generation, Rejected: map[Reason]int64{}}
	for _, reason := range boardReasons {
		stats.Rejected[reason] = 0
	}
	if generation == 0 {
		return stats, nil
	}
	rows, err := tx.Query(ctx, `SELECT coalesce(reason,''),count(*) FROM claims.events
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 AND window_generation=$4 GROUP BY reason`,
		scope.TenantID, scope.StoreID, sessionID, generation)
	if err != nil {
		return Stats{}, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var count int64
		if err := rows.Scan(&reason, &count); err != nil {
			return Stats{}, mapError(err)
		}
		if reason == "" {
			stats.Accepted = count
		} else if _, ok := stats.Rejected[Reason(reason)]; ok {
			stats.Rejected[Reason(reason)] = count
		}
	}
	if err := rows.Err(); err != nil {
		return Stats{}, mapError(err)
	}
	return stats, nil
}

// ListBundles pages the session's bundles newest first (live:read) with pagination
// collection "claim-bundles" (ParentID = session, keys created_at,id). Bundles, their
// aggregated lines and link state come from ONE statement, so a page cannot tear between
// a bundle and its lines. There is deliberately no label/handle filter. Link state reads
// only non-secret claims.links columns (never token_hash). Called by M6.
func ListBundles(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, sessionID string, page pagination.Request) (pagination.Page[Bundle], error) {
	empty := pagination.Page[Bundle]{Items: []Bundle{}}
	if !command.ValidID(sessionID) {
		return empty, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return empty, err
	}
	binding := pagination.Binding{TenantID: scope.TenantID, StoreID: scope.StoreID, Collection: "claim-bundles", ParentID: sessionID}
	limit, keys, err := pagination.Decode(page, binding, 2)
	if err != nil {
		return empty, err
	}
	if err := requireSession(ctx, tx, scope, sessionID); err != nil {
		return empty, err
	}
	var afterTime, afterID any
	if len(keys) == 2 {
		afterTime, afterID = keys[0], keys[1]
	}
	rows, err := tx.Query(ctx, `SELECT b.id::text,b.platform,coalesce(b.label,''),b.bound_at IS NOT NULL,b.version,
		b.created_at,b.updated_at,k.generation,k.expires_at,coalesce(k.expires_at>clock_timestamp(),false),
		coalesce((SELECT jsonb_agg(jsonb_build_object('offer_id',l.offer_id,'keyword',o.keyword,'sku_id',l.sku_id,
		  'quantity',l.quantity,'version',l.version,'applied',coalesce(l.applied_version=l.version,false)) ORDER BY o.keyword)
		 FROM claims.lines l JOIN live.offers o ON o.tenant_id=l.tenant_id AND o.store_id=l.store_id AND o.id=l.offer_id
		 WHERE l.tenant_id=b.tenant_id AND l.store_id=b.store_id AND l.bundle_id=b.id),'[]'::jsonb)
		FROM claims.bundles b
		LEFT JOIN claims.links k ON k.tenant_id=b.tenant_id AND k.store_id=b.store_id AND k.bundle_id=b.id
		WHERE b.tenant_id=$1 AND b.store_id=$2 AND b.session_id=$3
		 AND ($4::timestamptz IS NULL OR (b.created_at,b.id)<($4::timestamptz,$5::uuid))
		ORDER BY b.created_at DESC,b.id DESC LIMIT $6`,
		scope.TenantID, scope.StoreID, sessionID, afterTime, afterID, limit+1)
	if err != nil {
		return empty, mapError(err)
	}
	defer rows.Close()
	out := pagination.Page[Bundle]{Items: []Bundle{}}
	for rows.Next() {
		var b Bundle
		var generation *int64
		var active bool
		var lines []byte
		if err := rows.Scan(&b.ID, &b.Platform, &b.Label, &b.Bound, &b.Version, &b.CreatedAt, &b.UpdatedAt,
			&generation, &b.Link.ExpiresAt, &active, &lines); err != nil {
			return empty, mapError(err)
		}
		b.Ref = strings.ToUpper(b.ID[:8])
		b.Link.State = "NONE"
		if generation != nil {
			b.Link.Generation = *generation
			b.Link.State = "EXPIRED"
			if active {
				b.Link.State = "ACTIVE"
			}
		}
		if err := json.Unmarshal(lines, &b.Lines); err != nil {
			return empty, err
		}
		out.Items = append(out.Items, b)
	}
	if err := rows.Err(); err != nil {
		return empty, mapError(err)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		last := out.Items[len(out.Items)-1]
		if out.NextCursor, err = pagination.Encode(binding, []string{last.CreatedAt.UTC().Format(cursorTime), last.ID}); err != nil {
			return empty, err
		}
	}
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return empty, err
	}
	return out, nil
}
