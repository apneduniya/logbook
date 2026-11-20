package projectname

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"logbook", "logbook"},
		{"", ""},
		{"  logbook \t", "logbook"},
		{"log\x00book", "logbook"},
		{"log\nbook", "logbook"},
		{"<<LAST_PROJECT>>", ""},
		{" <<LAST_PROJECT>> ", ""},
		{"<<LAST_PROJECT>>x", "<<LAST_PROJECT>>x"},
		{"my project", "my project"},
		{"проект", "проект"},
	}
	for _, tc := range tests {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
