package sqlite

import (
	"fmt"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// The SQLite engine runs IN-PROCESS over the host filesystem
// (ncruces/go-sqlite3's OS VFS), unlike PostgreSQL/MySQL whose
// statements run inside a throwaway container. Schema files and
// templates are repo-controlled, so any statement that names a database
// file — ATTACH, VACUUM INTO — would create or overwrite files wherever
// the repo points it, bypassing the SQLETCH306 path-escape policy the
// DSN itself is held to (security audit F15, CWE-22). Restrict closes
// that surface on a connection; it is the one seam devdb's schema
// application and the oracle share.
//
// The rest of the file-touching surface is closed by the build itself
// (ncruces/go-sqlite3-wasm compiles SQLITE_OMIT_LOAD_EXTENSION and
// links no fileio extension, so load_extension/readfile/writefile do
// not exist) — TestRestrict_NoFileFunctions pins their absence.

// FileAccessError reports a statement refused because it names a
// database file or redirects where the engine writes files.
type FileAccessError struct {
	// Action is the refused operation: "ATTACH", "DETACH", "VACUUM"
	// (VACUUM attaches its output database internally), or "PRAGMA
	// <name>" for the directory-redirecting pragmas.
	Action string
	// Target is the file or directory the statement named, when SQLite
	// reported it as a literal ("" for a computed filename).
	Target string
	// Pos is the byte offset of the refused statement in the script
	// passed to Guard.Exec (-1 when unknown).
	Pos int
}

func (e *FileAccessError) Error() string {
	target := ""
	if e.Target != "" {
		target = fmt.Sprintf(" (%q)", e.Target)
	}
	return fmt.Sprintf("%s%s refused: the SQLite engine runs in-process with filesystem access, so a statement naming a database file could read or write outside the project", e.Action, target)
}

// Guard is a connection restricted by Restrict. It records the most
// recent refusal so callers can tell a refusal from an ordinary SQL
// error (both surface from SQLite as a failed statement).
type Guard struct {
	conn     *sqlite3.Conn
	stepping bool
	refused  *FileAccessError
}

// Restrict forbids every statement on conn that names a database file:
// an authorizer denies ATTACH/DETACH (VACUUM and VACUUM INTO attach
// their target internally, so they are denied too) and the
// directory-redirecting pragmas, and SQLITE_LIMIT_ATTACHED is set to 0
// so an ATTACH still fails if the authorizer is ever replaced.
func Restrict(conn *sqlite3.Conn) (*Guard, error) {
	g := &Guard{conn: conn}
	conn.Limit(sqlite3.LIMIT_ATTACHED, 0)
	if err := conn.SetAuthorizer(g.authorize); err != nil {
		return nil, fmt.Errorf("install SQLite authorizer: %w", err)
	}
	return g, nil
}

func (g *Guard) authorize(action sqlite3.AuthorizerActionCode, name3rd, name4th, _, _ string) sqlite3.AuthorizerReturnCode {
	switch action {
	case sqlite3.AUTH_ATTACH:
		// An ATTACH statement is authorized while it is PREPARED; an
		// ATTACH that arrives while a statement is being STEPPED is
		// VACUUM's internal attach of its output database.
		act := "ATTACH"
		if g.stepping {
			act = "VACUUM"
		}
		g.refused = &FileAccessError{Action: act, Target: name3rd, Pos: -1}
		return sqlite3.AUTH_DENY
	case sqlite3.AUTH_DETACH:
		g.refused = &FileAccessError{Action: "DETACH", Pos: -1}
		return sqlite3.AUTH_DENY
	case sqlite3.AUTH_PRAGMA:
		switch name3rd {
		case "temp_store_directory", "data_store_directory":
			g.refused = &FileAccessError{Action: "PRAGMA " + name3rd, Target: name4th, Pos: -1}
			return sqlite3.AUTH_DENY
		}
	}
	return sqlite3.AUTH_OK
}

// reset forgets any refusal recorded by an earlier statement.
func (g *Guard) reset() { g.refused = nil }

// take returns and clears the recorded refusal.
func (g *Guard) take() *FileAccessError {
	r := g.refused
	g.refused = nil
	return r
}

// Exec runs a multi-statement script like sqlite3_exec, one statement
// at a time so a refusal can be attributed to its statement. A refusal
// returns *FileAccessError with Pos at the refused statement's first
// token; any other failure returns SQLite's error unchanged.
func (g *Guard) Exec(script string) error {
	g.reset()
	rest := script
	for {
		start := skipTrivia(script, len(script)-len(rest))
		stmt, tail, err := g.conn.Prepare(rest)
		if err != nil {
			return g.refusalOr(err, start)
		}
		if stmt == nil {
			if tail == rest || tail == "" {
				return nil
			}
			rest = tail
			continue
		}
		g.stepping = true
		for stmt.Step() {
			// Result rows (a PRAGMA's, a seed SELECT's) are discarded,
			// as sqlite3_exec does.
		}
		g.stepping = false
		err = stmt.Err()
		_ = stmt.Close()
		if err != nil {
			return g.refusalOr(err, start)
		}
		if tail == "" {
			return nil
		}
		rest = tail
	}
}

func (g *Guard) refusalOr(err error, pos int) error {
	if r := g.take(); r != nil {
		r.Pos = pos
		return r
	}
	return err
}

// skipTrivia advances pos past whitespace, comments, and semicolons.
func skipTrivia(src string, pos int) int {
	prof, b := Profile{}, []byte(src)
	for pos < len(b) {
		tok, err := prof.NextToken(b, pos)
		if err != nil {
			return pos
		}
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment,
			dialect.KindBlockComment, dialect.KindSemicolon:
			pos = tok.End
			continue
		}
		return pos
	}
	return pos
}
