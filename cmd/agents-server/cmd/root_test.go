package cmd

import "testing"

// An unnamed local zone prints its abbreviation once: when the abbreviation
// is itself the offset ("+08"), only the UTC offset is shown.
func TestFormatLocalZone(t *testing.T) {
	cases := []struct {
		abbr string
		off  int
		want string
	}{
		{"CST", 28800, "Local (CST UTC+08:00)"},
		{"+08", 28800, "Local (UTC+08:00)"},
		{"-0330", -12600, "Local (UTC-03:30)"},
		{"", 0, "Local (UTC+00:00)"},
	}
	for _, c := range cases {
		if got := formatLocalZone(c.abbr, c.off); got != c.want {
			t.Errorf("formatLocalZone(%q, %d) = %q, want %q", c.abbr, c.off, got, c.want)
		}
	}
}
