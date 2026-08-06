package session

import (
	"fmt"
	"reflect"
	"testing"
)

// Every exported ItemDisplay field must participate in merge: an update entry
// carrying only that field has to land on the folded display. Written by
// reflection so the NEXT field added to ItemDisplay fails here until its merge
// branch exists — a field merge silently drops is a display update that never
// arrives, and nothing else would notice.
func TestItemDisplayMergeCoversEveryField(t *testing.T) {
	typ := reflect.TypeFor[ItemDisplay]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			var other ItemDisplay
			fv := reflect.ValueOf(&other).Elem().Field(i)
			switch f.Type.Kind() {
			case reflect.String:
				fv.SetString("sample")
			case reflect.Bool:
				fv.SetBool(true)
			case reflect.Map:
				fv.Set(reflect.MakeMap(f.Type))
				fv.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf(any("v")))
			default:
				t.Fatalf("ItemDisplay gained a %s field; teach this test to build a sample for it", f.Type.Kind())
			}

			base := ItemDisplay{}
			base.merge(other)
			got := reflect.ValueOf(base).Field(i).Interface()
			want := fv.Interface()
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("merge dropped %s: got %v, want %v — add its branch to (*ItemDisplay).merge", f.Name, got, want)
			}
		})
	}
}
