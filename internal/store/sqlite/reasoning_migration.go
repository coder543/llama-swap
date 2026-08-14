package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// Version 2 was independently used by the private reasoning schema and by
// upstream's source column. Leave versions 1-3 intact and reconcile both
// histories in version 4. The transaction preserves existing private counts
// and backfills unsplit upstream totals as visible output.
func migrateReasoningTokens(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info('activity')")
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	if !columns["generated_tokens"] {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE activity RENAME COLUMN output_tokens TO generated_tokens;
			ALTER TABLE activity ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
			ALTER TABLE activity ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
			UPDATE activity SET output_tokens = generated_tokens;
		`); err != nil {
			return fmt.Errorf("add reasoning split: %w", err)
		}
	} else if !columns["reasoning_tokens"] || !columns["output_tokens"] {
		return fmt.Errorf("incomplete private reasoning schema")
	}

	// An existing private database already recorded version 2 as applied, so
	// Goose correctly skipped the upstream source migration with that number.
	if !columns["src"] {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE activity ADD COLUMN src TEXT NOT NULL DEFAULT '';
			CREATE INDEX idx_activity_src_created_id
				ON activity (src, ts_created DESC, id DESC);
		`); err != nil {
			return fmt.Errorf("add activity source: %w", err)
		}
	}
	return nil
}
