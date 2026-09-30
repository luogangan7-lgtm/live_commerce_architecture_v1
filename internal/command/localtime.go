package command

import (
	"reflect"
	"time"
)

// timeType is compared by identity while walking decoded values.
var timeType = reflect.TypeFor[time.Time]()

// InLocalTime rewrites every time.Time reachable from v (a non-nil pointer) to
// time.Local, in place, without changing the instant it denotes.
//
// Why: pgx (github.com/jackc/pgx/v5) scans timestamptz into time.Local, while
// encoding/json decodes RFC 3339 "Z" timestamps into time.UTC. On a UTC host
// (every Linux server and CI runner) the same instant then carries two
// different *time.Location values, so a result decoded from a saved replay
// receipt or a SQL-built JSON snapshot is not == / reflect.DeepEqual to the
// value first returned from a row scan. Callers that decode domain results
// from JSON (command.Run, buyer.RunCommand, checkout receipts/snapshots) call
// this so replayed and freshly scanned results are identical on any host TZ.
//
// Only exported, settable fields are visited; unexported fields, maps and
// interface values are left untouched (domain result types do not use them
// for timestamps). It never allocates new pointers or changes nil-ness, and
// the zero time is preserved.
func InLocalTime(v any) {
	if v == nil {
		return
	}
	value := reflect.ValueOf(v)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return
	}
	localizeValue(value.Elem())
}

func localizeValue(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			localizeValue(value.Elem())
		}
	case reflect.Struct:
		if value.Type() == timeType {
			// The zero time stays the zero value so IsZero/== checks keep working.
			if t := value.Interface().(time.Time); value.CanSet() && !t.IsZero() {
				value.Set(reflect.ValueOf(t.In(time.Local)))
			}
			return
		}
		for i := range value.NumField() {
			if field := value.Field(i); field.CanSet() {
				localizeValue(field)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			localizeValue(value.Index(i))
		}
	}
}
