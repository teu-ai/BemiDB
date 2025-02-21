package main

import (
	"context"
	"regexp"
	"strings"

	pgQuery "github.com/pganalyze/pg_query_go/v5"
)

var MAX_REDUNDANT_PG_NAMESPACE_OID = 1265

var PG_CATALOG_TABLE_NAMES = Set[string]{}

func CreatePgCatalogTableQueries(config *Config) []string {
	result := []string{
		// Static empty tables
		"CREATE TABLE pg_inherits(inhrelid oid, inhparent oid, inhseqno int4, inhdetachpending bool)",
		"CREATE TABLE pg_shdescription(objoid oid, classoid oid, description text)",
		"CREATE TABLE pg_statio_user_tables(relid oid, schemaname text, relname text, heap_blks_read int8, heap_blks_hit int8, idx_blks_read int8, idx_blks_hit int8, toast_blks_read int8, toast_blks_hit int8, tidx_blks_read int8, tidx_blks_hit int8)",
		"CREATE TABLE pg_replication_slots(slot_name text, plugin text, slot_type text, datoid oid, database text, temporary bool, active bool, active_pid int4, xmin int8, catalog_xmin int8, restart_lsn text, confirmed_flush_lsn text, wal_status text, safe_wal_size int8, two_phase bool, conflicting bool)",
		"CREATE TABLE pg_stat_gssapi(pid int4, gss_authenticated bool, principal text, encrypted bool, credentials_delegated bool)",
		"CREATE TABLE pg_auth_members(oid text, roleid oid, member oid, grantor oid, admin_option bool, inherit_option bool, set_option bool)",
		"CREATE TABLE pg_stat_activity(datid oid, datname text, pid int4, usesysid oid, usename text, application_name text, client_addr inet, client_hostname text, client_port int4, backend_start timestamp, xact_start timestamp, query_start timestamp, state_change timestamp, wait_event_type text, wait_event text, state text, backend_xid int8, backend_xmin int8, query text, backend_type text)",
		"CREATE TABLE pg_views(schemaname text, viewname text, viewowner text, definition text)",
		"CREATE TABLE pg_matviews(schemaname text, matviewname text, matviewowner text, tablespace text, hasindexes bool, ispopulated bool, definition text)",
		"CREATE TABLE pg_opclass(oid oid, opcmethod oid, opcname text, opcnamespace oid, opcowner oid, opcfamily oid, opcintype oid, opcdefault bool, opckeytype oid)",

		// Dynamic tables
		// DuckDB doesn't handle dynamic view replacement properly
		"CREATE TABLE pg_stat_user_tables(relid oid, schemaname text, relname text, seq_scan int8, last_seq_scan timestamp, seq_tup_read int8, idx_scan int8, last_idx_scan timestamp, idx_tup_fetch int8, n_tup_ins int8, n_tup_upd int8, n_tup_del int8, n_tup_hot_upd int8, n_tup_newpage_upd int8, n_live_tup int8, n_dead_tup int8, n_mod_since_analyze int8, n_ins_since_vacuum int8, last_vacuum timestamp, last_autovacuum timestamp, last_analyze timestamp, last_autoanalyze timestamp, vacuum_count int8, autovacuum_count int8, analyze_count int8, autoanalyze_count int8)",

		// Static views
		"CREATE VIEW pg_shadow AS SELECT '" + config.User + "' AS usename, '10'::oid AS usesysid, FALSE AS usecreatedb, FALSE AS usesuper, TRUE AS userepl, FALSE AS usebypassrls, '" + config.EncryptedPassword + "' AS passwd, NULL::timestamp AS valuntil, NULL::text[] AS useconfig",
		"CREATE VIEW pg_roles AS SELECT '10'::oid AS oid, '" + config.User + "' AS rolname, TRUE AS rolsuper, TRUE AS rolinherit, TRUE AS rolcreaterole, TRUE AS rolcreatedb, TRUE AS rolcanlogin, FALSE AS rolreplication, -1 AS rolconnlimit, NULL::text AS rolpassword, NULL::timestamp AS rolvaliduntil, FALSE AS rolbypassrls, NULL::text[] AS rolconfig",
		"CREATE VIEW pg_extension AS SELECT '13823'::oid AS oid, 'plpgsql' AS extname, '10'::oid AS extowner, '11'::oid AS extnamespace, FALSE AS extrelocatable, '1.0'::text AS extversion, NULL::text[] AS extconfig, NULL::text[] AS extcondition",
		"CREATE VIEW pg_database AS SELECT '16388'::oid AS oid, '" + config.Database + "' AS datname, '10'::oid AS datdba, '6'::int4 AS encoding, 'c' AS datlocprovider, FALSE AS datistemplate, TRUE AS datallowconn, '-1'::int4 AS datconnlimit, '722'::int8 AS datfrozenxid, '1'::int4 AS datminmxid, '1663'::oid AS dattablespace, 'en_US.UTF-8' AS datcollate, 'en_US.UTF-8' AS datctype, 'en_US.UTF-8' AS datlocale, NULL::text AS daticurules, NULL::text AS datcollversion, NULL::text[] AS datacl",
		"CREATE VIEW pg_user AS SELECT '" + config.User + "' AS usename, '10'::oid AS usesysid, TRUE AS usecreatedb, TRUE AS usesuper, TRUE AS userepl, TRUE AS usebypassrls, '' AS passwd, NULL::timestamp AS valuntil, NULL::text[] AS useconfig",
		"CREATE VIEW pg_collation AS SELECT '100'::oid AS oid, 'default' AS collname, '11'::oid AS collnamespace, '10'::oid AS collowner, 'd' AS collprovider, TRUE AS collisdeterministic, '-1'::int4 AS collencoding, NULL::text AS collcollate, NULL::text AS collctype, NULL::text AS colliculocale, NULL::text AS collicurules, NULL::text AS collversion",

		// Dynamic views
		// DuckDB does not support indnullsnotdistinct column
		"CREATE VIEW pg_index AS SELECT *, FALSE AS indnullsnotdistinct FROM pg_catalog.pg_index",
	}
	PG_CATALOG_TABLE_NAMES = extractTableNames(result)
	return result
}

type QueryRemapperTable struct {
	parserTable         *ParserTable
	parserWhere         *ParserWhere
	parserFunction      *ParserFunction
	icebergSchemaTables Set[IcebergSchemaTable]
	icebergReader       *IcebergReader
	duckdb              *Duckdb
	config              *Config
}

func NewQueryRemapperTable(config *Config, icebergReader *IcebergReader, duckdb *Duckdb) *QueryRemapperTable {
	remapper := &QueryRemapperTable{
		parserTable:    NewParserTable(config),
		parserWhere:    NewParserWhere(config),
		parserFunction: NewParserFunction(config),
		icebergReader:  icebergReader,
		duckdb:         duckdb,
		config:         config,
	}
	remapper.reloadIceberSchemaTables()
	return remapper
}

func (remapper *QueryRemapperTable) NodeToQuerySchemaTable(node *pgQuery.Node) QuerySchemaTable {
	return remapper.parserTable.NodeToQuerySchemaTable(node)
}

// FROM / JOIN [TABLE]
func (remapper *QueryRemapperTable) RemapTable(node *pgQuery.Node) *pgQuery.Node {
	parser := remapper.parserTable
	qSchemaTable := parser.NodeToQuerySchemaTable(node)

	// pg_catalog.pg_* system tables
	if remapper.isTableFromPgCatalog(qSchemaTable) {
		switch qSchemaTable.Table {

		// pg_class -> reload Iceberg tables
		case PG_TABLE_PG_CLASS:
			remapper.reloadIceberSchemaTables()

		// pg_stat_user_tables -> return Iceberg tables
		case PG_TABLE_PG_STAT_USER_TABLES:
			remapper.reloadIceberSchemaTables()
			remapper.upsertPgStatUserTables(remapper.icebergSchemaTables)
		}

		// pg_catalog.pg_table -> main.pg_table
		if PG_CATALOG_TABLE_NAMES.Contains(qSchemaTable.Table) {
			parser.RemapSchemaToMain(node)
			return node
		}

		// pg_catalog.pg_* other system tables -> return as is
		return node
	}

	// information_schema.* system tables
	if parser.IsTableFromInformationSchema(qSchemaTable) {
		switch qSchemaTable.Table {

		// information_schema.tables -> reload Iceberg tables
		case PG_TABLE_TABLES:
			remapper.reloadIceberSchemaTables()
			return node

		// information_schema.columns -> return hard-coded columns
		// DuckDB does not support udt_schema, udt_name
		case PG_TABLE_COLUMNS:

		// information_schema.* other system tables -> return as is
		default:
			return node
		}
	}

	// public.table -> FROM iceberg_scan('path', skip_schema_inference = true) table
	// schema.table -> FROM iceberg_scan('path', skip_schema_inference = true) schema_table
	schemaTable := qSchemaTable.ToIcebergSchemaTable()
	if !remapper.icebergSchemaTables.Contains(schemaTable) { // Reload Iceberg tables if not found
		remapper.reloadIceberSchemaTables()
		if !remapper.icebergSchemaTables.Contains(schemaTable) {
			return node // Let it return "Catalog Error: Table with name _ does not exist!"
		}
	}
	icebergPath := remapper.icebergReader.MetadataFilePath(schemaTable) // iceberg/schema/table/metadata/v1.metadata.json
	return parser.MakeIcebergTableNode(icebergPath, qSchemaTable)
}

// FROM [PG_FUNCTION()]
func (remapper *QueryRemapperTable) RemapTableFunction(node *pgQuery.Node) *pgQuery.Node {
	parser := remapper.parserTable

	schemaFunction := parser.SchemaFunction(node)

	if remapper.isFunctionFromPgCatalog(schemaFunction) {
		switch {

		// pg_catalog.pg_get_keywords() -> hard-coded keywords
		case schemaFunction.Function == PG_FUNCTION_PG_GET_KEYWORDS:
			return parser.MakePgGetKeywordsNode(node)

		// pg_catalog.pg_show_all_settings() -> duckdb_settings()
		case schemaFunction.Function == PG_FUNCTION_PG_SHOW_ALL_SETTINGS:
			return parser.MakePgShowAllSettingsNode(node)

		// pg_catalog.pg_is_in_recovery() -> 'f'::bool
		case schemaFunction.Function == PG_FUNCTION_PG_IS_IN_RECOVERY:
			return parser.MakePgIsInRecoveryNode(node)
		}
	}

	return node
}

// FROM PG_FUNCTION(PG_NESTED_FUNCTION())
func (remapper *QueryRemapperTable) RemapNestedTableFunction(functionCall *pgQuery.FuncCall) *pgQuery.FuncCall {
	schemaFunction := remapper.parserFunction.SchemaFunction(functionCall)

	switch {

	// array_upper(values, 1) -> len(values)
	case schemaFunction.Function == PG_FUNCTION_ARRAY_UPPER:
		return remapper.parserTable.MakeArrayUpperNode(functionCall)

	default:
		return functionCall
	}
}

func (remapper *QueryRemapperTable) RemapWhereClauseForTable(qSchemaTable QuerySchemaTable, selectStatement *pgQuery.SelectStmt) *pgQuery.SelectStmt {
	if remapper.isTableFromPgCatalog(qSchemaTable) {
		switch qSchemaTable.Table {

		// FROM pg_catalog.pg_namespace -> FROM pg_catalog.pg_namespace WHERE oid > 1265
		case PG_TABLE_PG_NAMESPACE:
			alias := qSchemaTable.Alias
			if alias == "" {
				alias = PG_TABLE_PG_NAMESPACE
			}
			withoutDuckdbOidsWhereCondition := remapper.parserWhere.MakeIntEqualityExpressionNode("oid", ">", MAX_REDUNDANT_PG_NAMESPACE_OID, alias)
			remapper.parserWhere.AppendWhereCondition(selectStatement, withoutDuckdbOidsWhereCondition)
		}
	}
	return selectStatement
}

func (remapper *QueryRemapperTable) reloadIceberSchemaTables() {
	newIcebergSchemaTables, err := remapper.icebergReader.SchemaTables()
	PanicIfError(err)

	ctx := context.Background()
	for _, icebergSchemaTable := range newIcebergSchemaTables.Values() {
		if !remapper.icebergSchemaTables.Contains(icebergSchemaTable) {
			icebergTableFields, err := remapper.icebergReader.TableFields(icebergSchemaTable)
			PanicIfError(err)

			var sqlColumns []string
			for _, icebergTableField := range icebergTableFields {
				sqlColumns = append(sqlColumns, icebergTableField.ToSql())
			}

			_, err = remapper.duckdb.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+icebergSchemaTable.Schema, nil)
			PanicIfError(err)
			_, err = remapper.duckdb.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+icebergSchemaTable.String()+" ("+strings.Join(sqlColumns, ", ")+")", nil)
			PanicIfError(err)
		}
	}
	for _, icebergSchemaTable := range remapper.icebergSchemaTables.Values() {
		if !newIcebergSchemaTables.Contains(icebergSchemaTable) {
			_, err = remapper.duckdb.ExecContext(ctx, "DROP TABLE IF EXISTS "+icebergSchemaTable.String(), nil)
			PanicIfError(err)
		}
	}

	remapper.icebergSchemaTables = newIcebergSchemaTables
}

func (remapper *QueryRemapperTable) upsertPgStatUserTables(icebergSchemaTables Set[IcebergSchemaTable]) {
	values := make([]string, len(icebergSchemaTables))
	for i, icebergSchemaTable := range icebergSchemaTables.Values() {
		values[i] = "('123456', '" + icebergSchemaTable.Schema + "', '" + icebergSchemaTable.Table + "', 0, NULL, 0, 0, NULL, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, NULL, NULL, NULL, NULL, 0, 0, 0, 0)"
	}

	err := remapper.duckdb.ExecTransactionContext(context.Background(), []string{
		"DELETE FROM pg_stat_user_tables",
		"INSERT INTO pg_stat_user_tables VALUES " + strings.Join(values, ", "),
	})
	PanicIfError(err)
}

// System pg_* tables
func (remapper *QueryRemapperTable) isTableFromPgCatalog(qSchemaTable QuerySchemaTable) bool {
	return qSchemaTable.Schema == PG_SCHEMA_PG_CATALOG ||
		(qSchemaTable.Schema == "" &&
			(PG_SYSTEM_TABLES.Contains(qSchemaTable.Table) || PG_SYSTEM_VIEWS.Contains(qSchemaTable.Table)) &&
			!remapper.icebergSchemaTables.Contains(qSchemaTable.ToIcebergSchemaTable()))
}

func (remapper *QueryRemapperTable) isFunctionFromPgCatalog(schemaFunction QuerySchemaFunction) bool {
	return schemaFunction.Schema == PG_SCHEMA_PG_CATALOG ||
		(schemaFunction.Schema == "" && PG_SYSTEM_FUNCTIONS.Contains(schemaFunction.Function))
}

func extractTableNames(tables []string) Set[string] {
	names := make(Set[string])
	re := regexp.MustCompile(`CREATE (TABLE|VIEW) (\w+)`)

	for _, table := range tables {
		matches := re.FindStringSubmatch(table)
		if len(matches) > 1 {
			names.Add(matches[2])
		}
	}

	return names
}
