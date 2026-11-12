package logging

import (
	"context"
	"testing"
)

func TestDialect_BatchOwnershipAndSettleOnce(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if err := s.SaveBatchFile(ctx, BatchFile{FileID: "f1", UserID: "alice", Provider: "openai", Models: `["gpt-4o"]`, EstimatedUSD: 0.25}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.GetBatchFile(ctx, "f1", "bob"); ok {
			t.Fatal("bob must not see alice's file")
		}
		f, ok, err := s.GetBatchFile(ctx, "f1", "alice")
		if err != nil || !ok || f.EstimatedUSD != 0.25 {
			t.Fatalf("alice's file: %+v ok=%v err=%v", f, ok, err)
		}

		if err := s.SaveBatch(ctx, Batch{BatchID: "b1", UserID: "alice", Provider: "openai", Endpoint: "/v1/chat/completions", Status: "validating"}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.GetBatch(ctx, "b1", "bob"); ok {
			t.Fatal("bob must not see alice's batch")
		}
		if err := s.UpdateBatchStatus(ctx, "b1", "completed", "out1", ""); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.BatchByOutputFile(ctx, "out1", "alice"); !ok {
			t.Fatal("alice owns the output file of her batch")
		}
		if _, ok, _ := s.BatchByOutputFile(ctx, "out1", "bob"); ok {
			t.Fatal("bob must not own alice's output file")
		}

		pending, _ := s.PendingBatches(ctx)
		if len(pending) != 1 {
			t.Fatalf("pending = %d, want 1", len(pending))
		}
		first, err := s.SettleBatch(ctx, "b1", 1.5)
		second, _ := s.SettleBatch(ctx, "b1", 1.5)
		if err != nil || !first || second {
			t.Fatalf("settle: first=%v second=%v err=%v; exactly one caller may claim a batch", first, second, err)
		}
		if pending, _ := s.PendingBatches(ctx); len(pending) != 0 {
			t.Fatal("a settled batch is still pending")
		}
	})
}
