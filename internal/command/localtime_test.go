package command

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

type localTimeInner struct {
	At time.Time
}

type localTimeResult struct {
	Created  time.Time
	Optional *time.Time
	Missing  *time.Time
	Zero     time.Time
	Lines    []localTimeInner
	Fixed    [1]localTimeInner
	hidden   time.Time
}

// A JSON round trip (what replay receipts do) must come back equal to a value
// whose timestamps are in time.Local, the location pgx scans timestamptz into.
func TestInLocalTimeMakesJSONReplayEqualToScannedValue(t *testing.T) {
	instant := time.Date(2026, 9, 28, 9, 55, 11, 476122000, time.UTC)
	optional := instant.Add(time.Hour)
	scanned := localTimeResult{
		Created:  instant.In(time.Local),
		Optional: ptr(optional.In(time.Local)),
		Lines:    []localTimeInner{{At: instant.In(time.Local)}},
		Fixed:    [1]localTimeInner{{At: instant.In(time.Local)}},
	}
	raw, err := json.Marshal(scanned)
	if err != nil {
		t.Fatal(err)
	}
	var replay localTimeResult
	if err = json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	InLocalTime(&replay)
	if !reflect.DeepEqual(scanned, replay) {
		t.Fatalf("replay differs from scanned value:\n%#v\n%#v", scanned, replay)
	}
	if replay.Created.Location() != time.Local || replay.Optional.Location() != time.Local || replay.Lines[0].At.Location() != time.Local {
		t.Fatal("timestamps were not moved to time.Local")
	}
	if !replay.Zero.IsZero() || replay.Zero != (time.Time{}) || replay.Missing != nil {
		t.Fatal("zero time or nil pointer changed")
	}
}

func TestInLocalTimeIgnoresInvalidTargets(t *testing.T) {
	InLocalTime(nil)
	InLocalTime(localTimeResult{})
	var missing *localTimeResult
	InLocalTime(missing)
	value := localTimeResult{hidden: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	InLocalTime(&value)
	if value.hidden.Location() != time.UTC {
		t.Fatal("unexported field must not be rewritten")
	}
}

func ptr[T any](v T) *T { return &v }
