package diagnostics

import (
	"os"
	"regexp"
	"testing"
)

// TestManualCoversAllCodes pins the diagnostics reference to the code:
// every SQLETCHnnn constant must appear in docs/manual/08-diagnostics.md,
// and the manual must not document codes that no longer exist.
func TestManualCoversAllCodes(t *testing.T) {
	src, err := os.ReadFile("diagnostics.go")
	if err != nil {
		t.Fatal(err)
	}
	manual, err := os.ReadFile("../../docs/manual/08-diagnostics.md")
	if err != nil {
		t.Fatal(err)
	}

	constRe := regexp.MustCompile(`Code = "(SQLETCH[0-9]{3})"`)
	defined := map[string]bool{}
	for _, m := range constRe.FindAllSubmatch(src, -1) {
		defined[string(m[1])] = true
	}
	if len(defined) < 30 {
		t.Fatalf("suspiciously few code constants found: %d", len(defined))
	}

	rowRe := regexp.MustCompile(`\| (SQLETCH[0-9]{3}) \|`)
	documented := map[string]bool{}
	for _, m := range rowRe.FindAllSubmatch(manual, -1) {
		documented[string(m[1])] = true
	}

	for code := range defined {
		if !documented[code] {
			t.Errorf("%s is not documented in docs/manual/08-diagnostics.md", code)
		}
	}
	for code := range documented {
		if !defined[code] {
			t.Errorf("%s is documented but no longer defined", code)
		}
	}
}
