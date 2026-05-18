package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/storage"
)

// nukePreservedTables are tables that hold infrastructure/schema state — they
// stay intact across a nuke so the database remains usable afterward.
// `metadata` holds _project_id / clone_id / repo_id and is preserved unless
// --full is passed.
var nukePreservedTables = map[string]struct{}{
	"schema_migrations":         {},
	"ignored_schema_migrations": {},
	"local_metadata":            {},
	"config":                    {},
	"metadata":                  {},
}

var nukeForce bool
var nukeFull bool

var nukeCmd = &cobra.Command{
	Use:     "nuke",
	GroupID: "maint",
	Short:   "WIPE all issues, deps, comments, events, wisps (schema/config preserved)",
	Long: `Permanently delete every row from the user-data tables: issues, dependencies,
comments, events, labels, custom_*, wisps and their satellites, routes,
interactions, federation_peers, snapshots, counters. Views (blocked_issues,
ready_issues) are derived from base tables and clear automatically.

Preserved: schema_migrations, ignored_schema_migrations, local_metadata, config.

This is irreversible. Default behavior asks for confirmation ("type DELETE").
Use --force to skip the prompt.

Examples:
  bd nuke              # interactive — type DELETE to proceed
  bd nuke --force      # no prompt — for scripts
  bd nuke --dry-run    # show what would be wiped, do nothing`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("nuke")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		// --full also wipes the metadata table (project_id, repo_id, clone_id).
		// Drops the entry from preserved set so listWipeableTables includes it.
		if nukeFull {
			delete(nukePreservedTables, "metadata")
		}

		if !usesSQLServer() {
			FatalError("bd nuke requires server-mode dolt (use --dolt-server)")
		}
		if store == nil {
			if err := ensureStoreActive(); err != nil {
				FatalError("%v", err)
			}
		}
		accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
		if !ok {
			FatalError("storage backend does not support raw DB access")
		}
		db := accessor.UnderlyingDB()
		if db == nil {
			FatalError("underlying database not available")
		}

		ctx := rootCtx
		tables, err := listWipeableTables(ctx, db)
		if err != nil {
			FatalError("list tables: %v", err)
		}
		if len(tables) == 0 {
			fmt.Println("nothing to nuke")
			return
		}

		counts, totalRows, err := countRows(ctx, db, tables)
		if err != nil {
			FatalError("count rows: %v", err)
		}

		fmt.Fprintf(os.Stderr, "Will wipe %d tables / %d rows:\n", len(tables), totalRows)
		for _, t := range tables {
			fmt.Fprintf(os.Stderr, "  %-30s %8d rows\n", t, counts[t])
		}
		fmt.Fprintln(os.Stderr, "Preserved:")
		for t := range nukePreservedTables {
			fmt.Fprintf(os.Stderr, "  %s\n", t)
		}

		if dryRun {
			fmt.Fprintln(os.Stderr, "(dry-run — no changes made)")
			return
		}

		if !nukeForce {
			fmt.Fprint(os.Stderr, "\nThis is IRREVERSIBLE. Type 'DELETE' to confirm: ")
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			if strings.TrimSpace(input) != "DELETE" {
				FatalError("aborted")
			}
		}

		wiped, err := wipeTables(ctx, db, tables)
		if err != nil {
			FatalError("nuke failed: %v", err)
		}
		fmt.Printf("✓ nuked %d rows across %d tables\n", wiped, len(tables))
	},
}

func init() {
	nukeCmd.Flags().BoolVarP(&nukeForce, "force", "f", false, "skip confirmation prompt")
	nukeCmd.Flags().Bool("dry-run", false, "show what would be wiped, do nothing")
	nukeCmd.Flags().BoolVar(&nukeFull, "full", false, "also wipe metadata table (project_id, repo_id, clone_id)")
	rootCmd.AddCommand(nukeCmd)
}

func listWipeableTables(ctx context.Context, db *sql.DB) ([]string, error) {
	// SHOW FULL TABLES returns (name, table_type) — table_type is "BASE TABLE"
	// or "VIEW". We skip views (e.g., blocked_issues, ready_issues) because
	// they don't support DELETE FROM.
	rows, err := db.QueryContext(ctx, "SHOW FULL TABLES")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name, tableType string
		if err := rows.Scan(&name, &tableType); err != nil {
			return nil, err
		}
		if !strings.EqualFold(tableType, "BASE TABLE") {
			continue
		}
		if _, preserved := nukePreservedTables[name]; preserved {
			continue
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(tables)
	return tables, nil
}

func countRows(ctx context.Context, db *sql.DB, tables []string) (map[string]int64, int64, error) {
	counts := make(map[string]int64, len(tables))
	var total int64
	for _, t := range tables {
		var n int64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`", t)).Scan(&n); err != nil {
			return nil, 0, fmt.Errorf("count %s: %w", t, err)
		}
		counts[t] = n
		total += n
	}
	return counts, total, nil
}

func wipeTables(ctx context.Context, db *sql.DB, tables []string) (int64, error) {
	var total int64
	for _, t := range tables {
		res, err := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM `%s`", t))
		if err != nil {
			return total, fmt.Errorf("delete from %s: %w", t, err)
		}
		if rows, err := res.RowsAffected(); err == nil {
			total += rows
		}
	}
	return total, nil
}
