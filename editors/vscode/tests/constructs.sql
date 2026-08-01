-- SYNTAX TEST "source.sql.sqletch" "sqletch construct highlighting"

-- name: SearchUsers :many
-- <------- keyword.other.directive.sqletch
--       ^^^^^^^^^^^ entity.name.function.sqletch
--                   ^^^^^ storage.modifier.sqletch

-- @param status: text
-- <-------- keyword.other.directive.sqletch
--        ^^^^^^ variable.parameter.sqletch
--                ^^^^ storage.type.sqletch

-- @column n: integer
-- <--------- keyword.other.directive.sqletch
--         ^ variable.parameter.sqletch
--            ^^^^^^^ storage.type.sqletch

@if-present(organization_id, status)
-- <---------- keyword.control.sqletch
--          ^^^^^^^^^^^^^^^ variable.parameter.sqletch
--                           ^^^^^^ variable.parameter.sqletch

  AND u.status = :status
--               ^^^^^^^ variable.parameter.sqletch

@endif
-- <----- keyword.control.sqletch

@when(include_cron = false)
-- <---- keyword.control.sqletch
--    ^^^^^^^^^^^^ variable.parameter.sqletch
--                 ^ keyword.operator.comparison.sqletch
--                   ^^^^^ constant.language.sqletch

@end
-- <--- keyword.control.sqletch

@choose(sort)
-- <------ keyword.control.sqletch
--      ^^^^ variable.parameter.sqletch

@case(email_asc)
-- <---- keyword.control.sqletch
--    ^^^^^^^^^ variable.parameter.sqletch

@default
-- <------- keyword.control.sqletch

@end
-- <--- keyword.control.sqletch

@order-by(sort)
-- <-------- keyword.control.sqletch
--        ^^^^ variable.parameter.sqletch

@key(created_at)
-- <--- keyword.control.sqletch
--   ^^^^^^^^^^ variable.parameter.sqletch

@end
-- <--- keyword.control.sqletch

@filter-tree!(scope)
-- <------------ keyword.control.sqletch
--            ^^^^^ variable.parameter.sqletch

@predicate(tenant)
-- <--------- keyword.control.sqletch
--         ^^^^^^ variable.parameter.sqletch

@end
-- <--- keyword.control.sqletch

WHERE u.status @in(:statuses)
--             ^^^ keyword.control.sqletch
--                 ^^^^^^^^^ variable.parameter.sqletch

SELECT tenant_id::text FROM t WHERE a = :param_x
--                                      ^^^^^^^^ variable.parameter.sqletch

