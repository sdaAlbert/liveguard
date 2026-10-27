package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"liveguard/internal/monitor"
)

func main() {
	databasePath := flag.String("db", "runtime/liveguard.sqlite", "SQLite database path")
	outputDir := flag.String("out", "", "new directory for JSON snapshot and observation ledger")
	flag.Parse()
	if *outputDir == "" {
		log.Fatal("-out is required; choose a new directory")
	}
	repo, err := monitor.OpenSQLiteRepository(*databasePath)
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()
	if err := repo.ExportJSON(context.Background(), *outputDir); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("JSON backup written to %s\n", *outputDir)
}
