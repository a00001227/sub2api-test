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
	f := service.EvidenceFlag{TargetKey: "u:42", TargetType: "user", TargetID: 42, StoreThreshold: 2, MaxTemplates: 500, StartedAt: 1, AdminID: 1}
	if err := s.SaveFlag(ctx, f); err != nil {
		t.Fatal(err)
	}
	flags, err := s.LoadFlags(ctx)
	if err != nil || len(flags) != 1 || flags[0].TargetKey != "u:42" || flags[0].StoreThreshold != 2 {
		t.Fatalf("load flags: %v %+v", err, flags)
	}
	if err := s.DeleteFlag(ctx, "u:42"); err != nil {
		t.Fatal(err)
	}
	if flags, _ := s.LoadFlags(ctx); len(flags) != 0 {
		t.Fatalf("flag should be deleted, got %+v", flags)
	}
}

func TestEvidenceStore_TemplateRoundTripAndSort(t *testing.T) {
	s := newEvidenceStoreTest(t)
	ctx := context.Background()
	// 不存在 → nil。
	if tp, _ := s.GetTemplate(ctx, "u:42", "aa"); tp != nil {
		t.Fatal("missing template should be nil")
	}
	_ = s.PutTemplate(ctx, "u:42", service.EvidenceTemplate{Simhash: "aa", Count: 2, Body: "b", HasBody: true}, time.Hour)
	_ = s.PutTemplate(ctx, "u:42", service.EvidenceTemplate{Simhash: "bb", Count: 9}, time.Hour)
	_ = s.PutTemplate(ctx, "u:42", service.EvidenceTemplate{Simhash: "cc", Count: 5}, time.Hour)

	if n, _ := s.TemplateCount(ctx, "u:42"); n != 3 {
		t.Fatalf("template count want 3, got %d", n)
	}
	got, _ := s.GetTemplate(ctx, "u:42", "aa")
	if got == nil || got.Count != 2 || !got.HasBody {
		t.Fatalf("get template aa: %+v", got)
	}
	list, err := s.ListTemplates(ctx, "u:42")
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v n=%d", err, len(list))
	}
	// 按 count 降序：bb(9), cc(5), aa(2)。
	if list[0].Simhash != "bb" || list[1].Simhash != "cc" || list[2].Simhash != "aa" {
		t.Fatalf("templates must be sorted by count desc, got %s,%s,%s", list[0].Simhash, list[1].Simhash, list[2].Simhash)
	}
}

func TestEvidenceStore_Purge(t *testing.T) {
	s := newEvidenceStoreTest(t)
	ctx := context.Background()
	_ = s.PutTemplate(ctx, "k:9", service.EvidenceTemplate{Simhash: "aa", Count: 1}, time.Hour)
	if n, _ := s.TemplateCount(ctx, "k:9"); n != 1 {
		t.Fatal("precondition: 1 template")
	}
	if err := s.PurgeEvidence(ctx, "k:9"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.TemplateCount(ctx, "k:9"); n != 0 {
		t.Fatal("evidence should be purged")
	}
}
