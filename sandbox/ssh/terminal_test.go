package ssh

import "testing"

func TestBuildShellCommand(t *testing.T) {
	tests := []struct {
		name  string
		dir   string
		env   map[string]string
		shell []string
		want  string
	}{
		{
			name: "default shell no dir",
			want: `exec "${SHELL:-/bin/sh}" -l`,
		},
		{
			name: "default shell with dir",
			dir:  "/srv/work",
			want: `cd '/srv/work' && exec "${SHELL:-/bin/sh}" -l`,
		},
		{
			name:  "custom shell",
			dir:   "/srv/work",
			shell: []string{"/bin/bash", "--noprofile"},
			want:  `cd '/srv/work' && exec '/bin/bash' '--noprofile'`,
		},
		{
			name: "env sorted and quoted",
			env:  map[string]string{"B": "2", "A": "a b"},
			want: `exec env 'A=a b' 'B=2' "${SHELL:-/bin/sh}" -l`,
		},
		{
			name: "dir with single quote",
			dir:  "/tmp/it's",
			want: `cd '/tmp/it'\''s' && exec "${SHELL:-/bin/sh}" -l`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildShellCommand(tt.dir, tt.env, tt.shell); got != tt.want {
				t.Errorf("buildShellCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}
