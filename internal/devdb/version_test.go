package devdb

import "testing"

// The error is shared by every dialect's Acquire; it must name the
// server it actually connected to (it used to say "PostgreSQL" for
// MySQL and SQLite too).
func TestVersionMismatchError_NamesTheServer(t *testing.T) {
	for _, tc := range []struct{ server, want string }{
		{"PostgreSQL", "connected server is PostgreSQL 15.2 but sqletch.yaml pins server_version 16"},
		{"MySQL", "connected server is MySQL 15.2 but sqletch.yaml pins server_version 16"},
		{"SQLite", "connected server is SQLite 15.2 but sqletch.yaml pins server_version 16"},
	} {
		err := &VersionMismatchError{Pinned: "16", Actual: "15.2", Server: tc.server}
		if got := err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// Defensive: an unset Server must not produce "connected server is  x".
func TestVersionMismatchError_UnnamedServer(t *testing.T) {
	err := &VersionMismatchError{Pinned: "16", Actual: "15.2"}
	want := "connected server is 15.2 but sqletch.yaml pins server_version 16"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
