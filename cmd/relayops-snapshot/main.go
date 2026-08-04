package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func main() {
	source := flag.String("source", "", "source RelayOps SQLite database")
	destination := flag.String("destination", "", "new snapshot database path")
	flag.Parse()
	if *source == "" || *destination == "" {
		log.Fatal("source and destination are required")
	}
	absSource, err := filepath.Abs(*source)
	if err != nil {
		log.Fatal(err)
	}
	absDestination, err := filepath.Abs(*destination)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(absDestination); err == nil {
		log.Fatal("destination already exists")
	} else if !os.IsNotExist(err) {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(absDestination), 0o700); err != nil {
		log.Fatal(err)
	}

	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(absSource)+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("PRAGMA busy_timeout=10000"); err != nil {
		log.Fatal(err)
	}
	if _, err := database.Exec("VACUUM INTO ?", absDestination); err != nil {
		log.Fatal(err)
	}

	copyDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(absDestination)+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer copyDB.Close()
	var integrity string
	if err := copyDB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		log.Fatal(err)
	}
	if integrity != "ok" {
		log.Fatalf("snapshot integrity check failed: %s", integrity)
	}
	fmt.Printf("snapshot_ok source=%s destination=%s\n", absSource, absDestination)
}
