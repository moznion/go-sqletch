package lsp

import "testing"

func TestUriToPath(t *testing.T) {
	tests := []struct {
		uri     string
		want    string
		wantErr bool
	}{
		{"file:///ws/queries/q.sql", "/ws/queries/q.sql", false},
		// A non-clean URI must be cleaned so it matches the glob
		// expansion's cleaned spelling of the same file.
		{"file:///ws/queries/../queries/q.sql", "/ws/queries/q.sql", false},
		{"file:///ws//queries///q.sql", "/ws/queries/q.sql", false},
		{"http://example/q.sql", "", true},
		{"::not-a-uri", "", true},
	}
	for _, tt := range tests {
		got, err := uriToPath(tt.uri)
		if tt.wantErr {
			if err == nil {
				t.Errorf("uriToPath(%q) = %q, want error", tt.uri, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("uriToPath(%q): %v", tt.uri, err)
			continue
		}
		if got != tt.want {
			t.Errorf("uriToPath(%q) = %q, want %q", tt.uri, got, tt.want)
		}
	}
}
