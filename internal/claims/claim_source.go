// claim_source.go owns the merchant "comment source" of a live session (meta-claims-intake-v1 §2,
// ruling h): GetClaimSource reads the session's binding to a Facebook post / live video or
// Instagram media, PutClaimSource binds, re-binds, edits or deactivates it, and
// ParseClaimSourceInput turns what the merchant pasted into (platform, page part, item id).
//
// Non-goals: no Graph call (R1 has no read path: Instagram shortcodes and fb.watch links are
// rejected, never resolved), no redirect following, no Page-token handling, no intake or reply
// logic (internal/claimsintake, live.put_claim_source's own guards decide route, binding and
// credential validity), no HTTP decoding (internal/httpapi/claimsource.go).
//
// PUTs are serialized by a per-session advisory lock (claim-source-put|tenant|store|session) so
// expected_version is a real CAS; the CLOSED default live.claim_windows row is inserted when the session
// has none, because live.claim_sources references it.

package claims

import (
	"context"
	"errors"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"
)

// Claim-source errors (the httperror codes of the claim-source interface).
var (
	ErrInputInvalid      = errors.New("claim source input invalid")
	ErrInputUnresolvable = errors.New("claim source input unresolvable")
	ErrBindingMissing    = errors.New("claim source binding missing")
	ErrBindingAmbiguous  = errors.New("claim source binding ambiguous")
	ErrSourceConflict    = errors.New("claim source bound to another session")
	ErrVersionChanged    = errors.New("claim source version changed")
	ErrPageTokenMissing  = errors.New("claim source private reply without page token") // ruling s
)

// ClaimSource is the wire shape of one binding (frozen HTTP interface).
type ClaimSource struct {
	ID             string    `json:"id"`
	Platform       string    `json:"platform"`
	Object         string    `json:"object"`
	AssetID        string    `json:"asset_id"`
	SourceObjectID string    `json:"source_object_id"`
	PrivateReply   bool      `json:"private_reply"`
	ReplyLocale    string    `json:"reply_locale"`
	Active         bool      `json:"active"`
	Version        int64     `json:"version"`
	Verified       bool      `json:"verified"` // false for facebook until probe U1 records the live-video post_id form
	IntakeCount    int64     `json:"intake_count"`
	IntakeCapped   int64     `json:"intake_capped"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ClaimSourceView is the GET envelope. Platforms lists the providers ("facebook", "instagram", sorted,
// never null) with at least one enabled Meta binding in the store, so the UI shows its platform select
// only when both exist (ruling p).
type ClaimSourceView struct {
	Source    *ClaimSource `json:"source"`
	Platforms []string     `json:"platforms"`
}

// ClaimSourceInput is the exact PUT body. Platform is the optional hint of ruling p ("" | "facebook" |
// "instagram"): needed only for a bare numeric id on a store with both an enabled Facebook and an
// enabled Instagram binding; when given it must agree with the parsed input.
type ClaimSourceInput struct {
	Input           string `json:"input"`
	PrivateReply    bool   `json:"private_reply"`
	ReplyLocale     string `json:"reply_locale"`
	Active          bool   `json:"active"`
	ExpectedVersion int64  `json:"expected_version"`
	Platform        string `json:"platform,omitempty"`
}

// ClaimSourceRef is a parsed paste. Platform is "" for a bare numeric id (either Meta binding may
// own it). Page is the numeric Page id given in a URL path or before the "_" of a post id ("" when
// absent); Item is the post, video or media id.
type ClaimSourceRef struct {
	Platform string
	Page     string
	Item     string
}

var (
	digitsRe    = regexp.MustCompile(`^[0-9]{1,40}$`)
	postIDRe    = regexp.MustCompile(`^([0-9]{1,40})_([0-9]{1,39})$`)
	shortcodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{5,30}$`)
	pfbidRe     = regexp.MustCompile(`^pfbid[A-Za-z0-9]{10,}$`)
	pageNameRe  = regexp.MustCompile(`^[A-Za-z0-9.\-]{1,80}$`)
)

// withPlatform applies the optional PUT platform hint (ruling p) to a parsed paste: an unknown value is
// invalid, a hint that contradicts the parsed platform is input_invalid, otherwise it fills a bare id.
func withPlatform(ref ClaimSourceRef, platform string) (ClaimSourceRef, error) {
	switch platform {
	case "":
		return ref, nil
	case "facebook", "instagram":
		if ref.Platform != "" && ref.Platform != platform {
			return ClaimSourceRef{}, ErrInputInvalid
		}
		ref.Platform = platform
		return ref, nil
	}
	return ClaimSourceRef{}, command.ErrInvalid
}

// ParseClaimSourceInput is pure and never resolves anything over the network. Accepted: a numeric id
// ("123" or a delivered post id "<page>_<post>"), facebook.com/<page>/posts/<id>, facebook.com/<page>/videos/<id>
// (hosts facebook.com, www., m., web.); rejected as ErrInputUnresolvable (would need a Graph call or a redirect):
// fb.watch/*, pfbid post links, Instagram /p|/reel|/tv/<shortcode>; everything else is ErrInputInvalid. Query
// strings and fragments are ignored; userinfo, ports and non-https schemes are invalid.
func ParseClaimSourceInput(input string) (ClaimSourceRef, error) {
	s := strings.TrimSpace(input)
	if s == "" || len(s) > 1024 || !utf8.ValidString(s) || strings.ContainsAny(s, " \t\r\n\x00") {
		return ClaimSourceRef{}, ErrInputInvalid
	}
	if digitsRe.MatchString(s) {
		return ClaimSourceRef{Item: s}, nil
	}
	if m := postIDRe.FindStringSubmatch(s); m != nil {
		return ClaimSourceRef{Platform: "facebook", Page: m[1], Item: m[2]}, nil // "<page>_<post>" exists only on Facebook
	}
	raw := s
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return ClaimSourceRef{}, ErrInputInvalid
	}
	host := strings.ToLower(u.Hostname())
	segments := strings.FieldsFunc(u.EscapedPath(), func(r rune) bool { return r == '/' })
	switch host {
	case "fb.watch":
		return ClaimSourceRef{}, ErrInputUnresolvable
	case "facebook.com", "www.facebook.com", "m.facebook.com", "web.facebook.com":
		if len(segments) != 3 || (segments[1] != "posts" && segments[1] != "videos") {
			return ClaimSourceRef{}, ErrInputInvalid
		}
		ref := ClaimSourceRef{Platform: "facebook"}
		if digitsRe.MatchString(segments[0]) {
			ref.Page = segments[0]
		} else if !pageNameRe.MatchString(segments[0]) {
			return ClaimSourceRef{}, ErrInputInvalid
		}
		switch {
		case digitsRe.MatchString(segments[2]) && len(segments[2]) <= 39:
			ref.Item = segments[2]
			return ref, nil
		case pfbidRe.MatchString(segments[2]):
			return ClaimSourceRef{}, ErrInputUnresolvable
		}
		return ClaimSourceRef{}, ErrInputInvalid
	case "instagram.com", "www.instagram.com":
		// /p/<code>, /reel/<code>, /tv/<code>, also behind /<user>/: a shortcode needs a Graph call to become a media id.
		for i := 0; i+1 < len(segments) && i < 2; i++ {
			if (segments[i] == "p" || segments[i] == "reel" || segments[i] == "tv") && shortcodeRe.MatchString(segments[i+1]) &&
				len(segments) == i+2 {
				return ClaimSourceRef{}, ErrInputUnresolvable
			}
		}
		return ClaimSourceRef{}, ErrInputInvalid
	}
	return ClaimSourceRef{}, ErrInputInvalid
}

// objectFor composes the (object, source_object_id) that live.claim_sources stores for a parsed
// paste against the store's binding of provider/asset: a Facebook feed post_id is "<page>_<post>"
// as delivered (contract §2); an Instagram media id is the bare media.id.
func objectFor(ref ClaimSourceRef, provider, asset string) (object, id string, err error) {
	switch provider {
	case "facebook":
		if ref.Platform == "instagram" || (ref.Page != "" && ref.Page != asset) {
			return "", "", ErrInputInvalid
		}
		id = asset + "_" + ref.Item
		if len(id) > 80 {
			return "", "", ErrInputInvalid
		}
		return "page", id, nil
	case "instagram":
		if ref.Platform == "facebook" || ref.Page != "" {
			return "", "", ErrInputInvalid
		}
		return "instagram", ref.Item, nil
	}
	return "", "", ErrInputInvalid
}

// GetClaimSource returns the session's current source (live:read): the active one, else the most
// recently changed, else nil, plus the store's enabled Meta binding platforms. Read-only, no locks. Called by GET claim-source.
func GetClaimSource(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, sessionID string) (ClaimSourceView, error) {
	if !command.ValidID(sessionID) {
		return ClaimSourceView{}, command.ErrInvalid
	}
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return ClaimSourceView{}, err
	}
	if err := requireSession(ctx, tx, scope, sessionID); err != nil {
		return ClaimSourceView{}, err
	}
	current, err := loadSource(ctx, tx, scope, sessionID)
	if err != nil {
		return ClaimSourceView{}, err
	}
	platforms := []string{}
	rows, err := tx.Query(ctx, `SELECT DISTINCT provider FROM integration.bindings
		WHERE tenant_id=$1 AND store_id=$2 AND enabled AND provider IN ('facebook','instagram') ORDER BY provider`,
		scope.TenantID, scope.StoreID)
	if err != nil {
		return ClaimSourceView{}, mapError(err)
	}
	platforms, err = pgx.AppendRows(platforms, rows, pgx.RowTo[string])
	if err != nil {
		return ClaimSourceView{}, mapError(err)
	}
	if err := authorize(ctx, tx, scope, token, readPermission); err != nil {
		return ClaimSourceView{}, err
	}
	return ClaimSourceView{Source: current, Platforms: platforms}, nil
}

// sourceColumns: verified is derived (facebook stays unverified until probe U1).
const sourceColumns = `id::text,platform,object,asset_id,source_object_id,private_reply,reply_locale,active,version,
	intake_count,intake_capped,updated_at`

func scanClaimSource(row pgx.Row) (*ClaimSource, error) {
	var c ClaimSource
	err := row.Scan(&c.ID, &c.Platform, &c.Object, &c.AssetID, &c.SourceObjectID, &c.PrivateReply, &c.ReplyLocale,
		&c.Active, &c.Version, &c.IntakeCount, &c.IntakeCapped, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	c.Verified = c.Platform == "instagram"
	return &c, nil
}

func loadSource(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID string) (*ClaimSource, error) {
	return scanClaimSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM live.claim_sources
		WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3 ORDER BY active DESC,updated_at DESC,id LIMIT 1`,
		scope.TenantID, scope.StoreID, sessionID))
}

// resolveBinding picks the store's single enabled Meta binding for the pasted platform (a bare
// numeric id may belong to either platform: exactly one enabled Meta binding overall).
func resolveBinding(ctx context.Context, tx pgx.Tx, scope platform.Scope, ref ClaimSourceRef) (provider, asset string, err error) {
	providers := []string{"facebook", "instagram"}
	if ref.Platform != "" {
		providers = []string{ref.Platform}
	}
	rows, err := tx.Query(ctx, `SELECT provider,external_asset_id FROM integration.bindings
		WHERE tenant_id=$1 AND store_id=$2 AND enabled AND provider=ANY($3) ORDER BY provider,external_asset_id LIMIT 2`,
		scope.TenantID, scope.StoreID, providers)
	if err != nil {
		return "", "", mapError(err)
	}
	defer rows.Close()
	type pair struct{ provider, asset string }
	var found []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.provider, &p.asset); err != nil {
			return "", "", mapError(err)
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		return "", "", mapError(err)
	}
	switch len(found) {
	case 0:
		return "", "", ErrBindingMissing
	case 1:
		return found[0].provider, found[0].asset, nil
	}
	return "", "", ErrBindingAmbiguous
}

// PutClaimSource binds the session to the pasted Meta object (live:manage + integration:execute), with
// receipt live.claim_source.put and audit live.claim_source.put. in.ExpectedVersion is the version
// of the session's current source as GET returned it (0 when none). A different object than the
// current one is a re-bind: the current source is deactivated (CAS on its version) and the new
// object is bound (its own row, version 1, or the earlier row of that object re-activated), in one
// transaction, so a session never keeps two active sources. Called by PUT claim-source.
func PutClaimSource(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, sessionID string, in ClaimSourceInput) (ClaimSource, error) {
	ref, err := ParseClaimSourceInput(in.Input)
	if err != nil {
		return ClaimSource{}, err
	}
	if ref, err = withPlatform(ref, in.Platform); err != nil {
		return ClaimSource{}, err
	}
	if !command.ValidID(sessionID) || in.ExpectedVersion < 0 || in.ExpectedVersion == math.MaxInt64 ||
		(in.ReplyLocale != "zh-TW" && in.ReplyLocale != "zh-CN" && in.ReplyLocale != "en") {
		return ClaimSource{}, command.ErrInvalid
	}
	if err := authorizeSourceWrite(ctx, tx, scope, token); err != nil {
		return ClaimSource{}, err
	}
	request := struct {
		PrincipalID     string `json:"principal_id"`
		SessionID       string `json:"session_id"`
		Input           string `json:"input"`
		PrivateReply    bool   `json:"private_reply"`
		ReplyLocale     string `json:"reply_locale"`
		Active          bool   `json:"active"`
		ExpectedVersion int64  `json:"expected_version"`
		Platform        string `json:"platform"`
	}{scope.PrincipalID, sessionID, strings.TrimSpace(in.Input), in.PrivateReply, in.ReplyLocale, in.Active, in.ExpectedVersion, in.Platform}
	var out ClaimSource
	err = command.Run(ctx, tx, scope, "live.claim_source.put", key, request, &out, func() error {
		// One writer per session: without it two PUTs with the same expected_version both see "no
		// source" (or an inactive one), both skip the deactivation and INSERT different rows, leaving two
		// active sources that GET cannot show (LIMIT 1). Lock order: this advisory lock (its own
		// namespace, not Ingest's claim-source dedup key) -> window row (ensureWindow) -> source row;
		// IngestMetaIntake takes source -> window FOR SHARE and never this
		// lock, so the order cannot invert. Not claim_windows FOR UPDATE: that would take window before source.
		if err := waitAdvisory(ctx, tx, "claim-source-put|"+scope.TenantID+"|"+scope.StoreID+"|"+sessionID); err != nil {
			return err
		}
		if err := requireSession(ctx, tx, scope, sessionID); err != nil {
			return err
		}
		provider, asset, err := resolveBinding(ctx, tx, scope, ref)
		if err != nil {
			return err
		}
		object, objectID, err := objectFor(ref, provider, asset)
		if err != nil {
			return err
		}
		if err := ensureWindow(ctx, tx, scope, sessionID); err != nil {
			return err
		}
		// Read under the lock (read committed: this statement sees every earlier writer's commit).
		current, err := loadSource(ctx, tx, scope, sessionID)
		if err != nil {
			return err
		}
		if err := authorizeSourceWrite(ctx, tx, scope, token); err != nil {
			return err
		}
		// CAS on every path: the caller holds the version of the session's current source (0 when none).
		var currentVersion int64
		if current != nil {
			currentVersion = current.Version
		}
		if currentVersion != in.ExpectedVersion {
			return ErrVersionChanged
		}
		expected := in.ExpectedVersion
		if current != nil && (current.Object != object || current.AssetID != asset || current.SourceObjectID != objectID) {
			// Re-bind: retire the current source first (its version was just checked).
			if current.Active {
				if err := putSource(ctx, tx, sessionID, current.Object, current.AssetID, current.SourceObjectID,
					false, current.ReplyLocale, false, current.Version); err != nil {
					return err
				}
			}
			expected = 0
			var earlier int64
			err := tx.QueryRow(ctx, `SELECT version FROM live.claim_sources WHERE tenant_id=$1 AND store_id=$2 AND session_id=$3
				AND object=$4 AND asset_id=$5 AND source_object_id=$6`, scope.TenantID, scope.StoreID, sessionID,
				object, asset, objectID).Scan(&earlier)
			if err == nil {
				expected = earlier
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return mapError(err)
			}
		}
		if err := putSource(ctx, tx, sessionID, object, asset, objectID, in.PrivateReply, in.ReplyLocale, in.Active, expected); err != nil {
			return err
		}
		saved, err := loadSourceByKey(ctx, tx, scope, sessionID, object, asset, objectID)
		if err != nil {
			return err
		}
		out = saved
		return command.Audit(ctx, tx, scope, "live.claim_source.put")
	})
	if err != nil {
		return ClaimSource{}, mapSourceError(err)
	}
	if err := authorizeSourceWrite(ctx, tx, scope, token); err != nil {
		return ClaimSource{}, err
	}
	return out, nil
}

func authorizeSourceWrite(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) error {
	if err := authorize(ctx, tx, scope, token, managePermission); err != nil {
		return err
	}
	return authorize(ctx, tx, scope, token, "integration:execute")
}

// ensureWindow inserts the default CLOSED/EXACT window row (generation 0, exactly what SetWindow's
// first save would write and what GetBoard already reports) when the session has none, because
// live.claim_sources references live.claim_windows. Opening stays the merchant's separate act (M2).
func ensureWindow(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID string) error {
	w, err := readWindow(ctx, tx, scope, sessionID, "")
	if err != nil || w.Version != 0 {
		return err
	}
	_, err = writeWindow(ctx, tx, scope, sessionID, 0, WindowInput{State: WindowClosed, MatchMode: MatchExact})
	return err
}

// putSource calls live.put_claim_source (definer commerce_claims_writer): re-checks live:manage and
// integration:execute of app.principal_id, the enabled (object, asset) route and binding, the Page-token
// credential when private_reply, and the version CAS (0 creates).
func putSource(ctx context.Context, tx pgx.Tx, sessionID, object, asset, objectID string, privateReply bool, locale string, active bool, expected int64) error {
	var id string
	return tx.QueryRow(ctx, `SELECT live.put_claim_source($1::uuid,$2,$3,$4,$5,$6,$7,$8)::text`,
		sessionID, object, asset, objectID, privateReply, locale, active, expected).Scan(&id)
}

func loadSourceByKey(ctx context.Context, tx pgx.Tx, scope platform.Scope, sessionID, object, asset, objectID string) (ClaimSource, error) {
	c, err := scanClaimSource(tx.QueryRow(ctx, `SELECT `+sourceColumns+` FROM live.claim_sources WHERE tenant_id=$1 AND store_id=$2
		AND session_id=$3 AND object=$4 AND asset_id=$5 AND source_object_id=$6`, scope.TenantID, scope.StoreID, sessionID, object, asset, objectID))
	if err != nil {
		return ClaimSource{}, err
	}
	if c == nil {
		return ClaimSource{}, command.ErrNotFound
	}
	return *c, nil
}

// mapSourceError separates the put_claim_source outcomes claims.mapError would merge into one
// ErrConflict. The SQL messages are fixed strings authored in migration 0064 (no row data).
func mapSourceError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505": // live.claim_sources_one_active: the object feeds another session
			return ErrSourceConflict
		case "PT409":
			switch pg.Message {
			case "claim source version changed":
				return ErrVersionChanged
			case "claim source route mismatch", "claim source binding mismatch":
				return ErrBindingMissing
			case "claim source has no page credential": // ruling s: distinct from binding_missing
				return ErrPageTokenMissing
			}
			return command.ErrConflict
		}
	}
	return mapError(err)
}
