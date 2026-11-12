package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pterm/pterm"

	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

const importPage = 1000

// RunImportHistory copies request history from a SQLite file into the
// store the config points at (normally Postgres), keeping each row's
// original timestamp. Rows are copied, never moved: the source is left
// alone. A destination that already has history is refused unless force is
// set, because running this twice would otherwise silently double every
// number on the dashboard.
func RunImportHistory(configPath, from string, force bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("run 'fitguard init' first, or check your config: %w", err)
	}
	if from == "" {
		from = filepath.Join(cfg.DataDir, "fitguard.db")
	}
	if _, err := os.Stat(from); err != nil {
		return fmt.Errorf("no SQLite history at %q (pass --from <path to fitguard.db>)", from)
	}

	src, err := logging.Open(from)
	if err != nil {
		return fmt.Errorf("opening source %q: %w", from, err)
	}
	defer src.Close()

	var dst *logging.Store
	if cfg.Storage.IsPostgres() {
		dst, err = logging.OpenPostgres(cfg.Storage.PostgresURL)
		if err != nil {
			return fmt.Errorf("connecting to postgres at %s: %w", maskDBURL(cfg.Storage.PostgresURL), err)
		}
	} else {
		dstPath := filepath.Join(cfg.DataDir, "fitguard.db")
		if a, _ := filepath.Abs(dstPath); a == mustAbs(from) {
			return fmt.Errorf("the source and the destination are the same file (%s); point storage.backend at postgres in %s first", from, configPath)
		}
		dst, err = logging.Open(dstPath)
		if err != nil {
			return err
		}
	}
	defer dst.Close()

	ctx := context.Background()
	have, err := dst.CountRequests(ctx)
	if err != nil {
		return err
	}
	if have > 0 && !force {
		return fmt.Errorf("the destination already has %d request rows; importing again would double-count them. Use --force only if you know it is a different history", have)
	}
	total, err := src.CountRequests(ctx)
	if err != nil {
		return err
	}

	var after, copied int64
	for {
		page, err := src.RecordsAfter(ctx, after, importPage)
		if err != nil {
			return fmt.Errorf("reading the source after row %d: %w", after, err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if _, err := dst.Insert(ctx, r); err != nil {
				return fmt.Errorf("copied %d of %d rows, then failed on source row %d: %w", copied, total, r.ID, err)
			}
			copied++
			after = r.ID
		}
	}
	pterm.Success.Printfln("Copied %d of %d request rows. The source file was not changed.", copied, total)
	return nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
