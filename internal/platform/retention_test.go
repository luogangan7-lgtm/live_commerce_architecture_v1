package platform

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// Unit tests of the retention pool openers (no database): input bounds and the rule that a
// failure never echoes the DSN. Admission against real logins is CRP02 (independent gate).

const retentionDSNSentinel = "retention-dsn-sentinel-2c81"

func retentionDSN(host string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword("lc_retention", retentionDSNSentinel), Host: host, Path: "/db"}
	return u.String()
}

func TestRetentionOpenersFailClosedWithoutLeakingTheDSN(t *testing.T) {
	ctx := context.Background()
	open := map[string]func(context.Context, string) error{
		"job":      func(c context.Context, d string) error { _, err := OpenRetentionJobPool(c, d); return err },
		"operator": func(c context.Context, d string) error { _, err := OpenRetentionOperatorPool(c, d); return err },
	}
	for name, f := range open {
		for label, dsn := range map[string]string{
			"empty":     "",
			"oversize":  retentionDSN(strings.Repeat("h", 9000)),
			"garbage":   "%%" + retentionDSNSentinel,
			"no server": retentionDSN("127.0.0.1:1"), // refused at once; the driver error would echo the DSN
		} {
			err := f(ctx, dsn)
			if err == nil {
				t.Fatalf("%s/%s: opened a pool", name, label)
			}
			if strings.Contains(err.Error(), retentionDSNSentinel) || strings.Contains(err.Error(), "lc_retention") {
				t.Fatalf("%s/%s: error echoes the DSN: %v", name, label, err)
			}
		}
		if err := f(nil, retentionDSN("h")); err == nil { //nolint:staticcheck // a nil context must be refused, not panic
			t.Fatalf("%s: nil context accepted", name)
		}
	}
	if err := ValidateRetentionJobPool(ctx, nil); err == nil {
		t.Fatal("nil job pool accepted")
	}
	if err := ValidateRetentionOperatorPool(ctx, nil); err == nil {
		t.Fatal("nil operator pool accepted")
	}
	if err := ValidateRetentionJobPool(nil, nil); err == nil { //nolint:staticcheck
		t.Fatal("nil context accepted")
	}
}
