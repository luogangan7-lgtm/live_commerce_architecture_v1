package metaads

// classify.go: the contract §3 classification tables, verbatim. The single rule behind all of them:
// a Graph create/status POST has NO idempotency key (F9), so the only response that may end an
// operation as FAILED_FINAL is a 4xx whose body is a parseable Graph error (the request was
// rejected); every doubt (timeout, 5xx, transport error, unparseable body, 2xx without the expected
// shape) is UNKNOWN, and the only follow-up is the query-only reconcile, never a second POST.

import (
	"encoding/json"
	"strconv"

	"livecommerce/internal/integrations/core"
)

// Graph error codes that mean "throttled", not "rejected" (contract §3: FAILED_FINAL `rate_limited`,
// a retry is a new publish attempt, §5.3). Sources: F13 insights best practices (code 4) and F21
// rate limiting (BUC, 80004), retrieved 2026-09-29:
// https://developers.facebook.com/docs/marketing-api/insights/best-practices/
// https://developers.facebook.com/docs/marketing-api/overview/rate-limiting/
// Codes 17 (user-level) and 613 (custom-level) are named by contract §3; same pages, same date.
const (
	codeAppLimit    = 4
	codeUserLimit   = 17
	codeCustomLimit = 613
	codeBUCLimit    = 80004
	// codeInvalidParameter (100) is the generic "invalid parameter" error (also the U9 dev-mode
	// creative failure, subcode 1885183). It is deliberately NOT special-cased: it ends an op as
	// FAILED_FINAL graph_100 like every other rejected request. Same F-pages, retrieved 2026-09-29.
	codeInvalidParameter = 100
)

func unknown(code string) core.Outcome     { return core.Outcome{State: "UNKNOWN", Code: code} }
func failedFinal(code string) core.Outcome { return core.Outcome{State: "FAILED_FINAL", Code: code} }
func unconfirmed() core.Outcome            { return unknown("graph_unconfirmed") }

// graphErrorCode extracts error.code from a Graph error body.
func graphErrorCode(body []byte) (int, bool) {
	var doc struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Error == nil || doc.Error.Code < 1 || doc.Error.Code > 999999 {
		return 0, false
	}
	return doc.Error.Code, true
}

// rejection is the FAILED_FINAL outcome for a 4xx Graph error body, or ok=false when the response is
// not a proven rejection (then the caller records UNKNOWN).
func rejection(rep reply, err error) (core.Outcome, bool) {
	if err != nil || rep.status < 400 || rep.status > 499 {
		return core.Outcome{}, false
	}
	code, ok := graphErrorCode(rep.body)
	if !ok {
		return core.Outcome{}, false
	}
	switch code {
	case codeAppLimit, codeUserLimit, codeCustomLimit, codeBUCLimit:
		return failedFinal("rate_limited"), true
	}
	return failedFinal("graph_" + strconv.Itoa(code)), true
}

// failureOutcome classifies a response that is not the expected 2xx for a create or a read.
func failureOutcome(rep reply, err error) core.Outcome {
	if out, ok := rejection(rep, err); ok {
		return out
	}
	return unconfirmed()
}

// classifyCreate: 2xx with a numeric `id` is SUCCEEDED with the id as provider_reference (it becomes
// ads.remote_objects.remote_id, `^[0-9]{1,40}$`); 2xx without one is UNKNOWN (the object may exist).
func classifyCreate(rep reply, err error) core.Outcome {
	if err == nil && rep.ok() {
		if id, ok := numericField(rep.body, "id"); ok {
			return core.Outcome{State: "SUCCEEDED", Code: "graph_created", ProviderReference: id}
		}
		return unconfirmed()
	}
	return failureOutcome(rep, err)
}

// classifyStatusPost is activate/pause: `{"success":true}` is SUCCEEDED and EVERYTHING else is
// UNKNOWN, never FAILED_FINAL: a rate-limited or rejected status POST does not prove the campaign's
// state, and the status GET (reconcile) is the only proof.
func classifyStatusPost(rep reply, err error) core.Outcome {
	if err == nil && rep.ok() {
		var doc struct {
			Success bool `json:"success"`
		}
		if json.Unmarshal(rep.body, &doc) == nil && doc.Success {
			return core.Outcome{State: "SUCCEEDED", Code: "graph_success"}
		}
	}
	return unconfirmed()
}

// numericField returns a string-typed digits-only member of a JSON object.
func numericField(body []byte, name string) (string, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return "", false
	}
	var s string
	if json.Unmarshal(doc[name], &s) != nil || !numericIDPattern.MatchString(s) {
		return "", false
	}
	return s, true
}
