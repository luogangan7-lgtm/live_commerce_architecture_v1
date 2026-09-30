package metaads

// ops.go: dispatch and reconcile of the eight meta_ads actions and the CAPI POST. Every request
// body is the frozen ads-core D11 JSON written by the ads planners (no tokens, no PII); it is
// decoded strictly and an unknown or invalid key ends the operation as FAILED_FINAL `bad_request`
// BEFORE any Graph call, so a planner bug can never become a half-formed remote write.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
)

// Dispatcher action names (contract §6.1).
const (
	ActionCreateCampaign = "meta.ads.create_campaign"
	ActionCreateAdset    = "meta.ads.create_adset"
	ActionCreateCreative = "meta.ads.create_creative"
	ActionCreateAd       = "meta.ads.create_ad"
	ActionPreflight      = "meta.ads.preflight_account"
	ActionActivate       = "meta.ads.activate"
	ActionPause          = "meta.ads.pause"
	ActionReadInsights   = "meta.ads.read_insights"
)

// Wire constants of the Marketing API. Field names: campaign https://developers.facebook.com/docs/marketing-api/reference/ad-account/campaigns/
// (F8), ad set https://developers.facebook.com/docs/marketing-api/reference/ad-campaign/ (F22), creative
// https://developers.facebook.com/docs/marketing-api/reference/ad-creative/ (F12); all retrieved 2026-09-30.
const (
	// statusPaused/statusActive: AD3 - the campaign is created PAUSED, children ACTIVE; the campaign
	// status POST is the only spend switch (F9).
	statusPaused = "PAUSED"
	statusActive = "ACTIVE"
	// bidStrategy needs no bid amount with a lifetime budget. // UNKNOWN until MA-S1 (U2 family).
	bidStrategy = "LOWEST_COST_WITHOUT_CAP"
	// purchaseAction is the aggregated all-channel purchase action in Insights `actions`.
	// // UNKNOWN until a LIVE read (F20: the sandbox has no Insights); MA-L1 confirms the name.
	purchaseAction = "omni_purchase"
	maxPages       = 10 // reconcile listing bound (G5): 10 pages of 100
	pageLimit      = "100"
	tagPrefix      = "lc-"
)

var (
	// specialAdCategories is U1: the value meaning "none" in v26. // UNKNOWN until MA-S1; [] is the
	// documented empty list (F8 lists the field as required).
	specialAdCategories = []string{}

	// goals is G4 (U2, closed by MA-S1): optimization_goal / billing_event per template.
	// // UNKNOWN until MA-S1: these pairs are not yet accepted by a real ad account.
	goals = map[string][2]string{
		"BOOST_POST":      {"POST_ENGAGEMENT", "IMPRESSIONS"},
		"PRODUCT_TRAFFIC": {"LINK_CLICKS", "IMPRESSIONS"},
	}
	// objectiveByName is ads-core D11: BOOST_POST -> OUTCOME_ENGAGEMENT, PRODUCT_TRAFFIC -> OUTCOME_TRAFFIC.
	objectiveByName = map[string]string{"OUTCOME_ENGAGEMENT": "BOOST_POST", "OUTCOME_TRAFFIC": "PRODUCT_TRAFFIC"}

	storyPattern   = regexp.MustCompile(`^[0-9]{1,40}_[0-9]{1,40}$`)
	countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)
	cursorPattern  = regexp.MustCompile(`^[A-Za-z0-9_=+/-]{1,512}$`)
	dayPattern     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

var badRequest = failedFinal("bad_request")

// Client is the Graph adapter behind the routes and PostEvent. It holds no credential.
type Client struct {
	g       *graph
	partner string
}

// NewClient validates cfg (host guard, version pin) and builds a client.
func NewClient(cfg Config) (*Client, error) {
	g, err := newGraph(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{g: g, partner: cfg.PartnerAgent}, nil
}

func decodeStrict(raw []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return false
	}
	var extra json.RawMessage
	return dec.Decode(&extra) == io.EOF
}

type baseReq struct {
	V       int    `json:"v"`
	DraftID string `json:"draft_id"`
	Attempt int    `json:"attempt"`
}

func (b baseReq) valid() bool {
	return b.V == 1 && command.ValidID(b.DraftID) && b.Attempt >= 1 && b.Attempt <= 5
}

// createSpec is one validated create: where to POST, what, and where the reconcile listing lives.
type createSpec struct {
	path     string
	payload  map[string]any
	listPath string
	name     string
}

func validID(s string) bool { return numericIDPattern.MatchString(s) }

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t.UTC(), err == nil
}

// buildCreate validates the frozen request of a create action against DispatchRequest and returns the
// wire call. ok=false means bad_request. The object name must be exactly "lc-<operation uuid>": that
// tag is the ONLY thing reconcile can match on (F9), so a different name would make the op unprovable.
func buildCreate(req core.DispatchRequest) (createSpec, bool) {
	account := "act_" + req.ExternalAssetID
	tag := tagPrefix + req.OperationID
	switch req.Action {
	case ActionCreateCampaign:
		var r struct {
			baseReq
			Name          string `json:"name"`
			Objective     string `json:"objective"`
			Currency      string `json:"currency"`
			SpendCapMinor int64  `json:"spend_cap_minor"`
		}
		if !decodeStrict(req.Request, &r) || !r.valid() || r.Name != tag || objectiveByName[r.Objective] == "" {
			return createSpec{}, false
		}
		payload := map[string]any{"name": tag, "objective": r.Objective, "status": statusPaused,
			"special_ad_categories": specialAdCategories}
		if _, err := MetaBudget(r.Currency, 0); err != nil || r.SpendCapMinor < 0 {
			return createSpec{}, false
		}
		if r.SpendCapMinor > 0 {
			// I05: spend_cap is a money amount in the currency's Meta offset; never truncated.
			capAmount, err := MetaBudget(r.Currency, r.SpendCapMinor)
			if err != nil {
				return createSpec{}, false
			}
			payload["spend_cap"] = capAmount
		}
		return createSpec{path: account + "/campaigns", payload: payload, listPath: account + "/campaigns", name: tag}, true
	case ActionCreateAdset:
		var r struct {
			baseReq
			Name                string   `json:"name"`
			CampaignID          string   `json:"campaign_id"`
			Template            string   `json:"template"`
			Currency            string   `json:"currency"`
			LifetimeBudgetMinor int64    `json:"lifetime_budget_minor"`
			StartTime           string   `json:"start_time"`
			EndTime             string   `json:"end_time"`
			Countries           []string `json:"countries"`
			AgeMin              int      `json:"age_min"`
			AgeMax              int      `json:"age_max"`
		}
		if !decodeStrict(req.Request, &r) || !r.valid() || r.Name != tag || !validID(r.CampaignID) {
			return createSpec{}, false
		}
		pair, known := goals[r.Template]
		start, sok := parseTime(r.StartTime)
		end, eok := parseTime(r.EndTime)
		if !known || !sok || !eok || !end.After(start) || len(r.Countries) < 1 || len(r.Countries) > 10 ||
			r.AgeMin < 18 || r.AgeMin > 65 || r.AgeMax < r.AgeMin || r.AgeMax > 65 || r.LifetimeBudgetMinor <= 0 {
			return createSpec{}, false
		}
		for _, c := range r.Countries {
			if !countryPattern.MatchString(c) {
				return createSpec{}, false
			}
		}
		// I05: lifetime_budget goes through the single money conversion (TWD never truncated).
		budget, err := MetaBudget(r.Currency, r.LifetimeBudgetMinor)
		if err != nil {
			return createSpec{}, false
		}
		return createSpec{path: account + "/adsets", listPath: r.CampaignID + "/adsets", name: tag, payload: map[string]any{
			"name": tag, "campaign_id": r.CampaignID, "lifetime_budget": budget,
			"start_time": start.Format(time.RFC3339), "end_time": end.Format(time.RFC3339),
			"billing_event": pair[1], "optimization_goal": pair[0], "bid_strategy": bidStrategy,
			"targeting": map[string]any{"geo_locations": map[string]any{"countries": r.Countries},
				"age_min": r.AgeMin, "age_max": r.AgeMax},
			"status": statusActive, // AD3: children ACTIVE under a PAUSED campaign (effective CAMPAIGN_PAUSED)
		}}, true
	case ActionCreateCreative:
		var r struct {
			baseReq
			Name                   string `json:"name"`
			Template               string `json:"template"`
			PageID                 string `json:"page_id"`
			ObjectStoryID          string `json:"object_story_id"`
			SourceInstagramMediaID string `json:"source_instagram_media_id"`
			InstagramUserID        string `json:"instagram_user_id"`
			LinkURL                string `json:"link_url"`
		}
		if !decodeStrict(req.Request, &r) || !r.valid() || r.Name != tag || (r.PageID != "" && !validID(r.PageID)) {
			return createSpec{}, false
		}
		// page_id is optional only for BOOST_POST of an Instagram media (Instagram-only store: the
		// identity has no Page, 0074 ads.request_for omits the key); the other two shapes need it.
		payload := map[string]any{"name": tag}
		switch {
		case r.Template == "BOOST_POST" && r.ObjectStoryID != "" && r.SourceInstagramMediaID == "" && r.InstagramUserID == "" && r.LinkURL == "":
			if r.PageID == "" || !storyPattern.MatchString(r.ObjectStoryID) {
				return createSpec{}, false
			}
			payload["object_story_id"] = r.ObjectStoryID
		case r.Template == "BOOST_POST" && r.SourceInstagramMediaID != "" && r.ObjectStoryID == "" && r.LinkURL == "":
			if !validID(r.SourceInstagramMediaID) || (r.InstagramUserID != "" && !validID(r.InstagramUserID)) {
				return createSpec{}, false
			}
			payload["source_instagram_media_id"] = r.SourceInstagramMediaID
			if r.InstagramUserID != "" {
				payload["instagram_user_id"] = r.InstagramUserID
			}
		case r.Template == "PRODUCT_TRAFFIC" && r.LinkURL != "" && r.ObjectStoryID == "" && r.SourceInstagramMediaID == "" && r.InstagramUserID == "":
			if r.PageID == "" || !validLink(r.LinkURL) {
				return createSpec{}, false
			}
			payload["object_story_spec"] = map[string]any{"page_id": r.PageID,
				"link_data": map[string]any{"link": r.LinkURL}}
		default:
			return createSpec{}, false
		}
		return createSpec{path: account + "/adcreatives", payload: payload, listPath: account + "/adcreatives", name: tag}, true
	case ActionCreateAd:
		var r struct {
			baseReq
			Name       string `json:"name"`
			AdsetID    string `json:"adset_id"`
			CreativeID string `json:"creative_id"`
		}
		if !decodeStrict(req.Request, &r) || !r.valid() || r.Name != tag || !validID(r.AdsetID) || !validID(r.CreativeID) {
			return createSpec{}, false
		}
		return createSpec{path: account + "/ads", listPath: r.AdsetID + "/ads", name: tag, payload: map[string]any{
			"name": tag, "adset_id": r.AdsetID, "creative": map[string]any{"creative_id": r.CreativeID},
			"status": statusActive,
		}}, true
	}
	return createSpec{}, false
}

// validLink: an https URL with a host, no credentials, no fragment, <= 500 bytes (the frozen
// link_url is the merchant storefront product URL, ads-core D11).
func validLink(s string) bool {
	u, err := url.Parse(s)
	return err == nil && len(s) <= 500 && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" && !strings.ContainsAny(s, " \t\r\n")
}

// statusReq is the frozen activate/pause request; the ad account id is DispatchRequest.ExternalAssetID.
type statusReq struct {
	baseReq
	Seq        int    `json:"seq"`
	CampaignID string `json:"campaign_id"`
}

func parseStatus(req core.DispatchRequest) (statusReq, bool) {
	var r statusReq
	if !decodeStrict(req.Request, &r) || !r.valid() || r.Seq < 1 || r.Seq > 50 || !validID(r.CampaignID) {
		return statusReq{}, false
	}
	return r, true
}

// dispatch is DispatchWithSecret for every meta_ads action.
func (c *Client) dispatch(ctx context.Context, req core.DispatchRequest, secret core.Secret) (core.Outcome, error) {
	token := secret.Reveal()
	if req.Provider != "meta_ads" || !validID(req.ExternalAssetID) || len(token) == 0 || !command.ValidID(req.OperationID) {
		return badRequest, nil
	}
	switch req.Action {
	case ActionCreateCampaign, ActionCreateAdset, ActionCreateCreative, ActionCreateAd:
		spec, ok := buildCreate(req)
		if !ok {
			return badRequest, nil
		}
		// One POST only. Any doubt is UNKNOWN (classifyCreate): Graph has no idempotency key (F9), a
		// second POST could create a duplicate object, so only the tag reconcile may follow.
		rep, err := c.g.do(ctx, http.MethodPost, spec.path, nil, token, spec.payload)
		return classifyCreate(rep, err), nil
	case ActionActivate, ActionPause:
		r, ok := parseStatus(req)
		if !ok {
			return badRequest, nil
		}
		target := statusActive
		if req.Action == ActionPause {
			target = statusPaused
		}
		// One POST only; never FAILED_FINAL (classifyStatusPost). The status GET decides afterwards.
		rep, err := c.g.do(ctx, http.MethodPost, r.CampaignID, nil, token, map[string]any{"status": target})
		return classifyStatusPost(rep, err), nil
	case ActionPreflight, ActionReadInsights:
		return c.read(ctx, req, token)
	}
	return badRequest, nil
}

// reconcile is ReconcileWithSecret (A-10): query only, never a POST. UNKNOWN stays UNKNOWN unless the
// remote state proves the outcome.
func (c *Client) reconcile(ctx context.Context, req core.DispatchRequest, secret core.Secret) (core.Outcome, error) {
	token := secret.Reveal()
	if req.Provider != "meta_ads" || !validID(req.ExternalAssetID) || len(token) == 0 || !command.ValidID(req.OperationID) {
		return unknown("bad_request"), nil // never FAILED_FINAL in reconcile: an effect may exist
	}
	switch req.Action {
	case ActionCreateCampaign, ActionCreateAdset, ActionCreateCreative, ActionCreateAd:
		spec, ok := buildCreate(req)
		if !ok {
			return unknown("bad_request"), nil
		}
		return c.reconcileCreate(ctx, spec, token), nil
	case ActionActivate, ActionPause:
		r, ok := parseStatus(req)
		if !ok {
			return unknown("bad_request"), nil
		}
		target := statusActive
		if req.Action == ActionPause {
			target = statusPaused
		}
		rep, err := c.g.do(ctx, http.MethodGet, r.CampaignID, url.Values{"fields": {"status,effective_status"}}, token, nil)
		if err != nil || !rep.ok() {
			return unconfirmed(), nil // a rejected status GET proves nothing about the campaign
		}
		var doc struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(rep.body, &doc) == nil && doc.Status == target {
			return core.Outcome{State: "SUCCEEDED", Code: "reconciled_status"}, nil
		}
		return unknown("status_unmatched"), nil
	case ActionPreflight, ActionReadInsights:
		// A read has no effect: reconcile repeats the read (G5).
		out, err := c.read(ctx, req, token)
		return out, err
	}
	return unknown("bad_request"), nil
}

// reconcileCreate lists the parent's children and pins the unique object tagged lc-<op uuid> (G5):
// one match SUCCEEDED, none stays UNKNOWN (never recreate: the create may simply not be listed yet),
// more than one UNKNOWN duplicate_remote_objects (needs review, never a guess).
func (c *Client) reconcileCreate(ctx context.Context, spec createSpec, token []byte) core.Outcome {
	var matches []string
	after := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"fields": {"id,name"}, "limit": {pageLimit}}
		if after != "" {
			q.Set("after", after)
		}
		rep, err := c.g.do(ctx, http.MethodGet, spec.listPath, q, token, nil)
		if err != nil || !rep.ok() {
			return unconfirmed()
		}
		var doc struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if json.Unmarshal(rep.body, &doc) != nil {
			return unconfirmed()
		}
		for _, item := range doc.Data {
			if item.Name == spec.name && validID(item.ID) {
				matches = append(matches, item.ID)
			}
		}
		if doc.Paging.Next == "" || !cursorPattern.MatchString(doc.Paging.Cursors.After) {
			break
		}
		after = doc.Paging.Cursors.After
	}
	switch len(matches) {
	case 0:
		return unknown("reconcile_unproven")
	case 1:
		return core.Outcome{State: "SUCCEEDED", Code: "reconciled_by_tag", ProviderReference: matches[0]}
	}
	return unknown("duplicate_remote_objects")
}

// read runs preflight_account / read_insights. A read has no effect: a 4xx Graph error is FAILED_FINAL
// (the planner plans the next seq), any doubt is UNKNOWN and reconcile repeats the read.
func (c *Client) read(ctx context.Context, req core.DispatchRequest, token []byte) (core.Outcome, error) {
	if req.Action == ActionPreflight {
		var r struct {
			baseReq
			Seq int `json:"seq"`
		}
		if !decodeStrict(req.Request, &r) || !r.valid() || r.Seq < 1 || r.Seq > 50 {
			return badRequest, nil
		}
		return c.readPreflight(ctx, req.ExternalAssetID, token), nil
	}
	var r struct {
		V          int    `json:"v"`
		DraftID    string `json:"draft_id"`
		CampaignID string `json:"campaign_id"`
		Day        string `json:"day"`
	}
	if !decodeStrict(req.Request, &r) || r.V != 1 || !command.ValidID(r.DraftID) || !validID(r.CampaignID) || !dayPattern.MatchString(r.Day) {
		return badRequest, nil
	}
	if _, err := time.Parse("2006-01-02", r.Day); err != nil {
		return badRequest, nil
	}
	return c.readInsights(ctx, req.ExternalAssetID, r.CampaignID, r.Day, token), nil
}

func (c *Client) readPreflight(ctx context.Context, asset string, token []byte) core.Outcome {
	rep, err := c.g.do(ctx, http.MethodGet, "act_"+asset,
		url.Values{"fields": {"account_status,currency,timezone_name,funding_source"}}, token, nil)
	if err != nil || !rep.ok() {
		return failureOutcome(rep, err)
	}
	var doc struct {
		AccountStatus *int            `json:"account_status"`
		Currency      string          `json:"currency"`
		TimezoneName  string          `json:"timezone_name"`
		FundingSource json.RawMessage `json:"funding_source"`
	}
	if json.Unmarshal(rep.body, &doc) != nil || doc.AccountStatus == nil {
		return unconfirmed()
	}
	// F10: an absent funding_source means "no delivery".
	funding := strings.Trim(string(doc.FundingSource), `"`)
	p := Preflight{Status: *doc.AccountStatus, Currency: doc.Currency, Timezone: doc.TimezoneName,
		Funded: funding != "" && funding != "null" && funding != "0"}
	ref, err := EncodePreflight(p)
	if err != nil {
		return failedFinal("bad_result")
	}
	return core.Outcome{State: "SUCCEEDED", Code: "graph_read", ProviderReference: ref}
}

func (c *Client) readInsights(ctx context.Context, asset, campaign, day string, token []byte) core.Outcome {
	// 1. account currency + timezone (the account's, F10); 2. campaign effective_status; 3. one day of insights.
	rep, err := c.g.do(ctx, http.MethodGet, "act_"+asset, url.Values{"fields": {"currency,timezone_name"}}, token, nil)
	if err != nil || !rep.ok() {
		return failureOutcome(rep, err)
	}
	var acct struct {
		Currency     string `json:"currency"`
		TimezoneName string `json:"timezone_name"`
	}
	if json.Unmarshal(rep.body, &acct) != nil {
		return unconfirmed()
	}
	rep, err = c.g.do(ctx, http.MethodGet, campaign, url.Values{"fields": {"effective_status"}}, token, nil)
	if err != nil || !rep.ok() {
		return failureOutcome(rep, err)
	}
	var camp struct {
		EffectiveStatus string `json:"effective_status"`
	}
	if json.Unmarshal(rep.body, &camp) != nil {
		return unconfirmed()
	}
	rng, _ := json.Marshal(map[string]string{"since": day, "until": day})
	rep, err = c.g.do(ctx, http.MethodGet, campaign+"/insights", url.Values{
		"fields": {"spend,impressions,clicks,actions,action_values"}, "time_range": {string(rng)}, "limit": {"2"}}, token, nil)
	if err != nil || !rep.ok() {
		return failureOutcome(rep, err)
	}
	var ins struct {
		Data []struct {
			Spend        string        `json:"spend"`
			Impressions  string        `json:"impressions"`
			Clicks       string        `json:"clicks"`
			Actions      []actionValue `json:"actions"`
			ActionValues []actionValue `json:"action_values"`
		} `json:"data"`
	}
	if json.Unmarshal(rep.body, &ins) != nil || len(ins.Data) > 1 {
		return unconfirmed()
	}
	d := InsightsDay{EffectiveStatus: camp.EffectiveStatus, Currency: acct.Currency, Timezone: acct.TimezoneName}
	if len(ins.Data) == 1 {
		row := ins.Data[0]
		var e1, e2 error
		spend := row.Spend
		if spend == "" {
			spend = "0" // Meta omits the metric when nothing was delivered
		}
		// I05: exact decimal x100, never rounded; more than 2 fraction digits ends the read as bad_spend.
		if d.SpendMinor, e1 = SpendMinor(d.Currency, spend); e1 != nil {
			return spendFailure(e1)
		}
		if d.Impressions, e2 = count(row.Impressions); e2 != nil {
			return failedFinal("bad_result")
		}
		if d.Clicks, e2 = count(row.Clicks); e2 != nil {
			return failedFinal("bad_result")
		}
		for _, a := range row.Actions {
			if a.Type == purchaseAction {
				n, err := count(a.Value)
				if err != nil {
					return failedFinal("bad_result")
				}
				d.Purchases = &n
			}
		}
		for _, a := range row.ActionValues {
			if a.Type == purchaseAction {
				v, err := SpendMinor(d.Currency, a.Value)
				if err != nil {
					return spendFailure(err)
				}
				d.PurchaseValueMinor = &v
			}
		}
	} else if _, err := SpendMinor(d.Currency, "0"); err != nil {
		return spendFailure(err)
	}
	ref, err := EncodeInsights(d)
	if err != nil {
		return failedFinal("bad_result")
	}
	return core.Outcome{State: "SUCCEEDED", Code: "graph_read", ProviderReference: ref}
}

type actionValue struct {
	Type  string `json:"action_type"`
	Value string `json:"value"`
}

func spendFailure(err error) core.Outcome {
	if err == ErrUnsupportedCurrency {
		return failedFinal("unsupported_currency")
	}
	return failedFinal("bad_spend")
}

func count(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	if !countPattern.MatchString(s) {
		return 0, ErrBadSpend
	}
	return strconv.ParseInt(s, 10, 64)
}

// PostEvent sends exactly one CAPI event for pixelID (contract §3 CAPI): POST /{version}/{pixel}/events
// with the caller's JSON body plus partner_agent (F7) and the token. It is called by the ads-capi
// route only (meta_dataset, meta.capi.purchase). Classification: 2xx with events_received == 1
// SUCCEEDED; a 4xx Graph error FAILED_FINAL (an event older than 7 days rejects the batch, F14);
// everything else UNKNOWN. UNKNOWN is never re-POSTed: server-to-server dedup is not documented
// (F15), so a resend could double-count a purchase. Invalid arguments end as FAILED_FINAL bad_request
// before any call; a missing PartnerAgent is a configuration error.
func (c *Client) PostEvent(ctx context.Context, token []byte, pixelID string, body []byte) (core.Outcome, error) {
	if c == nil || c.partner == "" {
		return core.Outcome{}, ErrConfig
	}
	var payload map[string]json.RawMessage
	var data []json.RawMessage
	if len(token) == 0 || !validID(pixelID) || json.Unmarshal(body, &payload) != nil ||
		json.Unmarshal(payload["data"], &data) != nil || len(data) != 1 {
		return badRequest, nil
	}
	generic := make(map[string]any, len(payload)+2)
	for k, v := range payload {
		generic[k] = v
	}
	generic["partner_agent"] = c.partner
	rep, err := c.g.do(ctx, http.MethodPost, pixelID+"/events", nil, token, generic)
	if err == nil && rep.ok() {
		var doc struct {
			EventsReceived int    `json:"events_received"`
			TraceID        string `json:"fbtrace_id"`
		}
		if json.Unmarshal(rep.body, &doc) == nil && doc.EventsReceived == 1 {
			ref := ""
			if len(doc.TraceID) <= 200 && !strings.ContainsAny(doc.TraceID, " \r\n\t") && !strings.ContainsFunc(doc.TraceID, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
				ref = doc.TraceID
			}
			return core.Outcome{State: "SUCCEEDED", Code: "graph_received", ProviderReference: ref}, nil
		}
		return unconfirmed(), nil
	}
	return failureOutcome(rep, err), nil
}
