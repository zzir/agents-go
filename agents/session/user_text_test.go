package session

import "testing"

// inputItems decodes raw JSON items the way a real run's input arrives, so
// role-only messages ({"role":"user",...}) resolve to the EasyInputMessage
// variant.
func inputItems(t *testing.T, raws ...string) []InputItem {
	t.Helper()
	items := make([]InputItem, 0, len(raws))
	for _, r := range raws {
		it, err := UnmarshalInputItem([]byte(r))
		if err != nil {
			t.Fatalf("unmarshal input item %q: %v", r, err)
		}
		items = append(items, it)
	}
	return items
}

func TestUserText(t *testing.T) {
	cases := []struct {
		name string
		raws []string
		want string
	}{
		{
			name: "plain string content",
			raws: []string{`{"role":"user","content":"delete the database"}`},
			want: "delete the database",
		},
		{
			name: "content array parts",
			raws: []string{`{"role":"user","content":[{"type":"input_text","text":"run the deploy"}]}`},
			want: "run the deploy",
		},
		{
			name: "multiple parts concatenate",
			raws: []string{`{"role":"user","content":[{"type":"input_text","text":"run "},{"type":"input_text","text":"the deploy"}]}`},
			want: "run the deploy",
		},
		{
			name: "skips non-user items",
			raws: []string{
				`{"role":"system","content":"be careful"}`,
				`{"role":"user","content":"go"}`,
				`{"role":"assistant","content":"ok"}`,
			},
			want: "go",
		},
		{
			name: "joins user messages with newlines",
			raws: []string{
				`{"role":"user","content":"first"}`,
				`{"role":"user","content":"second"}`,
			},
			want: "first\nsecond",
		},
		{
			name: "whitespace-only user item is dropped",
			raws: []string{
				`{"role":"user","content":"  \n\t"}`,
				`{"role":"user","content":"real"}`,
			},
			want: "real",
		},
		{
			name: "empty when no user item",
			raws: []string{`{"role":"assistant","content":"hi"}`},
			want: "",
		},
		{
			name: "empty slice",
			raws: nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UserText(inputItems(t, tc.raws...)); got != tc.want {
				t.Errorf("UserText = %q, want %q", got, tc.want)
			}
		})
	}
}
