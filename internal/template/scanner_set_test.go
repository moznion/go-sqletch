package template

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
)

// useCase2 is the spec's partial-UPDATE example (Use Case 2).
const useCase2 = `-- name: UpdateUserProfile :one
UPDATE users
SET
    updated_at = now()
@if-present(email)
  , email = :email
@endif
@if-present(nickname)
  , nickname = :nickname
@endif
WHERE id = :id
RETURNING id, email, nickname, updated_at;
`

func TestScan_UseCase2_SetItems(t *testing.T) {
	f := scanClean(t, useCase2)
	q := f.Queries[0]
	if q.Annotation != AnnotationOne {
		t.Fatalf("annotation = %v", q.Annotation)
	}
	var sets []*IfPresent
	for _, it := range q.Items {
		if ip, ok := it.(*IfPresent); ok {
			sets = append(sets, ip)
		}
	}
	if len(sets) != 2 {
		t.Fatalf("set items = %d, want 2", len(sets))
	}
	for i, want := range []string{"email = :email", "nickname = :nickname"} {
		if sets[i].Slot != SlotSetItem || sets[i].Sep != SepComma || sets[i].Body != want {
			t.Errorf("set[%d] = slot %v sep %v body %q, want SlotSetItem/SepComma/%q",
				i, sets[i].Slot, sets[i].Sep, sets[i].Body, want)
		}
	}
	// The WHERE conjunct param and RETURNING are skeleton.
	if q.Params["id"].Occurrences[0].Guards != nil {
		t.Error("id must be unguarded")
	}
	// Guard bits assigned in order.
	if len(q.GuardAtoms) != 2 || q.GuardAtoms[0].Param != "email" || q.GuardAtoms[1].Param != "nickname" {
		t.Errorf("guard atoms = %+v", q.GuardAtoms)
	}
}

func TestScan_SetItemNeedsComma(t *testing.T) {
	src := `-- name: Bad :exec
UPDATE users
SET updated_at = now()
@if-present(email)
  email = :email
@endif
WHERE id = :id;
`
	_, diags := scan(t, src)
	found := false
	for _, d := range diags {
		if d.Code == diagnostics.CodeConjunctNeedsAnd && strings.Contains(d.Message, "SET item") {
			found = true
		}
	}
	if !found {
		t.Errorf("want separator diagnostic for SET item, got %+v", diags)
	}
}

// UPDATE with both optional SET items and optional WHERE conjuncts:
// contexts must not bleed into each other.
func TestScan_UpdateWithGuardedWhere(t *testing.T) {
	src := `-- name: Q :exec
UPDATE users
SET
    updated_at = now()
@if-present(email)
  , email = :email
@endif
WHERE TRUE
@if-present(tenant)
  AND users.tenant_id = :tenant
@endif
;
`
	f := scanClean(t, src)
	q := f.Queries[0]
	var slots []Slot
	for _, it := range q.Items {
		if ip, ok := it.(*IfPresent); ok {
			slots = append(slots, ip.Slot)
		}
	}
	if len(slots) != 2 || slots[0] != SlotSetItem || slots[1] != SlotWhereConjunct {
		t.Fatalf("slots = %v, want [SlotSetItem SlotWhereConjunct]", slots)
	}
}
