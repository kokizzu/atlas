// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

//go:build !ent

package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
)

// A diff provides a MySQL implementation for schema.Inspector.
type inspect struct{ *conn }

var _ schema.Inspector = (*inspect)(nil)

// InspectRealm returns schema descriptions of all resources in the given realm.
func (i *inspect) InspectRealm(ctx context.Context, opts *schema.InspectRealmOption) (*schema.Realm, error) {
	schemas, err := i.schemas(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts == nil {
		opts = &schema.InspectRealmOption{}
	}
	var (
		mode = sqlx.ModeInspectRealm(opts)
		r    = schema.NewRealm(schemas...).SetCharset(i.charset).SetCollation(i.collate)
	)
	if len(schemas) > 0 {
		if mode.Is(schema.InspectTables) {
			if err := i.inspectTables(ctx, r, nil); err != nil {
				return nil, err
			}
			sqlx.LinkSchemaTables(schemas)
		}
	}
	return schema.ExcludeRealm(r, opts.Exclude)
}

// InspectSchema returns schema descriptions of the tables in the given schema.
// If the schema name is empty, the result will be the attached schema.
func (i *inspect) InspectSchema(ctx context.Context, name string, opts *schema.InspectOptions) (*schema.Schema, error) {
	schemas, err := i.schemas(ctx, &schema.InspectRealmOption{Schemas: []string{name}})
	if err != nil {
		return nil, err
	}
	switch n := len(schemas); {
	case n == 0:
		return nil, &schema.NotExistError{Err: fmt.Errorf("mysql: schema %q was not found", name)}
	case n > 1:
		return nil, fmt.Errorf("mysql: %d schemas were found for %q", n, name)
	}
	if opts == nil {
		opts = &schema.InspectOptions{}
	}
	var (
		mode = sqlx.ModeInspectSchema(opts)
		r    = schema.NewRealm(schemas...).SetCharset(i.charset).SetCollation(i.collate)
	)
	if mode.Is(schema.InspectTables) {
		if err := i.inspectTables(ctx, r, opts); err != nil {
			return nil, err
		}
		sqlx.LinkSchemaTables(schemas)
	}
	return schema.ExcludeSchema(r.Schemas[0], opts.Exclude)
}

func (i *inspect) inspectTables(ctx context.Context, r *schema.Realm, opts *schema.InspectOptions) error {
	if err := i.tables(ctx, r, opts); err != nil {
		return err
	}
	for _, s := range r.Schemas {
		if len(s.Tables) == 0 {
			continue
		}
		if err := i.columns(ctx, s); err != nil {
			return err
		}
		if err := i.indexes(ctx, s); err != nil {
			return err
		}
		if err := i.fks(ctx, s); err != nil {
			return err
		}
		if err := i.checks(ctx, s); err != nil {
			return err
		}
		if err := i.showCreate(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// schemas returns the list of the schemas in the database.
func (i *inspect) schemas(ctx context.Context, opts *schema.InspectRealmOption) ([]*schema.Schema, error) {
	var (
		args  []any
		query = schemasQuery
	)
	if opts != nil {
		switch n := len(opts.Schemas); {
		case n == 1 && opts.Schemas[0] == "":
			query = fmt.Sprintf(schemasQueryArgs, "= SCHEMA()")
		case n == 1 && opts.Schemas[0] != "":
			query = fmt.Sprintf(schemasQueryArgs, "= ?")
			args = append(args, opts.Schemas[0])
		case n > 0:
			query = fmt.Sprintf(schemasQueryArgs, "IN ("+nArgs(len(opts.Schemas))+")")
			for _, s := range opts.Schemas {
				args = append(args, s)
			}
		}
	}
	rows, err := i.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: querying schemas: %w", err)
	}
	defer rows.Close()
	var schemas []*schema.Schema
	for rows.Next() {
		var name, charset, collation string
		if err := rows.Scan(&name, &charset, &collation); err != nil {
			return nil, err
		}
		schemas = append(schemas, &schema.Schema{
			Name: name,
			Attrs: []schema.Attr{
				&schema.Charset{
					V: charset,
				},
				&schema.Collation{
					V: collation,
				},
			},
		})
	}
	return schemas, nil
}

func (i *inspect) tables(ctx context.Context, realm *schema.Realm, opts *schema.InspectOptions) error {
	var (
		args  []any
		query = fmt.Sprintf(i.tablesQuery(ctx), nArgs(len(realm.Schemas)))
	)
	for _, s := range realm.Schemas {
		args = append(args, s.Name)
	}
	if opts != nil && len(opts.Tables) > 0 {
		for _, t := range opts.Tables {
			args = append(args, t)
		}
		query = fmt.Sprintf(i.tablesQueryArgs(ctx), nArgs(len(realm.Schemas)), nArgs(len(opts.Tables)))
	}
	rows, err := i.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			defaultE                                                          sql.NullBool
			autoinc                                                           sql.NullInt64
			tSchema, name, charset, collation, comment, options, engine, ttyp sql.NullString
		)
		if err := rows.Scan(&tSchema, &name, &charset, &collation, &autoinc, &comment, &options, &engine, &defaultE, &ttyp); err != nil {
			return fmt.Errorf("scan table information: %w", err)
		}
		if !sqlx.ValidString(tSchema) || !sqlx.ValidString(name) {
			return fmt.Errorf("invalid schema or table name: %q.%q", tSchema.String, name.String)
		}
		s, ok := realm.Schema(tSchema.String)
		if !ok {
			return fmt.Errorf("schema %q was not found in realm", tSchema.String)
		}
		t := schema.NewTable(name.String)
		s.AddTables(t)
		if sqlx.ValidString(charset) {
			t.Attrs = append(t.Attrs, &schema.Charset{
				V: charset.String,
			})
		}
		if sqlx.ValidString(collation) {
			t.Attrs = append(t.Attrs, &schema.Collation{
				V: collation.String,
			})
		}
		if sqlx.ValidString(comment) {
			t.Attrs = append(t.Attrs, &schema.Comment{
				Text: comment.String,
			})
		}
		if sqlx.ValidString(options) {
			t.Attrs = append(t.Attrs, &CreateOptions{
				V: options.String,
			})
		}
		if sqlx.ValidString(engine) && defaultE.Valid {
			t.Attrs = append(t.Attrs, &Engine{
				V:       engine.String,
				Default: defaultE.Bool,
			})
		}
		if autoinc.Valid {
			t.Attrs = append(t.Attrs, &AutoIncrement{
				V: autoinc.Int64,
			})
		}
	}
	return rows.Err()
}

// columns queries and appends the columns of the given table.
func (i *inspect) columns(ctx context.Context, s *schema.Schema) error {
	query := columnsQuery
	if i.SupportsGeneratedColumns() {
		query = columnsExprQuery
	}
	rows, err := i.querySchema(ctx, query, s)
	if err != nil {
		return fmt.Errorf("mysql: query schema %q columns: %w", s.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := i.addColumn(s, rows); err != nil {
			return fmt.Errorf("mysql: %w", err)
		}
	}
	return rows.Err()
}

// addColumn scans the current row and adds a new column from it to the table.
func (i *inspect) addColumn(s *schema.Schema, rows *sql.Rows) error {
	var table, name, typ, comment, nullable, key, defaults, extra, charset, collation, expr sql.NullString
	if err := rows.Scan(&table, &name, &typ, &comment, &nullable, &key, &defaults, &extra, &charset, &collation, &expr); err != nil {
		return err
	}
	t, ok := s.Table(table.String)
	if !ok {
		return fmt.Errorf("table %q was not found in schema", table.String)
	}
	c := &schema.Column{
		Name: name.String,
		Type: &schema.ColumnType{
			Raw:  typ.String,
			Null: nullable.String == "YES",
		},
	}
	ct, err := ParseType(c.Type.Raw)
	if err != nil {
		return fmt.Errorf("parse %q.%q type %q: %w", t.Name, c.Name, c.Type.Raw, err)
	}
	c.Type.Type = ct
	attr, err := parseExtra(extra.String)
	if err != nil {
		return err
	}
	if attr.autoinc {
		a := &AutoIncrement{}
		if !sqlx.Has(t.Attrs, a) {
			// A table can have only one AUTO_INCREMENT column. If it was returned as NULL
			// from INFORMATION_SCHEMA, it is due to information_schema_stats_expiry, and
			// we need to extract it from the 'CREATE TABLE' command.
			putShow(t).auto = a
		}
		c.Attrs = append(c.Attrs, a)
	}
	if attr.onUpdate != "" {
		c.Attrs = append(c.Attrs, &OnUpdate{A: attr.onUpdate})
	}
	if x := expr.String; x != "" {
		if !i.Maria() {
			x = unescape(x)
		}
		c.SetGeneratedExpr(&schema.GeneratedExpr{Expr: x, Type: attr.generatedType})
	}
	if defaults.Valid {
		if i.Maria() {
			c.Default = i.marDefaultExpr(c, defaults.String)
		} else {
			c.Default = i.myDefaultExpr(c, defaults.String, attr)
		}
	}
	if sqlx.ValidString(comment) {
		c.SetComment(comment.String)
	}
	if sqlx.ValidString(charset) {
		c.SetCharset(charset.String)
	}
	if sqlx.ValidString(collation) {
		c.SetCollation(collation.String)
	}
	t.AddColumns(c)
	return nil
}

// indexes queries and appends the indexes of the given table.
func (i *inspect) indexes(ctx context.Context, s *schema.Schema) error {
	query := i.indexQuery()
	rows, err := i.querySchema(ctx, query, s)
	if err != nil {
		return fmt.Errorf("mysql: query schema %q indexes: %w", s.Name, err)
	}
	defer rows.Close()
	if err := i.addIndexes(s, rows); err != nil {
		return err
	}
	return rows.Err()
}

// addIndexes scans the rows and adds the indexes to the table.
func (i *inspect) addIndexes(s *schema.Schema, rows *sql.Rows) error {
	for rows.Next() {
		var (
			seqno                          int
			table, name, indexType         string
			nonuniq, desc                  sql.NullBool
			column, subPart, expr, comment sql.NullString
		)
		if err := rows.Scan(&table, &name, &column, &nonuniq, &seqno, &indexType, &desc, &comment, &subPart, &expr); err != nil {
			return fmt.Errorf("mysql: scanning indexes for schema %q: %w", s.Name, err)
		}
		t, ok := s.Table(table)
		if !ok {
			return fmt.Errorf("table %q was not found in schema", table)
		}
		var idx *schema.Index
		switch {
		// Primary key.
		case name == "PRIMARY":
			if idx = t.PrimaryKey; idx == nil {
				idx = schema.NewIndex("PRI").
					AddAttrs(&IndexType{T: indexType})
				t.PrimaryKey = idx
			}
		default:
			if idx, ok = t.Index(name); !ok {
				idx = schema.NewIndex(name).
					SetUnique(!nonuniq.Bool).
					AddAttrs(&IndexType{T: indexType})
				if indexType == IndexTypeFullText {
					putShow(t).addFullText(idx)
				}
				if sqlx.ValidString(comment) {
					idx.SetComment(comment.String)
				}
				t.AddIndexes(idx)
			}
		}
		// Rows are ordered by SEQ_IN_INDEX that specifies the
		// position of the column in the index definition.
		part := &schema.IndexPart{SeqNo: seqno, Desc: desc.Bool}
		switch {
		case sqlx.ValidString(expr):
			part.X = &schema.RawExpr{X: unescape(expr.String)}
		case sqlx.ValidString(column):
			part.C, ok = t.Column(column.String)
			if !ok {
				return fmt.Errorf("mysql: column %q was not found for index %q", column.String, idx.Name)
			}
			if sqlx.ValidString(subPart) && indexType != IndexTypeSpatial {
				n, err := strconv.Atoi(subPart.String)
				if err != nil {
					return fmt.Errorf("mysql: parse index prefix size %q: %w", subPart.String, err)
				}
				part.Attrs = append(part.Attrs, &SubPart{
					Len: n,
				})
			}
			part.C.Indexes = append(part.C.Indexes, idx)
		default:
			return fmt.Errorf("mysql: invalid part for index %q", idx.Name)
		}
		idx.Parts = append(idx.Parts, part)
	}
	return nil
}

// fks queries and appends the foreign keys of the given table.
func (i *inspect) fks(ctx context.Context, s *schema.Schema) error {
	rows, err := i.querySchema(ctx, fksQuery, s)
	if err != nil {
		return fmt.Errorf("mysql: querying %q foreign keys: %w", s.Name, err)
	}
	defer rows.Close()
	if err := sqlx.SchemaFKs(s, rows); err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	return rows.Err()
}

// checks queries and appends the check constraints of the given table.
func (i *inspect) checks(ctx context.Context, s *schema.Schema) error {
	query, ok := i.supportsCheck()
	if !ok {
		return nil
	}
	rows, err := i.querySchema(ctx, query, s)
	if err != nil {
		return fmt.Errorf("mysql: querying %q check constraints: %w", s.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, clause, enforced sql.NullString
		if err := rows.Scan(&table, &name, &clause, &enforced); err != nil {
			return fmt.Errorf("mysql: %w", err)
		}
		t, ok := s.Table(table.String)
		if !ok {
			return fmt.Errorf("table %q was not found in schema", table.String)
		}
		check := &schema.Check{
			Name: name.String,
			Expr: unescape(clause.String),
		}
		if i.Maria() {
			check.Expr = clause.String
			// In MariaDB, JSON is an alias to LONGTEXT. For versions >= 10.4.3, the CHARSET and COLLATE set to utf8mb4
			// and a CHECK constraint is automatically created for the column as well (i.e. JSON_VALID(`<C>`)). However,
			// we expect tools like Atlas and Ent to manually add this CHECK for older versions of MariaDB.
			c, ok := t.Column(check.Name)
			if ok && c.Type.Raw == TypeLongText && check.Expr == fmt.Sprintf("json_valid(`%s`)", c.Name) {
				c.Type.Raw = TypeJSON
				c.Type.Type = &schema.JSONType{T: TypeJSON}
				// Unset the inspected CHARSET/COLLATE attributes
				// as they are valid only for character types.
				c.UnsetCharset().UnsetCollation()
				// Skip adding this CHECK constraint to the table definition
				// as it is implicitly created by MariaDB for this JSON column.
				continue
			}
		} else if enforced.String == "NO" {
			// The ENFORCED attribute is not supported by MariaDB.
			// Also, skip adding it in case the CHECK is ENFORCED,
			// as the default is ENFORCED if not state otherwise.
			check.Attrs = append(check.Attrs, &Enforced{V: false})
		}
		t.Attrs = append(t.Attrs, check)
	}
	return rows.Err()
}

// supportsCheck reports if the connected database supports
// the CHECK clause, and return the querying for getting them.
func (i *inspect) supportsCheck() (string, bool) {
	q := myChecksQuery
	if i.Maria() {
		q = marChecksQuery
	}
	return q, i.SupportsCheck()
}

// indexQuery returns the query to retrieve the indexes of the given table.
func (i *inspect) indexQuery() string {
	query := indexesNoCommentQuery
	if i.SupportsIndexComment() {
		query = indexesQuery
	}
	if i.SupportsIndexExpr() {
		query = indexesExprQuery
	}
	return query
}

// extraAttr is a parsed version of the information_schema EXTRA column.
type extraAttr struct {
	autoinc          bool
	onUpdate         string
	generatedType    string
	defaultGenerated bool
}

var (
	reGenerateType = regexp.MustCompile(`(?i)^(stored|persistent|virtual) generated$`)
	reTimeOnUpdate = regexp.MustCompile(`(?i)^(?:default_generated )?on update (current_timestamp(?:\(\d?\))?)$`)
)

// parseExtra returns a parsed version of the EXTRA column
// from the INFORMATION_SCHEMA.COLUMNS table.
func parseExtra(extra string) (*extraAttr, error) {
	attr := &extraAttr{}
	switch el := strings.ToLower(extra); {
	case el == "", el == "null":
	case el == defaultGen:
		attr.defaultGenerated = true
		// The column has an expression default value,
		// and it is handled in Driver.addColumn.
	case el == autoIncrement:
		attr.autoinc = true
	case reTimeOnUpdate.MatchString(extra):
		attr.onUpdate = reTimeOnUpdate.FindStringSubmatch(extra)[1]
	case reGenerateType.MatchString(extra):
		attr.generatedType = reGenerateType.FindStringSubmatch(extra)[1]
	default:
		return nil, fmt.Errorf("unknown extra column attribute %q", extra)
	}
	return attr, nil
}

// showCreate sets and fixes schema elements that require information from
// the 'SHOW CREATE' command.
func (i *inspect) showCreate(ctx context.Context, s *schema.Schema) error {
	for _, t := range s.Tables {
		st, ok := popShow(t)
		if !ok {
			continue
		}
		c, err := i.createStmt(ctx, t)
		if err != nil {
			return err
		}
		st.setIndexParser(c)
		if err := st.setAutoInc(t, c); err != nil {
			return err
		}
	}
	return nil
}

var reAutoinc = regexp.MustCompile(`(?i)\s*AUTO_INCREMENT\s*=\s*(\d+)\s*`)

// createStmt loads the CREATE TABLE statement for the table.
func (i *inspect) createStmt(ctx context.Context, t *schema.Table) (*CreateStmt, error) {
	c := &CreateStmt{}
	b := &sqlx.Builder{QuoteOpening: '`', QuoteClosing: '`'}
	rows, err := i.QueryContext(ctx, b.P("SHOW CREATE TABLE").Table(t).String())
	if err != nil {
		return nil, fmt.Errorf("query CREATE TABLE %q: %w", t.Name, err)
	}
	if err := sqlx.ScanOne(rows, &sql.NullString{}, &c.S); err != nil {
		return nil, fmt.Errorf("scan CREATE TABLE %q: %w", t.Name, err)
	}
	t.Attrs = append(t.Attrs, c)
	return c, nil
}

var reCurrTimestamp = regexp.MustCompile(`(?i)^current_timestamp(?:\(\d?\))?$`)

// myDefaultExpr returns the correct schema.Expr based on the column attributes for MySQL.
func (i *inspect) myDefaultExpr(c *schema.Column, x string, attr *extraAttr) schema.Expr {
	// In MySQL, the DEFAULT_GENERATED indicates the column has an expression default value.
	if i.SupportsExprDefault() && attr.defaultGenerated {
		// Skip CURRENT_TIMESTAMP, because wrapping it with parens will translate it to now().
		if _, ok := c.Type.Type.(*schema.TimeType); ok && reCurrTimestamp.MatchString(x) {
			return &schema.RawExpr{X: x}
		}
		return &schema.RawExpr{X: sqlx.MayWrap(unescape(x))}
	}
	switch c.Type.Type.(type) {
	case *schema.BinaryType:
		// MySQL v8 uses Hexadecimal representation.
		if isHex(x) {
			return &schema.Literal{V: x}
		}
	case *BitType, *schema.BoolType, *schema.IntegerType, *schema.DecimalType, *schema.FloatType:
		return &schema.Literal{V: x}
	case *schema.TimeType:
		// "current_timestamp" is exceptional in old versions
		// of MySQL for timestamp and datetime data types.
		if reCurrTimestamp.MatchString(x) {
			return &schema.RawExpr{X: x}
		}
	}
	return &schema.Literal{V: quote(x)}
}

// parseColumn returns column parts, size and signed-info from a MySQL type.
func parseColumn(typ string) (parts []string, size int, unsigned bool, err error) {
	// Remove MariaDB like comments embedded in the type
	// for compatibility. For example: /* mariadb-5.3 */.
	if i := strings.Index(typ, "/*"); i > 0 && strings.HasSuffix(strings.TrimSpace(typ), "*/") {
		typ = strings.TrimSpace(typ[:i])
	}
	if parts = strings.FieldsFunc(typ, func(r rune) bool {
		return r == '(' || r == ')' || r == ' ' || r == ','
	}); len(parts) == 0 {
		return nil, 0, false, fmt.Errorf("unexpected or empty type %q", typ)
	}
	switch parts[0] {
	case TypeBit, TypeBinary, TypeVarBinary, TypeChar, TypeVarchar:
	case TypeTinyInt, TypeSmallInt, TypeMediumInt, TypeInt, TypeBigInt,
		TypeDecimal, TypeNumeric, TypeFloat, TypeDouble, TypeReal:
		if attr := parts[len(parts)-1]; attr == "unsigned" || attr == "zerofill" {
			unsigned = true
		}
	}
	if len(parts) > 1 && sqlx.IsUint(parts[1]) {
		size, err = strconv.Atoi(parts[1])
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("parse %q to int: %w", parts[1], err)
	}
	return parts, size, unsigned, nil
}

// hasNumericDefault reports if the given type has a numeric default value.
func hasNumericDefault(t schema.Type) bool {
	switch t.(type) {
	case *BitType, *schema.BoolType, *schema.IntegerType, *schema.DecimalType, *schema.FloatType:
		return true
	}
	return false
}

func isHex(x string) bool { return len(x) > 2 && strings.ToLower(x[:2]) == "0x" }

// marDefaultExpr returns the correct schema.Expr based on the column attributes for MariaDB.
func (i *inspect) marDefaultExpr(c *schema.Column, x string) schema.Expr {
	// Unlike MySQL, NULL means default to NULL or no default.
	if x == "NULL" {
		return nil
	}
	// From MariaDB 10.2.7, string-based literals are quoted to distinguish them from expressions.
	if i.GTE("10.2.7") && sqlx.IsQuoted(x, '\'') {
		return &schema.Literal{V: x}
	}
	// In this case, we need to manually check if the expression is literal, or fallback to raw expression.
	switch c.Type.Type.(type) {
	case *BitType:
		// Bit literal values. See https://mariadb.com/kb/en/binary-literals.
		if strings.HasPrefix(x, "b'") && strings.HasSuffix(x, "'") {
			return &schema.Literal{V: x}
		}
	case *schema.BoolType, *schema.IntegerType, *schema.DecimalType, *schema.FloatType:
		if _, err := strconv.ParseFloat(x, 64); err == nil {
			return &schema.Literal{V: x}
		}
	case *schema.TimeType:
		// "current_timestamp" is exceptional in old versions
		// of MySQL (i.e. MariaDB in this case).
		if strings.ToLower(x) == currentTS {
			return &schema.RawExpr{X: x}
		}
	}
	if !i.SupportsExprDefault() {
		return &schema.Literal{V: quote(x)}
	}
	return &schema.RawExpr{X: sqlx.MayWrap(x)}
}

func (i *inspect) querySchema(ctx context.Context, query string, s *schema.Schema) (*sql.Rows, error) {
	// Number of times the schema name is parameterized.
	args := make([]any, strings.Count(query, "?"))
	for i := range args {
		args[i] = s.Name
	}
	for _, t := range s.Tables {
		args = append(args, t.Name)
	}
	return i.QueryContext(ctx, fmt.Sprintf(query, nArgs(len(s.Tables))), args...)
}

func nArgs(n int) string { return strings.Repeat("?, ", n-1) + "?" }

const (
	// Query to list system variables.
	variablesQuery = "SELECT @@version, @@collation_server, @@character_set_server, @@lower_case_table_names"

	// Query to list database schemas.
	schemasQuery = "SELECT `SCHEMA_NAME`, `DEFAULT_CHARACTER_SET_NAME`, `DEFAULT_COLLATION_NAME` from `INFORMATION_SCHEMA`.`SCHEMATA` WHERE `SCHEMA_NAME` NOT IN ('information_schema','innodb','mysql','performance_schema','sys') ORDER BY `SCHEMA_NAME`"

	// Query to list specific database schemas.
	schemasQueryArgs = "SELECT `SCHEMA_NAME`, `DEFAULT_CHARACTER_SET_NAME`, `DEFAULT_COLLATION_NAME` from `INFORMATION_SCHEMA`.`SCHEMATA` WHERE `SCHEMA_NAME` %s ORDER BY `SCHEMA_NAME`"

	// Query to list table columns.
	columnsQuery     = "SELECT `TABLE_NAME`, `COLUMN_NAME`, `COLUMN_TYPE`, `COLUMN_COMMENT`, `IS_NULLABLE`, `COLUMN_KEY`, `COLUMN_DEFAULT`, `EXTRA`, `CHARACTER_SET_NAME`, `COLLATION_NAME`, NULL AS `GENERATION_EXPRESSION` FROM `INFORMATION_SCHEMA`.`COLUMNS` WHERE `TABLE_SCHEMA` = ? AND `TABLE_NAME` IN (%s) ORDER BY `ORDINAL_POSITION`"
	columnsExprQuery = "SELECT `TABLE_NAME`, `COLUMN_NAME`, `COLUMN_TYPE`, `COLUMN_COMMENT`, `IS_NULLABLE`, `COLUMN_KEY`, `COLUMN_DEFAULT`, `EXTRA`, `CHARACTER_SET_NAME`, `COLLATION_NAME`, `GENERATION_EXPRESSION` FROM `INFORMATION_SCHEMA`.`COLUMNS` WHERE `TABLE_SCHEMA` = ? AND `TABLE_NAME` IN (%s) ORDER BY `ORDINAL_POSITION`"

	// Query to list table indexes.
	indexesQuery          = "SELECT `TABLE_NAME`, `INDEX_NAME`, `COLUMN_NAME`, `NON_UNIQUE`, `SEQ_IN_INDEX`, `INDEX_TYPE`, UPPER(`COLLATION`) = 'D' AS `DESC`, `INDEX_COMMENT`, `SUB_PART`, NULL AS `EXPRESSION` FROM `INFORMATION_SCHEMA`.`STATISTICS` WHERE `TABLE_SCHEMA` = ? AND `TABLE_NAME` IN (%s) ORDER BY `index_name`, `seq_in_index`"
	indexesExprQuery      = "SELECT `TABLE_NAME`, `INDEX_NAME`, `COLUMN_NAME`, `NON_UNIQUE`, `SEQ_IN_INDEX`, `INDEX_TYPE`, UPPER(`COLLATION`) = 'D' AS `DESC`, `INDEX_COMMENT`, `SUB_PART`, `EXPRESSION` FROM `INFORMATION_SCHEMA`.`STATISTICS` WHERE `TABLE_SCHEMA` = ? AND `TABLE_NAME` IN (%s) ORDER BY `index_name`, `seq_in_index`"
	indexesNoCommentQuery = "SELECT `TABLE_NAME`, `INDEX_NAME`, `COLUMN_NAME`, `NON_UNIQUE`, `SEQ_IN_INDEX`, `INDEX_TYPE`, UPPER(`COLLATION`) = 'D' AS `DESC`, NULL AS `INDEX_COMMENT`, `SUB_PART`, NULL AS `EXPRESSION` FROM `INFORMATION_SCHEMA`.`STATISTICS` WHERE `TABLE_SCHEMA` = ? AND `TABLE_NAME` IN (%s) ORDER BY `index_name`, `seq_in_index`"

	tablesQuery = `
SELECT
	t1.TABLE_SCHEMA,
	t1.TABLE_NAME,
	t2.CHARACTER_SET_NAME,
	t1.TABLE_COLLATION,
	t1.AUTO_INCREMENT,
	t1.TABLE_COMMENT,
	t1.CREATE_OPTIONS,
	t1.ENGINE,
	t3.SUPPORT = 'DEFAULT' AS DEFAULT_ENGINE,
	t1.TABLE_TYPE
FROM
	INFORMATION_SCHEMA.TABLES AS t1
	LEFT JOIN INFORMATION_SCHEMA.COLLATIONS AS t2
	ON t1.TABLE_COLLATION = t2.COLLATION_NAME
	LEFT JOIN INFORMATION_SCHEMA.ENGINES AS t3
	ON t1.ENGINE = t3.ENGINE
WHERE
	TABLE_SCHEMA IN (%s)
	AND TABLE_TYPE = 'BASE TABLE'
ORDER BY
	TABLE_SCHEMA, TABLE_NAME`

	tablesQueryArgs = `
SELECT
	t1.TABLE_SCHEMA,
	t1.TABLE_NAME,
	t2.CHARACTER_SET_NAME,
	t1.TABLE_COLLATION,
	t1.AUTO_INCREMENT,
	t1.TABLE_COMMENT,
	t1.CREATE_OPTIONS,
	t1.ENGINE,
	t3.SUPPORT = 'DEFAULT' AS DEFAULT_ENGINE,
	t1.TABLE_TYPE
FROM
	INFORMATION_SCHEMA.TABLES AS t1
	JOIN INFORMATION_SCHEMA.COLLATIONS AS t2
	ON t1.TABLE_COLLATION = t2.COLLATION_NAME
	LEFT JOIN INFORMATION_SCHEMA.ENGINES AS t3
	ON t1.ENGINE = t3.ENGINE
WHERE
	TABLE_SCHEMA IN (%s)
	AND TABLE_NAME IN (%s)
	AND TABLE_TYPE = 'BASE TABLE'
ORDER BY
	TABLE_SCHEMA, TABLE_NAME`

	// Query to list table check constraints.
	myChecksQuery = `
SELECT
	t1.TABLE_NAME,
	t1.CONSTRAINT_NAME,
	t2.CHECK_CLAUSE,
	t1.ENFORCED
FROM
	INFORMATION_SCHEMA.TABLE_CONSTRAINTS AS t1
	JOIN INFORMATION_SCHEMA.CHECK_CONSTRAINTS AS t2
	ON t1.CONSTRAINT_NAME = t2.CONSTRAINT_NAME
	AND t1.CONSTRAINT_SCHEMA = t2.CONSTRAINT_SCHEMA
WHERE
	t1.CONSTRAINT_TYPE = 'CHECK'
	AND t1.TABLE_SCHEMA = ?
	AND t1.TABLE_NAME IN (%s)
ORDER BY
	t1.TABLE_NAME, t1.CONSTRAINT_NAME
`

	marChecksQuery = `
SELECT
	TABLE_NAME,
	CONSTRAINT_NAME,
	CHECK_CLAUSE,
	"YES" AS ENFORCED
FROM
	INFORMATION_SCHEMA.CHECK_CONSTRAINTS
WHERE
	CONSTRAINT_SCHEMA = ?
	AND TABLE_NAME IN (%s)
ORDER BY
	TABLE_NAME, CONSTRAINT_NAME
`
	// Query to list table foreign keys.
	fksQuery = `
SELECT
	t1.CONSTRAINT_NAME,
	t1.TABLE_NAME,
	t1.COLUMN_NAME,
	t1.TABLE_SCHEMA,
	t1.REFERENCED_TABLE_NAME,
	t1.REFERENCED_COLUMN_NAME,
	t1.REFERENCED_TABLE_SCHEMA,
	t2.UPDATE_RULE,
	t2.DELETE_RULE
FROM
	INFORMATION_SCHEMA.KEY_COLUMN_USAGE AS t1
	JOIN INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS AS t2
	ON t1.CONSTRAINT_NAME = t2.CONSTRAINT_NAME
WHERE
	t1.REFERENCED_COLUMN_NAME IS NOT NULL
	AND BINARY t1.TABLE_SCHEMA = ?
	AND BINARY t2.CONSTRAINT_SCHEMA = ?
	AND t1.TABLE_NAME IN (%s)
ORDER BY
	BINARY t1.TABLE_NAME,
	BINARY t1.CONSTRAINT_NAME,
	t1.ORDINAL_POSITION`
)

type (
	// AutoIncrement attribute for columns with "AUTO_INCREMENT" as a default.
	// V represent an optional start value for the counter.
	AutoIncrement struct {
		schema.Attr
		V int64
	}

	// CreateOptions attribute for describing extra options used with CREATE TABLE.
	CreateOptions struct {
		schema.Attr
		V string
	}

	// CreateStmt describes the SQL statement used to create a table.
	CreateStmt struct {
		schema.Attr
		S string
	}

	// Engine attribute describes the storage engine used to create a table.
	Engine struct {
		schema.Attr
		V       string // InnoDB, MyISAM, etc.
		Default bool   // The default engine used by the server.
	}

	// SystemVersioned is an attribute attached to MariaDB tables indicates they are
	// system versioned. See: https://mariadb.com/kb/en/system-versioned-tables
	SystemVersioned struct {
		schema.Attr
	}

	// OnUpdate attribute for columns with "ON UPDATE CURRENT_TIMESTAMP" as a default.
	OnUpdate struct {
		schema.Attr
		A string
	}

	// SubPart attribute defines an option index prefix length for columns.
	SubPart struct {
		schema.Attr
		Len int
	}

	// Enforced attribute defines the ENFORCED flag for CHECK constraint.
	Enforced struct {
		schema.Attr
		V bool // V indicates if the CHECK is enforced or not.
	}

	// The DisplayWidth represents a display width of an integer type.
	DisplayWidth struct {
		schema.Attr
		N int
	}

	// The ZeroFill represents the ZEROFILL attribute which is
	// deprecated for MySQL version >= 8.0.17.
	ZeroFill struct {
		schema.Attr
		A string
	}

	// IndexType represents an index type.
	IndexType struct {
		schema.Attr
		T string // BTREE, HASH, FULLTEXT, SPATIAL, RTREE
	}

	// IndexParser defines the parser plugin used
	// by a FULLTEXT index.
	IndexParser struct {
		schema.Attr
		P string // Name of the parser plugin. e.g., ngram or mecab.
	}

	// BitType represents the type bit.
	BitType struct {
		schema.Type
		T    string
		Size int
	}

	// SetType represents a set type.
	SetType struct {
		schema.Type
		Values []string
	}

	// NetworkType stores an IPv4 or IPv6 address.
	NetworkType struct {
		schema.Type
		T string
	}

	// putShow is an intermediate table attribute used
	// on inspection to indicate if the 'SHOW TABLE' is
	// required and for what.
	showTable struct {
		schema.Attr
		// AUTO_INCREMENT value due to missing value in information_schema.
		auto *AutoIncrement
		// FULLTEXT indexes that might have custom parser.
		idxs []*schema.Index
	}
)

// NewAutoIncrement returns an "auto increment" attribute.
func NewAutoIncrement(v uint64) *AutoIncrement {
	return &AutoIncrement{V: max(0, int64(v))} // Keep BC with old ent versions.
}

// addIndex adds an index to the list of indexes
// that needs further processing.
func (s *showTable) addFullText(idx *schema.Index) {
	s.idxs = append(s.idxs, idx)
}

// setAutoInc extracts the updated AUTO_INCREMENT from CREATE TABLE.
func (s *showTable) setAutoInc(t *schema.Table, c *CreateStmt) error {
	if s.auto == nil {
		return nil
	}
	if sqlx.Has(t.Attrs, &AutoIncrement{}) {
		return fmt.Errorf("unexpected AUTO_INCREMENT attributes for table: %q", t.Name)
	}
	matches := reAutoinc.FindStringSubmatch(c.S)
	if len(matches) != 2 {
		return nil
	}
	v, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return err
	}
	s.auto.V = v
	t.Attrs = append(t.Attrs, s.auto)
	return nil
}

// reIndexParser matches the parser name from the index definition.
var reIndexParser = regexp.MustCompile("/\\*!50100 WITH PARSER `([^`]+)` \\*/")

// setIndexParser updates the FULLTEXT parser from CREATE TABLE statement.
func (s *showTable) setIndexParser(c *CreateStmt) {
	b := (&sqlx.Builder{QuoteOpening: '`', QuoteClosing: '`'}).P("FULLTEXT KEY")
	for _, idx := range s.idxs {
		bi := b.Clone().Ident(idx.Name).Wrap(func(b *sqlx.Builder) {
			b.MapComma(idx.Parts, func(i int, b *sqlx.Builder) {
				// We expect column names only, as functional
				// fulltext indexes are not supported by MySQL.
				if idx.Parts[i].C != nil {
					b.Ident(idx.Parts[i].C.Name)
				}
			})
		})
		i := strings.Index(c.S, bi.String())
		if i == -1 || i+bi.Len() >= len(c.S) {
			continue
		}
		i += bi.Len()
		j := strings.Index(c.S[i:], "\n")
		if j == -1 {
			continue
		}
		// The rest of the line holds index, algorithm and lock options.
		if matches := reIndexParser.FindStringSubmatch(c.S[i : i+j]); len(matches) == 2 {
			idx.AddAttrs(&IndexParser{P: matches[1]})
		}
	}
}

func putShow(t *schema.Table) *showTable {
	for i := range t.Attrs {
		if s, ok := t.Attrs[i].(*showTable); ok {
			return s
		}
	}
	s := &showTable{}
	t.Attrs = append(t.Attrs, s)
	return s
}

func popShow(t *schema.Table) (*showTable, bool) {
	for i := range t.Attrs {
		if s, ok := t.Attrs[i].(*showTable); ok {
			t.Attrs = append(t.Attrs[:i], t.Attrs[i+1:]...)
			return s, true
		}
	}
	return nil, false
}
