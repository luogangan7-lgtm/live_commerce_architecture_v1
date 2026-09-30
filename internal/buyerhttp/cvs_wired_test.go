package buyerhttp

import (
	"net/http"
	"testing"
)

// The integrator hook (handler.go): the four CVS routes are reachable through the shared route table and method gate.
func TestCVSRoutesWiredIntoTheSharedRouter(t *testing.T) {
	for _, tc := range []struct {
		path, method string
		kind         routeKind
		ok           bool
	}{
		{"/v1/buyer/cvs-selections", http.MethodPost, routeCVSSelectionOpen, true},
		{"/v1/buyer/cvs-selections", http.MethodGet, routeCVSSelectionOpen, false},
		{"/v1/buyer/cvs-selections/" + cvsID, http.MethodGet, routeCVSSelectionGet, true},
		{"/v1/buyer/cvs-selections/" + cvsID, http.MethodPost, routeCVSSelectionGet, false},
		{"/v1/buyer/cvs-selections/" + cvsID + "/verify", http.MethodPost, routeCVSSelectionVerify, true},
		{"/v1/buyer/cvs-selections/" + cvsID + "/verify", http.MethodGet, routeCVSSelectionVerify, false},
		{"/v1/buyer/cvs-stores", http.MethodPost, routeCVSStoreEnter, true},
		{"/v1/buyer/cvs-stores", http.MethodPut, routeCVSStoreEnter, false},
		{"/v1/buyer/cvs-stores/", http.MethodPost, unknownRoute, false},
	} {
		found := matchRoute(tc.path)
		if found.kind != tc.kind || allowed(found.kind, tc.method) != tc.ok {
			t.Errorf("%s %s: kind=%d allowed=%v", tc.method, tc.path, found.kind, allowed(found.kind, tc.method))
		}
	}
}
