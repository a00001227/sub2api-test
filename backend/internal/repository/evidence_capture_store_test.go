//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newEvidenceStoreTest(t *testing.T) service.EvidenceCaptureStore {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewEvidenceCaptureStore(rdb)
}

func TestEvidenceStore_NilRedis(t *testing.T) {
	if NewEvidenceCaptureStore(nil) != nil {
		t.Fatal("nil redis → nil store")
	}
}

func TestEvidenceStore_FlagRoundTrip(t *testing.T) {
	s := newEvidenceStoreTest(t)
	ctx := context.Background()
	f := service.EvidenceFlag{TargetKey: "u:42", TargetType: "user", TargetID: 42, Remaining: 5, Max: 5, StartedAt: 1, AdminID: 1}
	if err := s.SaveFlag(ctx, f); err != nil {
		t.Fatal(err)
	}
	flags, err := s.LoadFlags(ctx)
	if err != nil || len(flags) != 1 || flags[0].TargetKey != "u:42" || flags[0].Remaining != 5 {
		t.Fatalf("load flags: %v %+v", err, flags)
	}
	if err := s.DeleteFlag(ctx, "u:42"); err != nil {
		t.Fatal(err)
	}
	if flags, _ := s.LoadFlags(ctx); len(flags) != 0 {
		t.Fatalf("flag should be deleted, got %+v", flags)
	}
}

func TestEvidenceStore_AppendCapAndList(t *testing.T) {
	s := newEvidenceStoreTest(t)
	ctx := context.Background()
	// 追加 5 条，cap=3 → 只保留最新 3 条（LTRIM 封顶）。
	for i := 0; i < 5; i++ {
		e := service.EvidenceEntry{Ts: int64(i), UserID: 42, Body: "b", Model: "m"}
		if err := s.AppendEvidence(ctx, "u:42", e, 3, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	es, err := s.ListEvidence(ctx, "u:42", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("LTRIM should cap at 3, got %d", len(es))
	}
	// 最新在前（LPUSH）：Ts 应为 4,3,2。
	if es[0].Ts != 4 || es[2].Ts != 2 {
		t.Fatalf("order/newest-first wrong: %d..%d", es[0].Ts, es[2].Ts)
	}
}

func TestEvidenceStore_Purge(t *testing.T) {
	s := newEvidenceStoreTest(t)
	ctx := context.Background()
	_ = s.AppendEvidence(ctx, "k:9", service.EvidenceEntry{Ts: 1}, 10, time.Hour)
	if es, _ := s.ListEvidence(ctx, "k:9", 10); len(es) != 1 {
		t.Fatal("precondition: 1 entry")
	}
	if err := s.PurgeEvidence(ctx, "k:9"); err != nil {
		t.Fatal(err)
	}
	if es, _ := s.ListEvidence(ctx, "k:9", 10); len(es) != 0 {
		t.Fatal("evidence should be purged")
	}
}
