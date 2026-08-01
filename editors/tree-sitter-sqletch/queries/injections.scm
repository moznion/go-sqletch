; SQL runs are opaque to this grammar; hand them to the editor's SQL
; grammar as one combined document.
((sql_token) @injection.content
 (#set! injection.language "sql")
 (#set! injection.combined))
