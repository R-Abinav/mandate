// Mandate-verify is a standalone CLI that connects to the audit store and
// walks the full hash chain, reporting whether it is intact or naming the
// specific entry where it broke.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/R-Abinav/mandate/internal/audit"
	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/logging"
	_ "github.com/lib/pq"
)

func main() {
	logger := logging.New(config.Load().LogLevel)
	if err := run(); err != nil {
		logger.Error("mandate-verify: fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		return errors.New("mandate-verify: DATABASE_URL is required")
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("mandate-verify: failed to open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("mandate-verify: database unreachable: %w", err)
	}

	store := audit.NewPostgresStore(db)

	ctx := context.Background()
	entries, err := store.All(ctx)
	if err != nil {
		return fmt.Errorf("mandate-verify: failed to load chain: %w", err)
	}

	ok, broken, err := audit.Verify(ctx, store)
	if err != nil {
		return fmt.Errorf("mandate-verify: %w", err)
	}

	if ok {
		fmt.Printf("%d entries verified, chain intact\n", len(entries))
		return nil
	}

	fmt.Printf(
		"CHAIN BROKEN at entry %d (type=%s, created_at=%s): %s\n",
		broken.Entry.ID, broken.Entry.EntryType, broken.Entry.CreatedAt, broken.Reason,
	)
	return errors.New("mandate-verify: chain integrity check failed")
}
