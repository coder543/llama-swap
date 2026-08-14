package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestStore_InsertListAndFilterActivity(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	for i, model := range []string{"m1", "m2", "m1"} {
		_, err := store.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: time.Unix(int64(100+i), 0),
			Model:     model,
			ReqPath:   "/v1/chat/completions",
			Tokens: TokenMetrics{
				InputTokens:     i + 1,
				GeneratedTokens: i + 2,
				OutputTokens:    i + 2,
				PromptPerSecond: float64(10 + i),
				TokensPerSecond: float64(20 + i),
			},
			Metadata: map[string]string{"trace": model},
		})
		if err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	page, err := store.ListActivity(ctx, ActivityQuery{Limit: 2, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if page.Total != 3 || page.TotalPages != 2 || len(page.Data) != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Data[0].ID <= page.Data[1].ID {
		t.Fatalf("activity is not newest first: %+v", page.Data)
	}
	if page.Data[0].Metadata["trace"] != "m1" {
		t.Fatalf("metadata = %+v", page.Data[0].Metadata)
	}

	filtered, err := store.ListActivity(ctx, ActivityQuery{Model: "m1", Limit: 10, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity filtered: %v", err)
	}
	if filtered.Total != 2 || len(filtered.Data) != 2 {
		t.Fatalf("filtered page = %+v", filtered)
	}
	for _, entry := range filtered.Data {
		if entry.Model != "m1" {
			t.Fatalf("filtered model = %q", entry.Model)
		}
	}
}

func TestStore_ListActivitySort(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	// Insert rows whose generated_tokens ordering differs from insertion (id) order.
	outputs := []int{30, 10, 20}
	for i, out := range outputs {
		if _, err := store.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: time.Unix(int64(100+i), 0),
			Model:     "m1",
			Tokens:    TokenMetrics{GeneratedTokens: out, OutputTokens: out},
		}); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	asc, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1, Sort: "generated", Order: "asc"})
	if err != nil {
		t.Fatalf("ListActivity asc: %v", err)
	}
	gotAsc := []int{}
	for _, e := range asc.Data {
		gotAsc = append(gotAsc, e.Tokens.GeneratedTokens)
	}
	if len(gotAsc) != 3 || gotAsc[0] != 10 || gotAsc[1] != 20 || gotAsc[2] != 30 {
		t.Fatalf("ascending generated sort = %v", gotAsc)
	}

	desc, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1, Sort: "generated", Order: "desc"})
	if err != nil {
		t.Fatalf("ListActivity desc: %v", err)
	}
	gotDesc := []int{}
	for _, e := range desc.Data {
		gotDesc = append(gotDesc, e.Tokens.GeneratedTokens)
	}
	if len(gotDesc) != 3 || gotDesc[0] != 30 || gotDesc[1] != 20 || gotDesc[2] != 10 {
		t.Fatalf("descending generated sort = %v", gotDesc)
	}

	// Unknown sort keys fall back to id ordering (newest first).
	fallback, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1, Sort: "bogus"})
	if err != nil {
		t.Fatalf("ListActivity fallback: %v", err)
	}
	if fallback.Data[0].ID <= fallback.Data[len(fallback.Data)-1].ID {
		t.Fatalf("fallback sort is not newest first: %+v", fallback.Data)
	}
}

func TestStore_ActivityStats(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	entries := []ActivityLogEntry{
		{
			Timestamp: time.Unix(1, 0),
			Model:     "m1",
			Tokens: TokenMetrics{
				CachedTokens:    2,
				InputTokens:     10,
				GeneratedTokens: 20,
				ReasoningTokens: 5,
				OutputTokens:    15,
				PromptPerSecond: 100,
				TokensPerSecond: 50,
			},
		},
		{
			Timestamp: time.Unix(2, 0),
			Model:     "m1",
			Tokens: TokenMetrics{
				CachedTokens:    -1,
				InputTokens:     5,
				GeneratedTokens: 8,
				ReasoningTokens: 1,
				OutputTokens:    7,
				PromptPerSecond: 200,
				TokensPerSecond: 100,
			},
		},
		{
			Timestamp: time.Unix(3, 0),
			Model:     "m2",
			Tokens: TokenMetrics{
				InputTokens:     7,
				GeneratedTokens: 9,
				OutputTokens:    9,
				PromptPerSecond: 300,
			},
		},
	}
	for _, entry := range entries {
		if _, err := store.InsertActivity(ctx, entry); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}

	stats, err := store.ActivityStats(ctx, ActivityStatsQuery{Model: "m1"})
	if err != nil {
		t.Fatalf("ActivityStats: %v", err)
	}
	if stats.TotalRequests != 2 || stats.TotalInputTokens != 15 ||
		stats.TotalGeneratedTokens != 28 || stats.TotalReasoningTokens != 6 ||
		stats.TotalOutputTokens != 22 || stats.TotalCacheTokens != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.PromptHistogram == nil || stats.GenerationHistogram == nil {
		t.Fatalf("expected histograms: %+v", stats)
	}
}

func TestStore_UpdateActivityReasoningTokens(t *testing.T) {
	ctx := context.Background()
	activityStore, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer activityStore.Close()

	inserted, err := activityStore.InsertActivity(ctx, ActivityLogEntry{
		Timestamp: time.Unix(1, 0),
		Model:     "m",
		Tokens: TokenMetrics{
			GeneratedTokens: 10,
			OutputTokens:    10,
		},
		Metadata: map[string]string{"trace": "abc"},
	})
	if err != nil {
		t.Fatalf("InsertActivity: %v", err)
	}

	updated, found, err := activityStore.UpdateActivityReasoningTokens(ctx, inserted.ID, 3)
	if err != nil {
		t.Fatalf("UpdateActivityReasoningTokens: %v", err)
	}
	if !found {
		t.Fatal("updated activity was not found")
	}
	if updated.Tokens.GeneratedTokens != 10 || updated.Tokens.ReasoningTokens != 3 || updated.Tokens.OutputTokens != 7 {
		t.Fatalf("tokens = %+v", updated.Tokens)
	}
	if updated.Metadata["trace"] != "abc" {
		t.Fatalf("metadata = %+v", updated.Metadata)
	}

	updated, found, err = activityStore.UpdateActivityReasoningTokens(ctx, inserted.ID, 99)
	if err != nil || !found {
		t.Fatalf("clamped update: found=%v err=%v", found, err)
	}
	if updated.Tokens.ReasoningTokens != 10 || updated.Tokens.OutputTokens != 0 {
		t.Fatalf("clamped tokens = %+v", updated.Tokens)
	}

	if _, found, err := activityStore.UpdateActivityReasoningTokens(ctx, inserted.ID+100, 1); err != nil || found {
		t.Fatalf("missing update: found=%v err=%v", found, err)
	}
}

func TestStore_MigrationReasoningTokenBackfill(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "activity.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("migration filesystem: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations)
	if err != nil {
		t.Fatalf("new migration provider: %v", err)
	}
	if _, err := provider.UpTo(ctx, 1); err != nil {
		t.Fatalf("migrate to version 1: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO activity (ts_created, model_id, output_tokens)
		VALUES (1, 'legacy', 42)`); err != nil {
		t.Fatalf("insert legacy activity: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}

	var generated, reasoning, output int
	if err := db.QueryRowContext(ctx, `
		SELECT generated_tokens, reasoning_tokens, output_tokens
		FROM activity WHERE model_id = 'legacy'`).Scan(&generated, &reasoning, &output); err != nil {
		t.Fatalf("read migrated activity: %v", err)
	}
	if generated != 42 || reasoning != 0 || output != 42 {
		t.Fatalf("migrated tokens = generated:%d reasoning:%d output:%d", generated, reasoning, output)
	}
}

func TestStore_PruneActivity(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	for i := 0; i < 5; i++ {
		if _, err := store.InsertActivity(ctx, ActivityLogEntry{Timestamp: time.Unix(int64(i), 0), Model: "m"}); err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
	}
	if err := store.PruneActivity(ctx, 2); err != nil {
		t.Fatalf("PruneActivity: %v", err)
	}
	page, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Data[0].ID != 5 || page.Data[1].ID != 4 {
		t.Fatalf("kept IDs = %+v", page.Data)
	}
}

func TestStore_NewFilePersistsActivity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "llama-swap.sqlite")

	store, err := New(path)
	if err != nil {
		t.Fatalf("New file store: %v", err)
	}
	if _, err := store.InsertActivity(ctx, ActivityLogEntry{Timestamp: time.Unix(1, 0), Model: "m"}); err != nil {
		t.Fatalf("InsertActivity: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store, err = New(path)
	if err != nil {
		t.Fatalf("reopen file store: %v", err)
	}
	defer store.Close()
	page, err := store.ListActivity(ctx, ActivityQuery{Limit: 10, Page: 1})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if page.Total != 1 || len(page.Data) != 1 || page.Data[0].Model != "m" {
		t.Fatalf("page = %+v", page)
	}
}

func TestStore_NewFileUsesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llama-swap.sqlite")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New file store: %v", err)
	}
	defer store.Close()

	var mode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}
