package agents

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// nonZeroFor returns a value of type t that differs from t's zero value, for
// every type SessionEntry's fields use.
func nonZeroFor(t reflect.Type) (reflect.Value, bool) {
	switch t.Kind() {
	case reflect.String:
		return reflect.ValueOf("x").Convert(t), true
	case reflect.Int, reflect.Int64:
		return reflect.ValueOf(int64(7)).Convert(t), true
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(t), true
	case reflect.Slice:
		if t == reflect.TypeOf(json.RawMessage(nil)) {
			return reflect.ValueOf(json.RawMessage(`{"a":1}`)), true
		}
		v := reflect.MakeSlice(t, 1, 1)
		if elem, ok := nonZeroFor(t.Elem()); ok {
			v.Index(0).Set(elem)
		}
		return v, true
	case reflect.Map:
		v := reflect.MakeMap(t)
		key, kok := nonZeroFor(t.Key())
		val, vok := nonZeroFor(t.Elem())
		if !kok || !vok {
			return reflect.Value{}, false
		}
		v.SetMapIndex(key, val)
		return v, true
	case reflect.Pointer:
		v := reflect.New(t.Elem())
		if inner, ok := nonZeroFor(t.Elem()); ok {
			v.Elem().Set(inner)
		}
		return v, true
	case reflect.Interface:
		return reflect.ValueOf(any("x")), true
	case reflect.Struct:
		if t == reflect.TypeOf(time.Time{}) {
			return reflect.ValueOf(time.Unix(1, 0).UTC()), true
		}
		v := reflect.New(t).Elem()
		set := false
		for i := range t.NumField() {
			if !v.Field(i).CanSet() {
				continue
			}
			if inner, ok := nonZeroFor(t.Field(i).Type); ok {
				v.Field(i).Set(inner)
				set = true
			}
		}
		return v, set
	}
	return reflect.Value{}, false
}

// Every field of SessionEntry must take part in Equal. This walks the struct
// rather than listing the fields, so a field added later fails here instead of
// silently making two different entries compare equal — which is how a
// compaction pass gets discarded as a no-op or an index resumes onto a history
// that is not its own.
func TestEqualCoversEverySessionEntryField(t *testing.T) {
	typ := reflect.TypeOf(SessionEntry{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			var base SessionEntry
			mutated := base
			val, ok := nonZeroFor(field.Type)
			if !ok {
				t.Fatalf("no non-zero value known for %s (%s); teach nonZeroFor about it", field.Name, field.Type)
			}
			reflect.ValueOf(&mutated).Elem().Field(i).Set(val)
			if base.Equal(mutated) {
				t.Fatalf("Equal ignores %s: a change to it is invisible", field.Name)
			}
			same := base
			reflect.ValueOf(&same).Elem().Field(i).Set(val)
			if !mutated.Equal(same) {
				t.Fatalf("Equal is not reflexive for %s", field.Name)
			}
		})
	}
}

// An entry that has round-tripped through storage loses its monotonic clock
// reading. It is still the same entry, and Equal must say so — this is why
// CreatedAt is compared with time.Time.Equal and not with ==.
func TestEqualIgnoresMonotonicClock(t *testing.T) {
	now := time.Now()
	a := SessionEntry{ID: "e1", CreatedAt: now}
	b := SessionEntry{ID: "e1", CreatedAt: now.Round(0)} // strips the monotonic reading
	if !a.Equal(b) {
		t.Fatal("the same instant compared unequal because of the monotonic clock")
	}
}
