package devdb

import "testing"

// The version pin is a DOTTED PREFIX for every engine: `"16"` accepts
// any 16.x, `"16.4"` accepts only 16.4.x. PostgreSQL and MySQL used to
// compare majors only, which silently discarded whatever the user
// wrote after the first dot — `server_version: "8.4"` accepted a
// MySQL 8.0 server, and pinning a minor was indistinguishable from
// pinning the major it starts with.
func TestVersionPinMatch(t *testing.T) {
	cases := []struct {
		pinned, actual string
		want           bool
		why            string
	}{
		// Majors keep working exactly as before.
		{"16", "16.4", true, "major pin accepts its minors"},
		{"16", "16.9", true, "major pin accepts any minor"},
		{"16", "17.2", false, "major pin rejects another major"},
		{"8", "8.0.36-log", true, "major pin, MySQL build suffix"},

		// Build noise must not defeat the pin: the reported string is
		// reduced to its numeric run before comparing.
		{"16", "16.4 (Debian 16.4-1.pgdg120+1)", true, "PG Debian build"},
		{"16.4", "16.4 (Debian 16.4-1.pgdg120+1)", true, "minor pin, Debian build"},
		{"8.0.36", "8.0.36-log", true, "MySQL -log suffix"},

		// What the change is for.
		{"16.4", "16.9", false, "minor pin rejects another minor"},
		{"8.4", "8.0.36-log", false, "MySQL 8.4 pin must reject 8.0"},
		{"8.0", "8.4.0", false, "MySQL 8.0 pin must reject 8.4"},
		{"8.0.19", "8.0.18", false, "patch pin rejects an older patch"},

		// The prefix is dot-bounded, never a raw string prefix.
		{"1", "16.4", false, "1 must not match 16.x"},
		{"16", "160.1", false, "16 must not match 160.x"},
		{"8.0", "8.01.3", false, "8.0 must not match 8.01.x"},

		// SQLite's existing semantics are unchanged.
		{"3", "3.53.3", true, "SQLite major"},
		{"3.50", "3.50.4", true, "SQLite minor"},
		{"3.50", "3.53.3", false, "SQLite minor rejects another minor"},
		{"3.50", "3.5.4", false, "3.50 is not a prefix of 3.5"},

		// Exact equality at every depth.
		{"16.4", "16.4", true, "identical"},
		{"3.53.3", "3.53.3", true, "identical, three components"},

		// Degenerate reports: refuse rather than accept blindly.
		{"16", "", false, "a server that reports no version cannot satisfy a pin"},
		{"16", "unknown", false, "a non-numeric report cannot satisfy a pin"},
	}
	for _, c := range cases {
		if got := versionPinMatch(c.pinned, c.actual); got != c.want {
			t.Errorf("versionPinMatch(%q, %q) = %v, want %v (%s)",
				c.pinned, c.actual, got, c.want, c.why)
		}
	}
}

// A pin the user spelled with build noise still means the version it
// starts with — the comparison normalizes both sides, so the hint
// SQLETCH200 prints can be pasted back verbatim.
func TestVersionPinMatch_NormalizesThePinToo(t *testing.T) {
	if !versionPinMatch("16.4 (Debian 16.4-1.pgdg120+1)", "16.4") {
		t.Error("a pin carrying build noise must still match its version")
	}
}

func TestRecordVersion_EveryEngineUsesTheSameRule(t *testing.T) {
	// One rule, no per-dialect flag: the engine name only spells the
	// diagnostic.
	for _, server := range []string{"PostgreSQL", "MySQL", "SQLite"} {
		if err := (Config{ServerVersion: "8.4"}).recordVersion("8.0.36-log", server); err == nil {
			t.Errorf("%s: a minor pin must be enforced", server)
		}
		if err := (Config{ServerVersion: "8"}).recordVersion("8.0.36-log", server); err != nil {
			t.Errorf("%s: a major pin must still accept its minors: %v", server, err)
		}
	}
}
