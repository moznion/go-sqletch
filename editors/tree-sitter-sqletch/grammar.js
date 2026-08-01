/**
 * tree-sitter grammar for sqletch templates (docs/design/11).
 *
 * The grammar parses the TEMPLATE structure — query headers, construct
 * blocks with their bodies, parameter references, and the comment
 * directives — and treats SQL runs as opaque `sql_token`s; an
 * injection query hands those to the editor's SQL grammar. It is a
 * highlighting grammar: tolerant by design, the compiler's scanner is
 * the authority on validity.
 */

/* eslint-disable arrow-parens */

const IDENT = /[A-Za-z_][A-Za-z0-9_]*/;

module.exports = grammar({
  name: 'sqletch',

  extras: $ => [/[ \t\r\n]+/],

  rules: {
    document: $ => repeat($._item),

    _item: $ => choice(
      $.header,
      $.param_directive,
      $.column_directive,
      $._construct,
      $._sql_content,
    ),

    _construct: $ => choice(
      $.if_present_block,
      $.when_block,
      $.choose_block,
      $.order_by_block,
      $.filter_tree_block,
      $.in_expr,
    ),

    // ---- header and directives (comment-shaped, so they carry
    // lexical precedence over the generic comment token) -------------

    header: $ => seq(
      alias(token(prec(3, /--[ \t]*name:/)), $.header_marker),
      field('name', alias(IDENT, $.query_name)),
      field('annotation', alias(/:[a-z]+/, $.annotation)),
    ),

    param_directive: $ => seq(
      alias(token(prec(3, /--[ \t]*@param/)), $.directive_marker),
      field('name', alias(IDENT, $.parameter)),
      ':',
      field('type', alias(/[^\n]+/, $.type_name)),
    ),

    column_directive: $ => seq(
      alias(token(prec(3, /--[ \t]*@column/)), $.directive_marker),
      field('name', alias(IDENT, $.column_name)),
      ':',
      field('type', alias(/[^\n]+/, $.type_name)),
    ),

    // ---- construct blocks ------------------------------------------

    if_present_block: $ => seq(
      '@if-present',
      '(',
      field('guards', $.parameter_list),
      ')',
      repeat($._sql_content),
      '@endif',
    ),

    when_block: $ => seq(
      '@when',
      '(',
      field('param', alias(IDENT, $.parameter)),
      field('op', $.comparison),
      field('value', $.literal),
      ')',
      repeat($._sql_content),
      '@end',
    ),

    choose_block: $ => seq(
      '@choose',
      '(',
      field('param', alias(IDENT, $.parameter)),
      ')',
      repeat($.case_clause),
      optional($.default_clause),
      '@end',
    ),

    case_clause: $ => seq(
      '@case',
      '(',
      field('name', alias(IDENT, $.case_name)),
      ')',
      repeat($._sql_content),
    ),

    default_clause: $ => seq('@default', repeat($._sql_content)),

    order_by_block: $ => seq(
      '@order-by',
      '(',
      field('param', alias(IDENT, $.parameter)),
      ')',
      repeat($.key_clause),
      optional($.default_clause),
      '@end',
    ),

    key_clause: $ => seq(
      '@key',
      '(',
      field('name', alias(IDENT, $.key_name)),
      ')',
      repeat($._sql_content),
    ),

    filter_tree_block: $ => seq(
      choice('@filter-tree!', '@filter-tree'),
      '(',
      field('param', alias(IDENT, $.parameter)),
      ')',
      repeat($.predicate_clause),
      '@end',
    ),

    predicate_clause: $ => seq(
      '@predicate',
      '(',
      field('name', alias(IDENT, $.predicate_name)),
      ')',
      repeat($._sql_content),
    ),

    in_expr: $ => seq('@in', '(', $.param_ref, ')'),

    parameter_list: $ => seq(
      alias(IDENT, $.parameter),
      repeat(seq(',', alias(IDENT, $.parameter))),
    ),

    comparison: _ => choice('=', '!=', '<>'),

    literal: _ => choice(
      /'([^']|'')*'/,
      /-?[0-9]+/,
      'true',
      'false',
    ),

    // ---- SQL content (opaque; injected as sql) ---------------------

    _sql_content: $ => choice(
      $.comment,
      $.block_comment,
      $.string,
      $.quoted_ident,
      $.param_ref,
      $.sql_token,
    ),

    comment: _ => token(prec(1, /--[^\n]*/)),

    block_comment: _ => token(seq('/*', /([^*]|\*+[^*/])*\**/, '*/')),

    string: _ => token(/'([^']|'')*'/),

    quoted_ident: _ => token(choice(
      /"([^"]|"")*"/,
      /`([^`]|``)*`/,
      /\[[^\]]*\]/,
    )),

    param_ref: _ => token(seq(':', IDENT)),

    // Anything else: runs of plain characters, plus single-character
    // fallbacks for the bytes that can START a special token but did
    // not form one.
    sql_token: _ => token(choice(
      /[^@:'"`\[\-\/ \t\r\n]+/,
      /[:\-\/\[]/,
    )),
  },
});
