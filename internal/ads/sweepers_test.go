package ads

import (
	"testing"

	"github.com/riverqueue/river"
)

// sweepers_test.go: UNIT tests of the periodic registration (no database).

func TestPeriodicJobsAndWorkersRegister(t *testing.T) {
	jobs := PeriodicJobs()
	if len(jobs) != 3 {
		t.Fatalf("periodic jobs = %d", len(jobs))
	}
	w := river.NewWorkers()
	if err := AddWorkers(w, nil); err == nil {
		t.Fatal("nil pool accepted")
	}
	// A real pool is not needed to register: the workers only hold it.
	if err := AddWorkers(w, fakePool()); err != nil {
		t.Fatalf("add workers: %v", err)
	}
	if err := AddWorkers(w, fakePool()); err == nil {
		t.Fatal("duplicate registration must fail")
	}
}

func TestSweeperArgsKinds(t *testing.T) {
	for kind, args := range map[string]river.JobArgs{kindAdvance: advanceArgs{}, kindInsights: insightsArgs{}, kindPurge: purgeArgs{}} {
		if args.Kind() != kind {
			t.Fatalf("%s != %s", args.Kind(), kind)
		}
	}
}
