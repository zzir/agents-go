package cmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

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

// An explicit --db wins, AGENTS_DB backs it, and the default is last: the flag's
// own default must not shadow the variable.
func TestDBTargetPrecedence(t *testing.T) {
	env := func(v string) func(string) string {
		return func(key string) string {
			if key == "AGENTS_DB" {
				return v
			}
			return ""
		}
	}
	cases := []struct {
		name string
		flag string
		set  bool
		env  string
		want string
	}{
		{"explicit flag over the variable", "mine.db", true, "postgres://env", "mine.db"},
		{"variable over the default", "data.db", false, "postgres://env", "postgres://env"},
		{"default when neither is given", "data.db", false, "", "data.db"},
	}
	for _, c := range cases {
		if got := dbTarget(c.flag, c.set, env(c.env)); got != c.want {
			t.Errorf("%s: dbTarget = %q, want %q", c.name, got, c.want)
		}
	}
}

// A start whose address is taken must fail before it opens the database: the
// instance holding the address is live, and the orphan sweep would fail its tasks.
func TestRunRefusesATakenAddressBeforeOpeningTheDatabase(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dbPath := filepath.Join(t.TempDir(), "data.db")
	t.Setenv("AGENTS_DB", dbPath)
	oldHost, oldPort := flagHost, flagPort
	flagHost, flagPort = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	defer func() { flagHost, flagPort = oldHost, oldPort }()

	if err := run(rootCmd, nil); err == nil {
		t.Fatal("run succeeded on a taken address")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("the database was opened before the address was bound (stat: %v)", err)
	}
}
