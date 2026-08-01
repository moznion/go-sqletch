; sqletch construct keywords
[
  "@if-present"
  "@endif"
  "@when"
  "@end"
  "@choose"
  "@case"
  "@default"
  "@order-by"
  "@key"
  "@filter-tree"
  "@filter-tree!"
  "@predicate"
  "@in"
] @keyword.directive

(header_marker) @keyword.directive
(directive_marker) @keyword.directive

(query_name) @function
(annotation) @keyword.modifier
(type_name) @type

(parameter) @variable.parameter
(param_ref) @variable.parameter
(case_name) @constant
(key_name) @constant
(predicate_name) @constant
(column_name) @variable.member

(comparison) @operator
(literal) @constant

(comment) @comment
(block_comment) @comment
(string) @string
(quoted_ident) @variable
