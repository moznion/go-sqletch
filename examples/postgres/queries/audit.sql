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
