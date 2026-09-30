package main

import (
	"context"
	"strings"
	"testing"
)

// Config errors must exit 2 before any connection attempt and never echo the DSN.
func TestMigrateRejectsInvalidConfig(t *testing.T) {
	for name, dsn := range map[string]string{
		"empty":     "",
		"blank":     "   ",
		"oversized": "postgres://x/" + strings.Repeat("a", 9000),
		"malformed": "postgres://%zz",
	} {
		getenv := func(key string) string {
			if key != "COMMERCE_MIGRATE_DATABASE_URL" {
				t.Fatalf("%s: unexpected env read %q", name, key)
			}
			return dsn
		}
		if code := run(context.Background(), getenv); code != 2 {
			t.Fatalf("%s: exit %d, want 2", name, code)
		}
	}
}
