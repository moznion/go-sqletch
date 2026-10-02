package runtime

import (
	"context"
	"errors"
	"fmt"
)

// CtxErr normalizes a database error against the context the call ran
// under (design 23 §5). When the context is done and err does not
// already match its error, the result wraps BOTH — the context error
// first — so errors.Is(err, context.DeadlineExceeded) (or
// context.Canceled) holds alongside errors.Is on the driver's own
// error, which is preserved. Otherwise err is returned unchanged.
//
// Generated code for drivers that report an expired context in their
// own terms (ncruces/go-sqlite3 returns sqlite3.INTERRUPT) funnels every
// driver error through it. It decides from ctx.Err() rather than from
// the driver's error code because neither this package nor generated
// code may import a driver; the cost is that an unrelated error racing
// the deadline is also prefixed with the context error, its original
// still reachable.
func CtxErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	cerr := ctx.Err()
	if cerr == nil || errors.Is(err, cerr) {
		return err
	}
	return fmt.Errorf("%w: %w", cerr, err)
}
