// Command hashmigrations recomputes the atlas.sum checksum file for the
// versioned migration directory offline (no DB / no atlas CLI required).
// Run after hand-adding a migration .sql file: go run ./cmd/hashmigrations
package main

import (
	"log"

	atlasmigrate "ariga.io/atlas/sql/migrate"
)

func main() {
	dir, err := atlasmigrate.NewLocalDir("internal/ent/migrate/migrations")
	if err != nil {
		log.Fatalf("open migrations dir: %v", err)
	}
	sum, err := dir.Checksum()
	if err != nil {
		log.Fatalf("compute checksum: %v", err)
	}
	if err := atlasmigrate.WriteSumFile(dir, sum); err != nil {
		log.Fatalf("write atlas.sum: %v", err)
	}
	log.Println("atlas.sum regenerated successfully")
}
