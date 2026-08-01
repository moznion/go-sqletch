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

-- name: CountByStatus :many
-- @param tenant_id: integer
-- @column n: integer
SELECT u.status, count(*) AS n
FROM users AS u
WHERE u.tenant_id = :tenant_id
GROUP BY u.status
ORDER BY u.status;
