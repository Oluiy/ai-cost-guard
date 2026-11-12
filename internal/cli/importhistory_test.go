package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

func writeImportConfig(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "port: 8787\ndata_dir: " + dataDir + "\nproviders:\n  openai:\n    api_key: sk-test\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportHistory_CopiesRowsKeepsTimestampsAndRefusesToDoubleCount(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "old.db")
	src, err := logging.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for i := 0; i < 2500; i++ { // more than one page
		if _, err := src.Insert(ctx, logging.Record{Timestamp: when.Add(time.Duration(i) * time.Second), UserID: "alice", Model: "gpt-4o",
			PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.01, Kind: "chat", StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	src.Close()

	dataDir := t.TempDir()
	cfgPath := writeImportConfig(t, dataDir)
	if err := RunImportHistory(cfgPath, srcPath, false); err != nil {
		t.Fatal(err)
	}
	dst, err := logging.Open(filepath.Join(dataDir, "fitguard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if n, _ := dst.CountRequests(ctx); n != 2500 {
		t.Fatalf("copied %d rows, want 2500", n)
	}
	first, err := dst.RecordsAfter(ctx, 0, 1)
	if err != nil || len(first) != 1 || !first[0].Timestamp.Equal(when) {
		t.Fatalf("first row %+v err=%v, want timestamp %v kept", first, err, when)
	}

	err = RunImportHistory(cfgPath, srcPath, false)
	if err == nil || !strings.Contains(err.Error(), "double-count") {
		t.Fatalf("a second import must be refused, got %v", err)
	}
	if n, _ := dst.CountRequests(ctx); n != 2500 {
		t.Fatalf("refused import still changed the destination: %d rows", n)
	}
}

func TestImportHistory_MissingSourceIsNotCreated(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.db")
	err := RunImportHistory(writeImportConfig(t, t.TempDir()), missing, false)
	if err == nil {
		t.Fatal("expected an error for a missing source")
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatal("the import created an empty source file")
	}
}

func TestImportHistory_SameFileIsRefused(t *testing.T) {
	dataDir := t.TempDir()
	same := filepath.Join(dataDir, "fitguard.db")
	s, _ := logging.Open(same)
	s.Close()
	err := RunImportHistory(writeImportConfig(t, dataDir), same, false)
	if err == nil || !strings.Contains(err.Error(), "same file") {
		t.Fatalf("got %v, want the same-file refusal", err)
	}
}
