package fakegraph

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Route names, as computed from method + path (Fault.Route and Count use them).
const (
	RouteCreateCampaign = "create_campaign"
	RouteCreateAdset    = "create_adset"
	RouteCreateCreative = "create_creative"
	RouteCreateAd       = "create_ad"
	RouteStatusPost     = "status_post"
	RouteGetObject      = "get_object"
	RouteListCampaigns  = "list_campaigns"
	RouteListAdsets     = "list_adsets"
	RouteListCreatives  = "list_creatives"
	RouteListAds        = "list_ads"
	RouteAccountGet     = "account_get"
	RouteInsights       = "insights_get"
	RouteEvents         = "events_post"
	RouteOAuthToken     = "oauth_token"
	RouteMe             = "me"
	RoutePermissions    = "permissions"
	RouteAdAccounts     = "adaccounts"
	RouteAdPixels       = "adspixels"
	RouteUnknown        = "unknown"
)

// FaultKind is what a matching fault does.
type FaultKind int

const (
	// FaultTimeout hangs until the client gives up (the dispatcher CallTimeout). With Effect the remote effect (object
	// creation / status change) happens first, exactly the "timeout after create" case of F9/U8.
	FaultTimeout FaultKind = iota
	// Fault5xx answers 503 with a non-Graph body (Effect as above).
	Fault5xx
	// FaultGraphError answers HTTP (default 400) with a Graph error envelope carrying Code (no effect ever).
	FaultGraphError
	// FaultGarbled answers 200 with an unparseable body (Effect as above).
	FaultGarbled
	// FaultShapeless answers 200 {} (no id / no success / no events_received) (Effect as above).
	FaultShapeless
	// FaultNoSuccess answers 200 {"success":false} (status routes only; Effect as above).
	FaultNoSuccess
	// FaultHold blocks the request (server side) until Fault.Hold is closed, the client gives up, or 30 s pass, then serves it
	// NORMALLY (the effect happens even when the client already disconnected, as a real Graph would). Fault.Arrived, when set,
	// receives one signal when the request arrived: "the call is in flight".
	FaultHold
)

// Fault is one programmed misbehaviour; it fires Times times (0 = once) on the first request that matches.
type Fault struct {
	Route   string // exact route name; "" any
	Method  string // "" any
	Body    string // substring of the request body; "" any
	Path    string // substring of the path; "" any
	Kind    FaultKind
	Effect  bool
	HTTP    int // FaultGraphError only, default 400
	Code    int // FaultGraphError only
	Times   int
	Hold    chan struct{} // FaultHold only
	Arrived chan struct{} // FaultHold only (buffered; a non-blocking send)
	used    int
}

// Req is one captured request.
type Req struct {
	Seq         int
	Method      string
	Route       string
	Path        string // without the version prefix, e.g. "act_1/campaigns"
	Version     string
	RawQuery    string
	Header      http.Header
	Body        []byte
	Token       string // the access token as received
	TokenSource string // "bearer" | "body" | "query" | ""
	Status      int    // HTTP status answered
	Object      string // id created / touched, when any
}

// Object is one remote ad object.
type Object struct {
	ID, Kind, Name, Account, Parent, Status string
	Body                                    map[string]any
	overrideEffective                       string
}

// Account is one ad account the fake knows.
type Account struct {
	ID             string
	Currency       string
	Timezone       string
	Status         int
	Funded         bool
	MinDailyBudget string
}

// Pixel is a dataset visible to a token.
type Pixel struct{ ID, Name string }

// Insights is one campaign-day row.
type Insights struct{ Spend, Impressions, Clicks, PurchaseCount, PurchaseValue string }

type tokenInfo struct {
	client   string
	scopes   []string
	accounts []string
	pixels   map[string][]Pixel
}

// Server is the fake. Create with New, stop with Close.
type Server struct {
	srv *httptest.Server

	mu         sync.Mutex
	seq        int
	nextID     int64
	reqs       []Req
	accounts   map[string]*Account
	tokens     map[string]*tokenInfo
	codes      map[string]string // oauth code -> token (removed when used)
	usedCodes  map[string]bool
	objects    map[string]*Object
	order      []string // creation order of object ids
	insights   map[string]Insights
	faults     []*Fault
	violations []string
	appID      string
	appSecret  string
	redirect   string // when set, the code exchange must present exactly this redirect_uri
	pageSize   int
}

var routeRe = regexp.MustCompile(`^/(v[0-9]{1,3}\.[0-9]{1,2})/(.*)$`)

// New starts the fake on a loopback listener.
func New() *Server {
	s := &Server{nextID: 120000000000000, accounts: map[string]*Account{}, tokens: map[string]*tokenInfo{},
		codes: map[string]string{}, usedCodes: map[string]bool{}, objects: map[string]*Object{},
		insights: map[string]Insights{}, pageSize: 100}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// URL is the loopback base URL (http://127.0.0.1:port) for metaads.Config.GraphBaseURL.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the listener.
func (s *Server) Close() { s.srv.CloseClientConnections(); s.srv.Close() }

// SetPageSize makes list routes page after n rows (default 100).
func (s *Server) SetPageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// ExpectApp makes the code exchange require exactly this client id and secret, and (when non-empty) redirect_uri.
func (s *Server) ExpectApp(appID, secret, redirect string) {
	s.mu.Lock()
	s.appID, s.appSecret, s.redirect = appID, secret, redirect
	s.mu.Unlock()
}

// AddAccount registers an ad account.
func (s *Server) AddAccount(a Account) {
	s.mu.Lock()
	cp := a
	s.accounts[a.ID] = &cp
	s.mu.Unlock()
}

// SetAccount mutates an account (status, funding).
func (s *Server) SetAccount(id string, f func(*Account)) {
	s.mu.Lock()
	if a := s.accounts[id]; a != nil {
		f(a)
	}
	s.mu.Unlock()
}

// Grant registers a bearer token with a client business, granted scopes, the ad accounts it may act on and the pixels
// visible per account.
func (s *Server) Grant(token, clientBusiness string, scopes, accounts []string, pixels map[string][]Pixel) {
	s.mu.Lock()
	s.tokens[token] = &tokenInfo{client: clientBusiness, scopes: append([]string(nil), scopes...), accounts: append([]string(nil), accounts...), pixels: pixels}
	s.mu.Unlock()
}

// AddCode registers a single-use FLfB authorization code that exchanges for token.
func (s *Server) AddCode(code, token string) { s.mu.Lock(); s.codes[code] = token; s.mu.Unlock() }

// Inject appends a fault.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	cp := f
	s.faults = append(s.faults, &cp)
	s.mu.Unlock()
}

// ClearFaults removes every fault.
func (s *Server) ClearFaults() { s.mu.Lock(); s.faults = nil; s.mu.Unlock() }

// SetInsights sets the campaign-day row served by /{campaign}/insights.
func (s *Server) SetInsights(campaign, day string, in Insights) {
	s.mu.Lock()
	s.insights[campaign+"|"+day] = in
	s.mu.Unlock()
}

// SetEffectiveStatus overrides the effective_status served for an object (e.g. WITH_ISSUES, DISAPPROVED).
func (s *Server) SetEffectiveStatus(id, status string) {
	s.mu.Lock()
	if o := s.objects[id]; o != nil {
		o.overrideEffective = status
	}
	s.mu.Unlock()
}

// SetStatus changes an object's own status out of band (a remote change the platform did not make).
func (s *Server) SetStatus(id, status string) {
	s.mu.Lock()
	if o := s.objects[id]; o != nil {
		o.Status = status
	}
	s.mu.Unlock()
}

// Requests returns a copy of every captured request.
func (s *Server) Requests() []Req {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Req(nil), s.reqs...)
}

// Count is the number of requests of one route (any route when name is "").
func (s *Server) Count(route string) int {
	n := 0
	for _, r := range s.Requests() {
		if route == "" || r.Route == route {
			n++
		}
	}
	return n
}

// Posts counts every POST (the requests that can have an effect).
func (s *Server) Posts() int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == http.MethodPost {
			n++
		}
	}
	return n
}

// Mark returns the current request count; RequestsSince(mark) are the ones after it.
func (s *Server) Mark() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.reqs) }

// RequestsSince returns the requests after a Mark.
func (s *Server) RequestsSince(mark int) []Req {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mark > len(s.reqs) {
		mark = len(s.reqs)
	}
	return append([]Req(nil), s.reqs[mark:]...)
}

// Violations lists token->account isolation failures observed.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

// Objects returns the remote objects of one kind ("campaign","adset","creative","ad"; "" = all) in creation order.
func (s *Server) Objects(kind string) []Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Object
	for _, id := range s.order {
		if o := s.objects[id]; kind == "" || o.Kind == kind {
			cp := *o
			out = append(out, cp)
		}
	}
	return out
}

// Object returns one remote object.
func (s *Server) Object(id string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[id]
	if o == nil {
		return Object{}, false
	}
	return *o, true
}

// EffectiveStatus is the derived status (F9/F22): a child of a PAUSED campaign is CAMPAIGN_PAUSED.
func (s *Server) EffectiveStatus(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveLocked(id)
}

func (s *Server) effectiveLocked(id string) string {
	o := s.objects[id]
	if o == nil {
		return ""
	}
	if o.overrideEffective != "" {
		return o.overrideEffective
	}
	camp := s.campaignOfLocked(o)
	if o.Kind != "campaign" && camp != nil && camp.Status == "PAUSED" {
		return "CAMPAIGN_PAUSED"
	}
	return o.Status
}

func (s *Server) campaignOfLocked(o *Object) *Object {
	cur := o
	for i := 0; i < 4 && cur != nil && cur.Kind != "campaign"; i++ {
		cur = s.objects[cur.Parent]
	}
	if cur != nil && cur.Kind == "campaign" {
		return cur
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------------------------

func graphError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "OAuthException", "code": code, "fbtrace_id": "SYNTHTRACE"}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func classify(method, rest string, q url.Values) string {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case rest == "oauth/access_token":
		return RouteOAuthToken
	case rest == "me":
		return RouteMe
	case rest == "me/permissions":
		return RoutePermissions
	case rest == "me/adaccounts":
		return RouteAdAccounts
	case len(parts) == 1 && method == http.MethodGet && strings.HasPrefix(parts[0], "act_"):
		return RouteAccountGet
	case len(parts) == 1 && method == http.MethodGet:
		return RouteGetObject
	case len(parts) == 1 && method == http.MethodPost:
		return RouteStatusPost
	case len(parts) == 2 && method == http.MethodPost && strings.HasPrefix(parts[0], "act_"):
		switch parts[1] {
		case "campaigns":
			return RouteCreateCampaign
		case "adsets":
			return RouteCreateAdset
		case "adcreatives":
			return RouteCreateCreative
		case "ads":
			return RouteCreateAd
		}
	case len(parts) == 2 && method == http.MethodPost && parts[1] == "events":
		return RouteEvents
	case len(parts) == 2 && method == http.MethodGet:
		switch parts[1] {
		case "campaigns":
			return RouteListCampaigns
		case "adsets":
			return RouteListAdsets
		case "adcreatives":
			return RouteListCreatives
		case "ads":
			return RouteListAds
		case "insights":
			return RouteInsights
		case "adspixels":
			return RouteAdPixels
		}
	}
	return RouteUnknown
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	m := routeRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		graphError(w, 404, 803, "unknown path")
		return
	}
	version, rest := m[1], m[2]
	q := r.URL.Query()
	route := classify(r.Method, rest, q)

	// token discovery: bearer header, JSON/form body, or URL query
	token, source := "", ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token, source = strings.TrimPrefix(h, "Bearer "), "bearer"
	}
	var payload map[string]any
	if len(body) > 0 {
		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "json") {
			_ = json.Unmarshal(body, &payload)
		} else if vals, err := url.ParseQuery(string(body)); err == nil {
			payload = map[string]any{}
			for k := range vals {
				payload[k] = vals.Get(k)
			}
		}
	}
	if token == "" {
		if v, ok := payload["access_token"].(string); ok && v != "" {
			token, source = v, "body"
		}
	}
	if token == "" && q.Get("access_token") != "" {
		token, source = q.Get("access_token"), "query"
	}

	s.mu.Lock()
	s.seq++
	rec := Req{Seq: s.seq, Method: r.Method, Route: route, Path: rest, Version: version, RawQuery: r.URL.RawQuery,
		Header: r.Header.Clone(), Body: body, Token: token, TokenSource: source}
	idx := len(s.reqs)
	s.reqs = append(s.reqs, rec)
	fault := s.matchFaultLocked(&rec)
	s.mu.Unlock()

	if fault != nil && fault.Kind == FaultHold {
		if fault.Arrived != nil {
			select {
			case fault.Arrived <- struct{}{}:
			default:
			}
		}
		select {
		case <-fault.Hold:
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
		fault = nil
	}
	status, object := s.handle(w, r, route, rest, q, payload, token, fault, &rec)

	s.mu.Lock()
	s.reqs[idx].Status = status
	s.reqs[idx].Object = object
	s.mu.Unlock()
}

func (s *Server) matchFaultLocked(rec *Req) *Fault {
	for _, f := range s.faults {
		limit := f.Times
		if limit == 0 {
			limit = 1
		}
		if f.used >= limit {
			continue
		}
		if (f.Route != "" && f.Route != rec.Route) || (f.Method != "" && f.Method != rec.Method) ||
			(f.Path != "" && !strings.Contains(rec.Path, f.Path)) || (f.Body != "" && !strings.Contains(string(rec.Body), f.Body)) {
			continue
		}
		f.used++
		cp := *f
		return &cp
	}
	return nil
}

// applyFault answers per the fault. effectDone reports whether the caller already applied the effect.
func (s *Server) applyFault(w http.ResponseWriter, r *http.Request, f *Fault) int {
	switch f.Kind {
	case FaultTimeout:
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return 0
	case Fault5xx:
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		return 503
	case FaultGraphError:
		st := f.HTTP
		if st == 0 {
			st = 400
		}
		graphError(w, st, f.Code, "synthetic graph error")
		return st
	case FaultGarbled:
		_, _ = w.Write([]byte("<html>not json"))
		return 200
	case FaultShapeless:
		_, _ = w.Write([]byte("{}"))
		return 200
	case FaultNoSuccess:
		writeJSON(w, map[string]any{"success": false})
		return 200
	}
	return 200
}

func (s *Server) authorize(w http.ResponseWriter, token, account string, rec *Req) bool {
	s.mu.Lock()
	info := s.tokens[token]
	s.mu.Unlock()
	if info == nil {
		graphError(w, 401, 190, "invalid access token")
		return false
	}
	if account == "" {
		return true
	}
	for _, a := range info.accounts {
		if a == account {
			return true
		}
	}
	s.mu.Lock()
	s.violations = append(s.violations, fmt.Sprintf("seq %d %s %s: token not granted on account %s", rec.Seq, rec.Method, rec.Path, account))
	s.mu.Unlock()
	graphError(w, 403, 200, "token not granted on this ad account")
	return false
}

func (s *Server) newObject(kind, name, account, parent, status string, body map[string]any) *Object {
	s.nextID++
	id := strconv.FormatInt(s.nextID, 10)
	o := &Object{ID: id, Kind: kind, Name: name, Account: account, Parent: parent, Status: status, Body: body}
	s.objects[id] = o
	s.order = append(s.order, id)
	return o
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

// handle answers one request. It returns the HTTP status (0 = hung) and the object id touched.
func (s *Server) handle(w http.ResponseWriter, r *http.Request, route, rest string, q url.Values, payload map[string]any, token string, fault *Fault, rec *Req) (int, string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch route {
	case RouteOAuthToken:
		return s.oauthToken(w, q, fault, r), ""
	case RouteMe, RoutePermissions, RouteAdAccounts:
		if !s.authorize(w, token, "", rec) {
			return 401, ""
		}
		if fault != nil {
			return s.applyFault(w, r, fault), ""
		}
		return s.meRoutes(w, route, q, token), ""
	case RouteCreateCampaign, RouteCreateAdset, RouteCreateCreative, RouteCreateAd:
		account := strings.TrimPrefix(parts[0], "act_")
		if !s.authorize(w, token, account, rec) {
			return 403, ""
		}
		return s.create(w, r, route, account, payload, fault)
	case RouteStatusPost:
		s.mu.Lock()
		o := s.objects[parts[0]]
		account := ""
		if o != nil {
			account = o.Account
		}
		s.mu.Unlock()
		if o != nil && !s.authorize(w, token, account, rec) {
			return 403, o.ID
		}
		if o == nil && !s.authorize(w, token, "", rec) {
			return 401, ""
		}
		return s.statusPost(w, r, parts[0], payload, fault)
	case RouteGetObject:
		s.mu.Lock()
		o := s.objects[parts[0]]
		account := ""
		if o != nil {
			account = o.Account
		}
		s.mu.Unlock()
		if !s.authorize(w, token, account, rec) {
			return 403, ""
		}
		if fault != nil {
			return s.applyFault(w, r, fault), ""
		}
		if o == nil {
			graphError(w, 400, 100, "unsupported get request: object does not exist")
			return 400, ""
		}
		s.mu.Lock()
		out := map[string]any{"id": o.ID, "status": o.Status, "effective_status": s.effectiveLocked(o.ID), "name": o.Name}
		s.mu.Unlock()
		writeJSON(w, out)
		return 200, o.ID
	case RouteAccountGet:
		account := strings.TrimPrefix(parts[0], "act_")
		if !s.authorize(w, token, account, rec) {
			return 403, ""
		}
		if fault != nil {
			return s.applyFault(w, r, fault), ""
		}
		s.mu.Lock()
		a := s.accounts[account]
		s.mu.Unlock()
		if a == nil {
			graphError(w, 400, 100, "unsupported get request: ad account does not exist")
			return 400, ""
		}
		out := map[string]any{"id": "act_" + a.ID, "account_id": a.ID, "account_status": a.Status, "currency": a.Currency,
			"timezone_name": a.Timezone, "min_daily_budget": a.MinDailyBudget}
		if a.Funded {
			out["funding_source"] = "9000000000001"
		}
		writeJSON(w, out)
		return 200, ""
	case RouteListCampaigns, RouteListAdsets, RouteListCreatives, RouteListAds:
		return s.list(w, r, route, parts[0], q, token, fault, rec)
	case RouteInsights:
		s.mu.Lock()
		o := s.objects[parts[0]]
		account := ""
		if o != nil {
			account = o.Account
		}
		s.mu.Unlock()
		if !s.authorize(w, token, account, rec) {
			return 403, ""
		}
		if fault != nil {
			return s.applyFault(w, r, fault), ""
		}
		var tr struct{ Since, Until string }
		_ = json.Unmarshal([]byte(q.Get("time_range")), &tr)
		s.mu.Lock()
		row, ok := s.insights[parts[0]+"|"+tr.Since]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, map[string]any{"data": []any{}})
			return 200, parts[0]
		}
		item := map[string]any{"spend": row.Spend, "impressions": row.Impressions, "clicks": row.Clicks, "date_start": tr.Since, "date_stop": tr.Since}
		if row.PurchaseCount != "" {
			item["actions"] = []any{map[string]any{"action_type": "omni_purchase", "value": row.PurchaseCount}}
		}
		if row.PurchaseValue != "" {
			item["action_values"] = []any{map[string]any{"action_type": "omni_purchase", "value": row.PurchaseValue}}
		}
		writeJSON(w, map[string]any{"data": []any{item}})
		return 200, parts[0]
	case RouteAdPixels:
		account := strings.TrimPrefix(parts[0], "act_")
		if !s.authorize(w, token, account, rec) {
			return 403, ""
		}
		if fault != nil {
			return s.applyFault(w, r, fault), ""
		}
		s.mu.Lock()
		info := s.tokens[token]
		var data []any
		for _, p := range info.pixels[account] {
			data = append(data, map[string]any{"id": p.ID, "name": p.Name})
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"data": data})
		return 200, ""
	case RouteEvents:
		return s.events(w, r, parts[0], payload, token, fault, rec)
	}
	graphError(w, 404, 803, "unsupported route "+rest)
	return 404, ""
}

func (s *Server) oauthToken(w http.ResponseWriter, q url.Values, fault *Fault, r *http.Request) int {
	if fault != nil {
		return s.applyFault(w, r, fault)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appID != "" && (q.Get("client_id") != s.appID || q.Get("client_secret") != s.appSecret) {
		graphError(w, 400, 101, "invalid client")
		return 400
	}
	if s.redirect != "" && q.Get("redirect_uri") != s.redirect {
		graphError(w, 400, 100, "redirect_uri mismatch")
		return 400
	}
	code := q.Get("code")
	token, ok := s.codes[code]
	if !ok || s.usedCodes[code] {
		graphError(w, 400, 100, "invalid or used verification code")
		return 400
	}
	s.usedCodes[code] = true
	delete(s.codes, code)
	writeJSON(w, map[string]any{"access_token": token, "token_type": "bearer"})
	return 200
}

func (s *Server) meRoutes(w http.ResponseWriter, route string, q url.Values, token string) int {
	s.mu.Lock()
	info := s.tokens[token]
	s.mu.Unlock()
	switch route {
	case RouteMe:
		writeJSON(w, map[string]any{"id": "1000000000001", "client_business_id": info.client})
	case RoutePermissions:
		var data []any
		for _, sc := range info.scopes {
			data = append(data, map[string]any{"permission": sc, "status": "granted"})
		}
		data = append(data, map[string]any{"permission": "declined_synthetic", "status": "declined"})
		writeJSON(w, map[string]any{"data": data})
	case RouteAdAccounts:
		ids := append([]string(nil), info.accounts...)
		sort.Strings(ids)
		start := 0
		if after := q.Get("after"); after != "" {
			start, _ = strconv.Atoi(after)
		}
		size := s.pageSize
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		var data []any
		s.mu.Lock()
		for _, id := range ids[start:end] {
			a := s.accounts[id]
			if a == nil {
				continue
			}
			data = append(data, map[string]any{"id": "act_" + a.ID, "account_id": a.ID, "name": "Synthetic " + a.ID,
				"currency": a.Currency, "timezone_name": a.Timezone, "account_status": a.Status})
		}
		s.mu.Unlock()
		out := map[string]any{"data": data}
		if end < len(ids) {
			out["paging"] = map[string]any{"cursors": map[string]any{"after": strconv.Itoa(end)}, "next": "http://127.0.0.1/next"}
		}
		writeJSON(w, out)
	}
	return 200
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, route, account string, p map[string]any, fault *Fault) (int, string) {
	// validation first: a real Graph rejects malformed creates with a 400 error envelope and no object (no effect)
	if msg := validateCreate(route, p); msg != "" {
		graphError(w, 400, 100, msg)
		return 400, ""
	}
	s.mu.Lock()
	if s.accounts[account] == nil {
		s.mu.Unlock()
		graphError(w, 400, 100, "ad account does not exist")
		return 400, ""
	}
	if fault != nil && !fault.Effect {
		s.mu.Unlock()
		return s.applyFault(w, r, fault), ""
	}
	var o *Object
	name := str(p, "name")
	switch route {
	case RouteCreateCampaign:
		o = s.newObject("campaign", name, account, "", str(p, "status"), p)
	case RouteCreateAdset:
		o = s.newObject("adset", name, account, str(p, "campaign_id"), str(p, "status"), p)
	case RouteCreateCreative:
		o = s.newObject("creative", name, account, "", "ACTIVE", p)
	case RouteCreateAd:
		parent := str(p, "adset_id")
		o = s.newObject("ad", name, account, parent, str(p, "status"), p)
	}
	id := o.ID
	s.mu.Unlock()
	if fault != nil { // Effect: true — the object exists, the caller is told something else
		return s.applyFault(w, r, fault), id
	}
	writeJSON(w, map[string]any{"id": id})
	return 200, id
}

func validateCreate(route string, p map[string]any) string {
	if str(p, "name") == "" {
		return "missing name"
	}
	switch route {
	case RouteCreateCampaign:
		obj := str(p, "objective")
		okObj := map[string]bool{"OUTCOME_TRAFFIC": true, "OUTCOME_AWARENESS": true, "OUTCOME_ENGAGEMENT": true, "OUTCOME_SALES": true, "OUTCOME_LEADS": true, "OUTCOME_APP_PROMOTION": true}
		if !okObj[obj] {
			return "invalid objective"
		}
		if _, ok := p["special_ad_categories"]; !ok {
			return "special_ad_categories is required"
		}
		if st := str(p, "status"); st != "PAUSED" && st != "ACTIVE" {
			return "invalid status"
		}
	case RouteCreateAdset:
		for _, k := range []string{"campaign_id", "start_time", "end_time", "billing_event", "optimization_goal", "status"} {
			if str(p, k) == "" {
				return "missing " + k
			}
		}
		if _, ok := p["lifetime_budget"]; !ok {
			return "missing lifetime_budget"
		}
		if _, ok := p["daily_budget"]; ok {
			return "daily budgets are not used in v1"
		}
		if _, ok := p["targeting"].(map[string]any); !ok {
			return "missing targeting"
		}
	case RouteCreateCreative:
		_, a := p["object_story_id"]
		_, b := p["object_story_spec"]
		_, c := p["source_instagram_media_id"]
		if !a && !b && !c {
			return "creative needs object_story_id, object_story_spec or source_instagram_media_id"
		}
	case RouteCreateAd:
		if str(p, "adset_id") == "" || p["creative"] == nil || str(p, "status") == "" {
			return "missing adset_id/creative/status"
		}
	}
	return ""
}

func (s *Server) statusPost(w http.ResponseWriter, r *http.Request, id string, p map[string]any, fault *Fault) (int, string) {
	s.mu.Lock()
	o := s.objects[id]
	st := str(p, "status")
	if o == nil || (st != "ACTIVE" && st != "PAUSED") {
		s.mu.Unlock()
		graphError(w, 400, 100, "invalid status update")
		return 400, ""
	}
	if fault != nil && !fault.Effect {
		s.mu.Unlock()
		return s.applyFault(w, r, fault), id
	}
	o.Status = st
	s.mu.Unlock()
	if fault != nil {
		return s.applyFault(w, r, fault), id
	}
	writeJSON(w, map[string]any{"success": true})
	return 200, id
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, route, parent string, q url.Values, token string, fault *Fault, rec *Req) (int, string) {
	kind := map[string]string{RouteListCampaigns: "campaign", RouteListAdsets: "adset", RouteListCreatives: "creative", RouteListAds: "ad"}[route]
	account := ""
	if strings.HasPrefix(parent, "act_") {
		account = strings.TrimPrefix(parent, "act_")
	} else {
		s.mu.Lock()
		if o := s.objects[parent]; o != nil {
			account = o.Account
		}
		s.mu.Unlock()
	}
	if !s.authorize(w, token, account, rec) {
		return 403, ""
	}
	if fault != nil {
		return s.applyFault(w, r, fault), ""
	}
	s.mu.Lock()
	var rows []Object
	for _, id := range s.order {
		o := s.objects[id]
		if o.Kind != kind {
			continue
		}
		if strings.HasPrefix(parent, "act_") {
			if o.Account != account {
				continue
			}
		} else if o.Parent != parent {
			continue
		}
		rows = append(rows, *o)
	}
	size := s.pageSize
	s.mu.Unlock()
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l < size {
		size = l
	}
	start := 0
	if after := q.Get("after"); after != "" {
		start, _ = strconv.Atoi(after)
	}
	end := start + size
	if end > len(rows) {
		end = len(rows)
	}
	var data []any
	if start < len(rows) {
		for _, o := range rows[start:end] {
			data = append(data, map[string]any{"id": o.ID, "name": o.Name})
		}
	}
	out := map[string]any{"data": data}
	if end < len(rows) {
		out["paging"] = map[string]any{"cursors": map[string]any{"before": "0", "after": strconv.Itoa(end)}, "next": "http://127.0.0.1/next?after=" + strconv.Itoa(end)}
	} else if len(data) > 0 {
		out["paging"] = map[string]any{"cursors": map[string]any{"before": "0", "after": strconv.Itoa(end)}}
	}
	writeJSON(w, out)
	return 200, ""
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, pixel string, p map[string]any, token string, fault *Fault, rec *Req) (int, string) {
	if !s.authorize(w, token, "", rec) {
		return 401, ""
	}
	if fault != nil {
		return s.applyFault(w, r, fault), ""
	}
	data, _ := p["data"].([]any)
	if len(data) != 1 {
		graphError(w, 400, 100, "expected exactly one event in data")
		return 400, ""
	}
	ev, _ := data[0].(map[string]any)
	et, _ := ev["event_time"].(float64)
	if et == 0 || time.Since(time.Unix(int64(et), 0)) > 7*24*time.Hour {
		graphError(w, 400, 100, "event_time older than 7 days: the whole batch is rejected")
		return 400, ""
	}
	if str(ev, "action_source") == "website" {
		ud, _ := ev["user_data"].(map[string]any)
		if str(ev, "event_source_url") == "" || str(ud, "client_user_agent") == "" {
			graphError(w, 400, 100, "website events need event_source_url and client_user_agent")
			return 400, ""
		}
	}
	writeJSON(w, map[string]any{"events_received": 1, "messages": []any{}, "fbtrace_id": "SYNTHTRACE"})
	return 200, pixel
}

// Seed creates a remote object out of band (no request is recorded), e.g. decoys and duplicate tag matches for reconcile
// gates. kind is campaign|adset|creative|ad; parent is the campaign id (adset) or ad set id (ad), "" otherwise.
func (s *Server) Seed(kind, name, account, parent, status string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newObject(kind, name, account, parent, status, map[string]any{"seeded": true}).ID
}
