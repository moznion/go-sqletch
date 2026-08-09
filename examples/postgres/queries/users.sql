-- name: SearchUsers :many
SELECT
    u.id,
    u.email,
    u.status,
    u.created_at
FROM users AS u

@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif

WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif

@if-present(created_after)
  AND u.created_at >= :created_after
@endif

@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(created_at_asc)
ORDER BY u.created_at ASC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;

-- name: UpdateUserProfile :one
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

-- name: FilterUsers :many
-- @filter-tree! (v0.3): the caller composes a closed predicate
-- vocabulary with And/Or; nil is rejected, FilterUsersUnscoped() is
-- the explicit opt-out. See docs/spec.md Use Case 5.
SELECT u.id, u.email
FROM users AS u
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
u.tenant_id = :scope_tenant_id
@predicate(status_eq)
u.status = :scope_status
@predicate(email_prefix)
u.email LIKE :scope_prefix || '%'
@end
ORDER BY u.id
LIMIT :limit;

-- name: ListUsersSorted :many
-- @when + @order-by (v0.3): a value-conditioned guard and
-- caller-chosen multi-key sorting over a closed key set.
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE
@when(include_banned = false)
  AND u.status != 'banned'
@end
@order-by(sort)
@key(created_at)
u.created_at
@key(email)
u.email
@default
ORDER BY u.id ASC
@end
LIMIT :limit;

-- name: GetUserProfile :one
SELECT u.id, u.email, u.nickname, u.org_id
FROM users AS u
WHERE u.id = :id

@if-present(status)
  AND u.status = :status
@endif
;

-- name: FindUserByEmail :maybe-one
SELECT u.id, u.email, u.nickname
FROM users AS u
WHERE u.email = :email;
