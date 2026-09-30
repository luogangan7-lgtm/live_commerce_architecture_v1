package fulfillment

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// newTestJobs builds an insert-only River client over an unconnected pool: constructing it needs no database.
func newTestJobs(t *testing.T) *river.Client[pgx.Tx] {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(&pgxpool.Pool{}), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}
