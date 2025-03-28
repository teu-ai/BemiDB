package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"time"
)

func main() {
	config := LoadConfig()

	if len(flag.Args()) == 0 {
		start(config)
		return
	}

	command := flag.Arg(0)

	if false {
		// Manually enable profiling with ppfrof
		go func() { log.Println(http.ListenAndServe(":6060", nil)) }()
	}

	switch command {
	case "start":
		start(config)
	case "sync":
		if config.Pg.SyncInterval != "" {
			duration, err := time.ParseDuration(config.Pg.SyncInterval)
			if err != nil {
				panic("Invalid interval format: " + config.Pg.SyncInterval)
			}
			LogInfo(config, "Starting sync loop with interval:", config.Pg.SyncInterval)
			for {
				syncFromPg(config)
				LogInfo(config, "Sleeping for", config.Pg.SyncInterval)
				time.Sleep(duration)
			}
		} else {
			syncFromPg(config)
		}
	case "version":
		fmt.Println("BemiDB version:", VERSION)
	default:
		panic("Unknown command: " + command)
	}
}

func start(config *Config) {
	tcpListener := NewTcpListener(config)
	LogInfo(config, "BemiDB: Listening on", tcpListener.Addr())

	duckdb := NewDuckdb(config, true)
	LogInfo(config, "DuckDB: Connected")
	defer duckdb.Close()

	icebergReader := NewIcebergReader(config)
	queryHandler := NewQueryHandler(config, duckdb, icebergReader)

	for {
		conn := AcceptConnection(config, tcpListener)
		LogInfo(config, "BemiDB: Accepted connection from", conn.RemoteAddr())
		postgres := NewPostgres(config, &conn)

		go func() {
			postgres.Run(queryHandler)
			defer postgres.Close()
			LogInfo(config, "BemiDB: Closed connection from", conn.RemoteAddr())
		}()
	}
}

func syncFromPg(config *Config) {
	syncer := NewSyncer(config)
	syncer.SyncFromPostgres()
	LogInfo(config, "Sync from PostgreSQL completed successfully.")
}
