package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	MAX_IN_MEMORY_BUFFER_SIZE = 128 * 1024 * 1024 // 128 MB (expands to ~160 MB memory usage)
	MAX_PG_ROWS_BATCH_SIZE    = 1 * 1024 * 1024   // 1 MB
	PING_PG_INTERVAL_SECONDS  = 24
)

type Syncer struct {
	config                   *Config
	icebergWriter            *IcebergWriter
	icebergReader            *IcebergReader
	syncerFullRefresh        *SyncerFullRefresh
	syncerIncrementalRefresh *SyncerIncrementalRefresh
}

func NewSyncer(config *Config) *Syncer {
	if config.Pg.DatabaseUrl == "" {
		panic("Missing PostgreSQL database URL")
	}

	icebergWriter := NewIcebergWriter(config)
	icebergReader := NewIcebergReader(config)
	return &Syncer{
		config:                   config,
		icebergWriter:            icebergWriter,
		icebergReader:            icebergReader,
		syncerFullRefresh:        NewSyncerFullRefresh(config, icebergWriter),
		syncerIncrementalRefresh: NewSyncerIncrementalRefresh(config, icebergWriter),
	}
}

func (syncer *Syncer) SyncFromPostgres() {
	ctx := context.Background()
	databaseUrl := syncer.urlEncodePassword(syncer.config.Pg.DatabaseUrl)
	syncer.sendAnonymousAnalytics(databaseUrl)

	icebergSchemaTables, icebergSchemaTablesErr := syncer.icebergReader.SchemaTables()

	structureConn := syncer.newConnection(ctx, databaseUrl)
	defer structureConn.Close(ctx)

	copyConn := syncer.newConnection(ctx, databaseUrl)
	defer copyConn.Close(ctx)

	syncedPgSchemaTables := []PgSchemaTable{}

	for _, schema := range syncer.listPgSchemas(structureConn) {
		for _, pgSchemaTable := range syncer.listPgSchemaTables(structureConn, schema) {
			if syncer.shouldSyncTable(pgSchemaTable) {
				// Identify the batch size dynamically based on the table stats
				rowCountPerBatch := syncer.calculateRowCountPerBatch(pgSchemaTable, structureConn)
				LogDebug(syncer.config, "Row count per batch:", rowCountPerBatch)

				syncedPreviously := icebergSchemaTablesErr == nil && icebergSchemaTables.Contains(pgSchemaTable.ToIcebergSchemaTable())
				incrementalRefreshEnabled := syncer.config.Pg.IncrementallyRefreshedTables != nil && HasExactOrWildcardMatch(syncer.config.Pg.IncrementallyRefreshedTables, pgSchemaTable.ToConfigArg())

				var internalTableMetadata InternalTableMetadata
				var err error
				// Read internal table metadata if it exists
				if syncedPreviously && incrementalRefreshEnabled {
					internalTableMetadata, err = syncer.icebergReader.storage.InternalTableMetadata(pgSchemaTable)
					PanicIfError(err, syncer.config)
					LogDebug(syncer.config, "Read internal table metadata to sync incrementally:", internalTableMetadata.String())
				}

				// Sync the table
				if internalTableMetadata.XminMax != nil && internalTableMetadata.XminMin != nil {
					syncer.syncerIncrementalRefresh.SyncPgTable(pgSchemaTable, internalTableMetadata, rowCountPerBatch, structureConn, copyConn)
				} else {
					syncer.syncerFullRefresh.SyncPgTable(pgSchemaTable, rowCountPerBatch, structureConn, copyConn)
				}

				LogDebug(syncer.config, "Writing internal metadata to Iceberg...")
				syncer.writeInternalMetadata(pgSchemaTable, structureConn)

				LogInfo(syncer.config, "Finished writing to Iceberg\n")
				syncedPgSchemaTables = append(syncedPgSchemaTables, pgSchemaTable)
			}
		}
	}

	if syncer.config.Pg.SchemaPrefix == "" {
		syncer.deleteOldIcebergSchemaTables(syncedPgSchemaTables)
	}
}

// Example:
// - From postgres://username:pas$:wor^d@host:port/database
// - To postgres://username:pas%24%3Awor%5Ed@host:port/database
func (syncer *Syncer) urlEncodePassword(databaseUrl string) string {
	// No credentials
	if !strings.Contains(databaseUrl, "@") {
		return databaseUrl
	}

	password := strings.TrimPrefix(databaseUrl, "postgresql://")
	password = strings.TrimPrefix(password, "postgres://")
	passwordEndIndex := strings.LastIndex(password, "@")
	password = password[:passwordEndIndex]

	// Credentials without password
	if !strings.Contains(password, ":") {
		return databaseUrl
	}

	_, password, _ = strings.Cut(password, ":")
	decodedPassword, err := url.QueryUnescape(password)
	if err != nil {
		return databaseUrl
	}

	// Password is already encoded
	if decodedPassword != password {
		return databaseUrl
	}

	return strings.Replace(databaseUrl, ":"+password+"@", ":"+url.QueryEscape(password)+"@", 1)
}

func (syncer *Syncer) shouldSyncTable(pgSchemaTable PgSchemaTable) bool {
	if syncer.config.Pg.ExcludeTables != nil && HasExactOrWildcardMatch(syncer.config.Pg.ExcludeTables, pgSchemaTable.ToConfigArg()) {
		return false
	}

	if syncer.config.Pg.IncludeTables != nil {
		return HasExactOrWildcardMatch(syncer.config.Pg.IncludeTables, pgSchemaTable.ToConfigArg())
	}

	return true
}

func (syncer *Syncer) listPgSchemas(conn *pgx.Conn) []string {
	var schemas []string

	schemasRows, err := conn.Query(
		context.Background(),
		"SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('pg_catalog', 'pg_toast', 'information_schema')",
	)
	PanicIfError(err, syncer.config)
	defer schemasRows.Close()

	for schemasRows.Next() {
		var schema string
		err = schemasRows.Scan(&schema)
		PanicIfError(err, syncer.config)
		schemas = append(schemas, schema)
	}

	return schemas
}

func (syncer *Syncer) listPgSchemaTables(conn *pgx.Conn, schema string) []PgSchemaTable {
	var pgSchemaTables []PgSchemaTable

	tablesRows, err := conn.Query(
		context.Background(),
		`
		SELECT pg_class.relname AS table, COALESCE(parent.relname, '') AS parent_partitioned_table
		FROM pg_class
		JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
		LEFT JOIN pg_inherits ON pg_inherits.inhrelid = pg_class.oid
		LEFT JOIN pg_class AS parent ON pg_inherits.inhparent = parent.oid
		WHERE pg_namespace.nspname = $1 AND pg_class.relkind = 'r';
		`,
		schema,
	)
	PanicIfError(err, syncer.config)
	defer tablesRows.Close()

	for tablesRows.Next() {
		pgSchemaTable := PgSchemaTable{Schema: schema}
		err = tablesRows.Scan(&pgSchemaTable.Table, &pgSchemaTable.ParentPartitionedTable)
		PanicIfError(err, syncer.config)
		pgSchemaTables = append(pgSchemaTables, pgSchemaTable)
	}

	return pgSchemaTables
}

func (syncer *Syncer) calculateRowCountPerBatch(pgSchemaTable PgSchemaTable, conn *pgx.Conn) int {
	var tableSize int64
	var rowCount int64

	err := conn.QueryRow(
		context.Background(),
		`
		SELECT
			pg_total_relation_size(c.oid) AS table_size,
			CASE
				WHEN c.reltuples >= 0 THEN c.reltuples::bigint
				ELSE (SELECT count(*) FROM `+pgSchemaTable.String()+`)
			END AS row_count
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'r'`,
		pgSchemaTable.Schema,
		pgSchemaTable.Table,
	).Scan(&tableSize, &rowCount)
	PanicIfError(err, syncer.config)
	LogDebug(syncer.config, "Table size:", tableSize, "Row count:", rowCount)

	if tableSize == 0 || rowCount == 0 {
		return 1
	}

	rowSize := tableSize / rowCount
	rowCountPerBatch := int(MAX_PG_ROWS_BATCH_SIZE / rowSize)
	if rowCountPerBatch == 0 {
		return 1
	}

	return rowCountPerBatch
}

func (syncer *Syncer) newConnection(ctx context.Context, databaseUrl string) *pgx.Conn {
	conn, err := pgx.Connect(ctx, databaseUrl)
	PanicIfError(err, syncer.config)

	_, err = conn.Exec(ctx, "BEGIN TRANSACTION ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE")
	PanicIfError(err, syncer.config)

	return conn
}

func (syncer *Syncer) deleteOldIcebergSchemaTables(pgSchemaTables []PgSchemaTable) {
	var prefixedPgSchemaTables []PgSchemaTable
	for _, pgSchemaTable := range pgSchemaTables {
		prefixedPgSchemaTables = append(
			prefixedPgSchemaTables,
			PgSchemaTable{Schema: syncer.config.Pg.SchemaPrefix + pgSchemaTable.Schema, Table: pgSchemaTable.Table},
		)
	}

	icebergSchemas, err := syncer.icebergReader.Schemas()
	PanicIfError(err, syncer.config)

	for _, icebergSchema := range icebergSchemas {
		found := false
		for _, pgSchemaTable := range prefixedPgSchemaTables {
			if icebergSchema == pgSchemaTable.Schema {
				found = true
				break
			}
		}

		if !found {
			LogInfo(syncer.config, "Deleting", icebergSchema, "...")
			syncer.icebergWriter.DeleteSchema(icebergSchema)
		}
	}

	icebergSchemaTables, err := syncer.icebergReader.SchemaTables()
	PanicIfError(err, syncer.config)

	for _, icebergSchemaTable := range icebergSchemaTables.Values() {
		found := false
		for _, pgSchemaTable := range prefixedPgSchemaTables {
			if icebergSchemaTable.String() == pgSchemaTable.String() {
				found = true
				break
			}
		}

		if !found {
			LogInfo(syncer.config, "Deleting", icebergSchemaTable.String(), "...")
			syncer.icebergWriter.DeleteSchemaTable(icebergSchemaTable)
		}
	}
}

func (syncer *Syncer) writeInternalMetadata(pgSchemaTable PgSchemaTable, conn *pgx.Conn) {
	var xminMax *uint32
	var xminMin *uint32

	err := conn.QueryRow(
		context.Background(),
		"SELECT xmin FROM "+pgSchemaTable.String()+" ORDER BY age(xmin) ASC LIMIT 1",
	).Scan(&xminMax)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		PanicIfError(err, syncer.config)
	}

	err = conn.QueryRow(
		context.Background(),
		"SELECT xmin FROM "+pgSchemaTable.String()+" ORDER BY age(xmin) DESC LIMIT 1",
	).Scan(&xminMin)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		PanicIfError(err, syncer.config)
	}

	metadata := InternalTableMetadata{
		LastSyncedAt: time.Now().Unix(),
		XminMax:      xminMax,
		XminMin:      xminMin,
	}
	err = syncer.icebergWriter.storage.WriteInternalTableMetadata(pgSchemaTable, metadata)
	PanicIfError(err, syncer.config)
}

type AnonymousAnalyticsData struct {
	DbHost  string `json:"dbHost"`
	OsName  string `json:"osName"`
	Version string `json:"version"`
}

func (syncer *Syncer) sendAnonymousAnalytics(databaseUrl string) {
	if syncer.config.DisableAnonymousAnalytics {
		LogInfo(syncer.config, "Anonymous analytics is disabled")
		return
	}

	dbUrl, err := url.Parse(databaseUrl)
	if err != nil {
		return
	}

	hostname := dbUrl.Hostname()
	switch hostname {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return
	}

	data := AnonymousAnalyticsData{
		DbHost:  hostname,
		OsName:  runtime.GOOS + "-" + runtime.GOARCH,
		Version: syncer.config.Version,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}

	client := http.Client{Timeout: 5 * time.Second}
	_, _ = client.Post("https://api.bemidb.com/api/analytics", "application/json", bytes.NewBuffer(jsonData))
}
