package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// errInterrupt stands in for a driver's own "interrupted" error (e.g.
// ncruces/go-sqlite3's sqlite3.INTERRUPT), which does not wrap the
// context's error.
var errInterrupt = errors.New("driver: interrupted")

func TestCtxErr_NilErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := CtxErr(ctx, nil); got != nil {
		t.Errorf("CtxErr(done ctx, nil) = %v, want nil", got)
	}
}

// A live context means the failure is the driver's own: returned
// untouched (identity, not just errors.Is) — including sql.ErrNoRows-
// style sentinels the generated :maybe-one path compares against.
func TestCtxErr_LiveCtxUnchanged(t *testing.T) {
	if got := CtxErr(context.Background(), errInterrupt); got != errInterrupt {
		t.Errorf("live ctx: got %v, want the original error itself", got)
	}
}

func TestCtxErr_DeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	got := CtxErr(ctx, errInterrupt)
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("%v: not DeadlineExceeded", got)
	}
	if !errors.Is(got, errInterrupt) {
		t.Errorf("%v: original driver error lost", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Errorf("%v: deadline must not read as Canceled", got)
	}
	if want := "context deadline exceeded: driver: interrupted"; got.Error() != want {
		t.Errorf("message = %q, want %q", got.Error(), want)
	}
}

func TestCtxErr_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := CtxErr(ctx, errInterrupt)
	if !errors.Is(got, context.Canceled) || !errors.Is(got, errInterrupt) {
		t.Errorf("%v: want both Canceled and the driver error", got)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("%v: cancel must not read as DeadlineExceeded", got)
	}
}

// A driver that already reports the context error (pgx, go-sql-driver)
// is never double-wrapped: the error comes back as is.
func TestCtxErr_AlreadyWrappedUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wrapped := fmt.Errorf("driver: %w", context.Canceled)
	if got := CtxErr(ctx, wrapped); got != wrapped {
		t.Errorf("got %v, want the already-wrapped error itself", got)
	}
	if got := CtxErr(ctx, context.Canceled); got != context.Canceled {
		t.Errorf("got %v, want the bare context error itself", got)
	}
}

// Idempotent: normalizing a normalized error changes nothing.
func TestCtxErr_Idempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	once := CtxErr(ctx, errInterrupt)
	if twice := CtxErr(ctx, once); twice != once {
		t.Errorf("second normalization changed the error: %v -> %v", once, twice)
	}
}
