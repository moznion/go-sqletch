-- name: ListAuditLogs :many
SELECT a.id, a.actor_id, a.action, a.created_at
FROM audit_logs AS a
WHERE a.tenant_id = :tenant_id

@if-present(after_id)
  AND a.id < :after_id
@endif

ORDER BY a.id DESC
LIMIT :limit;
