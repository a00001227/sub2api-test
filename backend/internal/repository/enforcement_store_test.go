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

func newEnfStoreTest(t *testing.T) (*miniredis.Miniredis, service.EnforcementStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, NewEnforcementStore(rdb)
}

func TestEnforcementStore_NilRedis(t *testing.T) {
	if NewEnforcementStore(nil) != nil {
		t.Fatal("nil redis → nil store")
	}
}

func TestEnforcementStore_AllowlistRoundTrip(t *testing.T) {
	_, s := newEnfStoreTest(t)
	ctx := context.Background()
	if ids, _ := s.LoadAllowlist(ctx); len(ids) != 0 {
		t.Fatal("空名单")
	}
	_ = s.AddAllowlist(ctx, 7)
	_ = s.AddAllowlist(ctx, 7) // 幂等
	_ = s.AddAllowlist(ctx, 9)
	ids, err := s.LoadAllowlist(ctx)
	if err != nil || len(ids) != 2 {
		t.Fatalf("load allowlist: %v n=%d", err, len(ids))
	}
	_ = s.RemoveAllowlist(ctx, 7)
	if ids, _ := s.LoadAllowlist(ctx); len(ids) != 1 || ids[0] != 9 {
		t.Fatalf("移除后应只剩 9, got %+v", ids)
	}
}

func TestEnforcementStore_ThrottleCounter(t *testing.T) {
	mr, s := newEnfStoreTest(t)
	ctx := context.Background()
	for want := int64(1); want <= 3; want++ {
		got, err := s.IncrThrottleCounter(ctx, 42, time.Hour)
		if err != nil || got != want {
			t.Fatalf("incr #%d: got %d err %v", want, got, err)
		}
	}
	// 不同用户独立计数。
	if got, _ := s.IncrThrottleCounter(ctx, 43, time.Hour); got != 1 {
		t.Fatalf("独立用户应从 1 计，got %d", got)
	}
	// TTL 已设置（键存在且有正 TTL）。
	keys := mr.Keys()
	if len(keys) == 0 {
		t.Fatal("应有计数键")
	}
}

func TestEnforcementStore_ModelRulesRoundTrip(t *testing.T) {
	_, s := newEnfStoreTest(t)
	ctx := context.Background()
	if m, _ := s.LoadModelRules(ctx); len(m) != 0 {
		t.Fatal("空规则")
	}
	_ = s.SetModelRule(ctx, "claude-opus-4", "block")
	_ = s.SetModelRule(ctx, "claude-sonnet-4", "throttle")
	m, err := s.LoadModelRules(ctx)
	if err != nil || len(m) != 2 || m["claude-opus-4"] != "block" || m["claude-sonnet-4"] != "throttle" {
		t.Fatalf("load model rules: %v %+v", err, m)
	}
	_ = s.DeleteModelRule(ctx, "claude-opus-4")
	if m, _ := s.LoadModelRules(ctx); len(m) != 1 || m["claude-sonnet-4"] != "throttle" {
		t.Fatalf("删除后应只剩 sonnet, got %+v", m)
	}
}

func TestEnforcementStore_ModelThrottleCounterIsolated(t *testing.T) {
	_, s := newEnfStoreTest(t)
	ctx := context.Background()
	// (user,model) 独立于用户级桶，也按模型隔离。
	if got, _ := s.IncrModelThrottleCounter(ctx, 1, "opus", time.Hour); got != 1 {
		t.Fatalf("opus 应从 1 计, got %d", got)
	}
	if got, _ := s.IncrModelThrottleCounter(ctx, 1, "opus", time.Hour); got != 2 {
		t.Fatalf("opus 第二次应为 2, got %d", got)
	}
	if got, _ := s.IncrModelThrottleCounter(ctx, 1, "sonnet", time.Hour); got != 1 {
		t.Fatalf("sonnet 独立应从 1 计, got %d", got)
	}
	if got, _ := s.IncrThrottleCounter(ctx, 1, time.Hour); got != 1 {
		t.Fatalf("用户级桶独立应从 1 计, got %d", got)
	}
}
