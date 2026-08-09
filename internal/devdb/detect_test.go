package devdb

import (
	"context"
	"errors"
	"testing"
)

func TestConfig_WantVersion(t *testing.T) {
	// The round trip is worth making for either reason, and for
	// neither reason it must be skipped.
	if (Config{}).wantVersion() {
		t.Error("no pin and no sink: the version query must be skipped")
	}
	if !(Config{ServerVersion: "16"}).wantVersion() {
		t.Error("a pin requires the version query")
	}
	if !(Config{Detected: &Detected{}}).wantVersion() {
		t.Error("a Detected sink requires the version query")
	}
}

func TestConfig_RecordVersion(t *testing.T) {
	// The sink receives the RAW reported string: reducing it is the
	// caller's business (cache.NumericVersionPrefix), because the raw
	// form is what the drift diagnostic must be able to quote.
	var det Detected
	cfg := Config{ServerVersion: "16", Detected: &det}
	if err := cfg.recordVersion("16.4 (Debian 16.4-1.pgdg120+1)", "PostgreSQL"); err != nil {
		t.Fatal(err)
	}
	if det.ServerVersion != "16.4 (Debian 16.4-1.pgdg120+1)" {
		t.Errorf("sink got %q, want the raw reported string", det.ServerVersion)
	}
}

func TestConfig_RecordVersion_FillsSinkWithoutAPin(t *testing.T) {
	var det Detected
	if err := (Config{Detected: &det}).recordVersion("8.0.36-log", "MySQL"); err != nil {
		t.Fatal(err)
	}
	if det.ServerVersion != "8.0.36-log" {
		t.Errorf("sink got %q; an absent pin must not suppress detection", det.ServerVersion)
	}
}

func TestConfig_RecordVersion_PinStillEnforced(t *testing.T) {
	// Detection must not weaken the existing pin check (SQLETCH200).
	var det Detected
	err := (Config{ServerVersion: "16", Detected: &det}).recordVersion("15.2", "PostgreSQL")
	var vme *VersionMismatchError
	if !errors.As(err, &vme) {
		t.Fatalf("pin mismatch must still fail, got %v", err)
	}
	if vme.Actual != "15.2" || vme.Pinned != "16" || vme.Server != "PostgreSQL" {
		t.Errorf("unexpected mismatch error: %+v", vme)
	}
}

// AcquireSQLite is fully in-process, so the whole detection path —
// connect, ask the engine, fill the sink — is exercisable without a
// container or a devdb build tag.
func TestAcquireSQLite_FillsDetected(t *testing.T) {
	var det Detected
	conn, cleanup, err := AcquireSQLite(context.Background(), Config{Detected: &det})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	_ = conn
	if det.ServerVersion == "" {
		t.Fatal("Detected.ServerVersion must be filled in by a real acquire")
	}
	if got := det.ServerVersion; got[0] != '3' {
		t.Errorf("SQLite reported %q, want a 3.x version", got)
	}
}

func TestAcquireSQLite_NilDetectedIsFine(t *testing.T) {
	conn, cleanup, err := AcquireSQLite(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	_ = conn
}
