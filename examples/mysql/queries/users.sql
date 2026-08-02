-- name: SearchUsers :many
-- @param status: varchar(32)
-- @param email_prefix: varchar(255)
-- @param limit: bigint
SELECT u.id, u.email, u.status, u.nickname
FROM users AS u
WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE concat(:email_prefix, '%')
@endif

@choose(sort)
@case(email_asc)
ORDER BY u.email ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;

-- name: UsersInStatuses :many
-- @param tenant_id: bigint
-- @param statuses: varchar(32)
-- @param limit: bigint
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
-- @param scope_tenant_id: bigint
-- @param scope_status: varchar(32)
-- @param scope_prefix: varchar(255)
-- @param limit: bigint
SELECT u.id, u.email
FROM users AS u
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
u.tenant_id = :scope_tenant_id
@predicate(status_eq)
u.status = :scope_status
@predicate(email_prefix)
u.email LIKE concat(:scope_prefix, '%')
@end
ORDER BY u.id
LIMIT :limit;

-- name: UpdateUserProfile :execrows
-- @param new_email: varchar(255)
-- @param nickname: varchar(64)
-- @param id: bigint
UPDATE users
SET
    tenant_id = tenant_id
@if-present(new_email)
  , email = :new_email
@endif
@if-present(nickname)
  , nickname = :nickname
@endif
WHERE id = :id;
