package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestStore_ReasoningSchemaUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int64
		private bool
	}{
		{"original", 1, false},
		{"upstream source", 2, false},
		{"upstream cache", 3, false},
		{"deployed private reasoning", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "activity.sqlite")
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { db.Close() })
			db.SetMaxOpenConns(1)
			migrations, err := fs.Sub(migrationFS, "migrations")
			require.NoError(t, err)
			provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations)
			require.NoError(t, err)
			version := tc.version
			if tc.private {
				version = 1
			}
			_, err = provider.UpTo(ctx, version)
			require.NoError(t, err)
			if tc.private {
				_, err = db.ExecContext(ctx, `
					ALTER TABLE activity RENAME COLUMN output_tokens TO generated_tokens;
					ALTER TABLE activity ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
					ALTER TABLE activity ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
					INSERT INTO goose_db_version (version_id, is_applied) VALUES (2, 1);
					INSERT INTO activity (ts_created, model_id, generated_tokens, reasoning_tokens, output_tokens)
					VALUES (1, 'legacy', 42, 12, 30);
				`)
			} else {
				_, err = db.ExecContext(ctx, `
					INSERT INTO activity (ts_created, model_id, output_tokens)
					VALUES (1, 'legacy', 42);
				`)
			}
			require.NoError(t, err)
			if !tc.private && tc.version >= 2 {
				_, err = db.ExecContext(ctx, "UPDATE activity SET src = 'ip:127.0.0.1'")
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())

			// Open twice to catch accidental replay of an applied migration.
			for range 2 {
				st, err := New(path)
				require.NoError(t, err)
				page, err := st.Activity().List(ctx, store.ActivityQuery{})
				require.NoError(t, err)
				require.Len(t, page.Data, 1)
				tokens := page.Data[0].Tokens
				require.Equal(t, 42, tokens.GeneratedTokens)
				if tc.private {
					require.Equal(t, 12, tokens.ReasoningTokens)
					require.Equal(t, 30, tokens.OutputTokens)
				} else {
					require.Zero(t, tokens.ReasoningTokens)
					require.Equal(t, 42, tokens.OutputTokens)
				}
				if !tc.private && tc.version >= 2 {
					require.Equal(t, "ip:127.0.0.1", page.Data[0].Src)
				}
				_, _, err = st.Cache().Get(ctx, "missing")
				require.NoError(t, err)
				require.NoError(t, st.Close())
			}
		})
	}
}

func TestStore_UpdateReasoningTokens(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	entry, err := st.Activity().Insert(ctx, store.ActivityLogEntry{
		Model:    "test",
		Src:      "ip:127.0.0.1",
		Tokens:   store.TokenMetrics{GeneratedTokens: 10, OutputTokens: 10},
		Metadata: map[string]string{"trace": "abc"},
	})
	require.NoError(t, err)
	for _, count := range []int{3, 99, -2} {
		updated, found, err := st.Activity().UpdateReasoningTokens(ctx, entry.ID, count)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, 10, updated.Tokens.GeneratedTokens)
		require.Equal(t, min(max(count, 0), 10), updated.Tokens.ReasoningTokens)
		require.Equal(t, 10-min(max(count, 0), 10), updated.Tokens.OutputTokens)
		require.Equal(t, entry.Src, updated.Src)
		require.Equal(t, entry.Metadata, updated.Metadata)
	}
	_, found, err := st.Activity().UpdateReasoningTokens(ctx, entry.ID+1, 1)
	require.NoError(t, err)
	require.False(t, found)
	stats, err := st.Activity().Stats(ctx, store.ActivityStatsQuery{})
	require.NoError(t, err)
	require.Equal(t, 10, stats.TotalGeneratedTokens)
	require.Zero(t, stats.TotalReasoningTokens)
	require.Equal(t, 10, stats.TotalOutputTokens)
}
