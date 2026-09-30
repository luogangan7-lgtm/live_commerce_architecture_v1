package foundation_test

// S6 (r1-final-rulings; manual-fulfilment-v1 §3.2): REAL_PG. The tracking URL host grammar is one rule in
// three places: Go canonicalTrackingURL, the tracking_url column CHECK and record_manual_shipment. The
// CHECK's negative rows live in TestManualFulfilmentMF02Schema; this guards the twins against drift.

import "testing"

const mfsHostRegex = `^https://([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]*[a-zA-Z0-9])?([/?][!-~]*)?$`

func TestMFS6TrackingHostTwins(t *testing.T) {
	e := rfxNew(t)
	srsMust(t, e, "record_manual_shipment carries the LDH host rule",
		`SELECT position($1 in pg_get_functiondef('fulfillment.record_manual_shipment(bytea,uuid,uuid,text,bytea,bigint,text,text,text,text,text,text,text)'::regprocedure))>0`, mfsHostRegex)
	srsMust(t, e, "tracking_url column CHECK carries the LDH host rule",
		`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='fulfillment.manual_shipment_versions'::regclass
		  AND contype='c' AND position($1 in pg_get_constraintdef(oid))>0)`, mfsHostRegex)
}
