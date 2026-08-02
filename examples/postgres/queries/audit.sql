-- name: ListAuditLogs :many
SELECT a.id, a.actor_id, a.action, a.created_at
FROM audit_logs AS a
WHERE a.tenant_id = :tenant_id

@if-present(after_id)
  AND a.id < :after_id
@endif

ORDER BY a.id DESC
LIMIT :limit;

-- name: CountAuditLogs :one
-- The tenant_scope policy (sqletch.yaml) weaves `tenant_id` scoping
-- into every query touching audit_logs. ListAuditLogs above already
-- scopes by hand, so it is left as written; this one never mentions
-- tenants and is scoped by the compiler.
SELECT count(*) AS total FROM audit_logs;

-- name: AllAuditActions :many
-- Crossing tenants is a deliberate, reviewable exemption.
-- @policy-optout: tenant_scope (ops dashboard; aggregates across tenants)
SELECT a.action, count(*) AS occurrences
FROM audit_logs AS a
GROUP BY a.action
ORDER BY occurrences DESC;

-- name: UserAuditActions :many
-- audit_logs sits on the null-extended side here, so the policy
-- weaves into the JOIN's ON clause: every user row survives, and only
-- the tenant's audit rows join (a WHERE conjunct would have turned
-- the LEFT JOIN into an inner join).
SELECT u.id, a.action
FROM users AS u
LEFT JOIN audit_logs AS a ON a.actor_id = u.id
ORDER BY u.id, a.id;
