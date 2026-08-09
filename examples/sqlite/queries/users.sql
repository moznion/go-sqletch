-- name: SearchUsers :many
-- @param status: text
-- @param email_prefix: text
-- @param limit: integer
SELECT u.id, u.email, u.status, u.nickname
FROM users AS u
WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif

@choose(sort)
@case(email_asc)
ORDER BY u.email ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;

-- name: UsersInStatuses :many
-- @param tenant_id: integer
-- @param statuses: text
-- @param limit: integer
SELECT u.id, u.email, u.status
FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
ORDER BY u.id
LIMIT :limit;

-- name: FilterUsers :many
-- @filter-tree! (v0.3): the caller composes a closed predicate
-- vocabulary with And/Or; nil is rejected, FilterUsersUnscoped() is
-- the explicit opt-out. See docs/spec.md Use Case 5.
-- @param scope_tenant_id: integer
-- @param scope_status: text
-- @param scope_prefix: text
-- @param limit: integer
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

-- name: CountByStatus :many
-- @param tenant_id: integer
-- @column n: integer
SELECT u.status, count(*) AS n
FROM users AS u
WHERE u.tenant_id = :tenant_id
GROUP BY u.status
ORDER BY u.status;

-- name: FindUserByEmail :maybe-one
-- @param email: text
SELECT u.id, u.email, u.nickname
FROM users AS u
WHERE u.email = :email;
