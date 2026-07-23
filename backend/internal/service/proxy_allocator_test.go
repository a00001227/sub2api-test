package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2B-1: ProxyAllocator 单元测试（假 repo，region 匹配 /
// 无容量 / 大小写 / 错误透传）。SKIP LOCKED 的真实并发行为由集成测试
// 覆盖（proxy_allocation_repo_integration_test.go）。

type fakeAllocationRepo struct {
	got      string // 记录传入的 region（已归一化）
	gotPlat  string // 记录传入的 platform
	proxy    *Proxy
	err      error
	caps     []RegionCapacity
	capsErr  error
	capCalls int // RegionCapacity 被真正调用（查库）的次数
}

func (f *fakeAllocationRepo) SelectLeastLoadedActiveProxyForUpdate(_ context.Context, region, platform string) (*Proxy, error) {
	f.got = region
	f.gotPlat = platform
	return f.proxy, f.err
}

func (f *fakeAllocationRepo) RegionCapacity(_ context.Context, _ string) ([]RegionCapacity, error) {
	f.capCalls++
	return f.caps, f.capsErr
}

func TestProxyAllocator_RegionMatch(t *testing.T) {
	repo := &fakeAllocationRepo{proxy: &Proxy{ID: 7, Name: "us-1", Status: StatusActive}}
	a := NewProxyAllocator(repo)

	p, err := a.SelectProxy(context.Background(), "us", "anthropic")
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, int64(7), p.ID)
	require.Equal(t, "US", repo.got, "region must be upper-cased and trimmed before query")
}

// AvailableRegions: 脱敏、带 label、available 由 slots>0 决定。
func TestProxyAllocator_AvailableRegions(t *testing.T) {
	repo := &fakeAllocationRepo{caps: []RegionCapacity{
		{Region: "lax", AvailableSlots: 3},
		{Region: "sgp", AvailableSlots: 0},
		{Region: "US", AvailableSlots: 1},  // 存量粗粒度
		{Region: "zzz", AvailableSlots: 2}, // 未知 code → label 回退
	}}
	a := NewProxyAllocator(repo)

	out, err := a.AvailableRegions(context.Background(), "anthropic")
	require.NoError(t, err)
	require.Len(t, out, 4)

	byID := map[string]AvailableRegion{}
	for _, r := range out {
		byID[r.ID] = r
	}
	require.Equal(t, "Los Angeles", byID["lax"].Label)
	require.True(t, byID["lax"].Available)
	require.Equal(t, CapacityLimited, byID["lax"].Capacity, "3 slots → limited")

	require.False(t, byID["sgp"].Available, "0 slots → not available")
	require.Equal(t, CapacityFull, byID["sgp"].Capacity, "0 slots → full")
	require.Equal(t, "United States", byID["US"].Label)
	require.Equal(t, "zzz", byID["zzz"].Label, "unknown code falls back to code")
}

// 容量档位映射：0=full，1..threshold=limited，>threshold=ample。
func TestCapacityTierFromSlots(t *testing.T) {
	require.Equal(t, CapacityFull, capacityTierFromSlots(0))
	require.Equal(t, CapacityFull, capacityTierFromSlots(-3), "负数(理论上不会有)也视为满")
	require.Equal(t, CapacityLimited, capacityTierFromSlots(1))
	require.Equal(t, CapacityLimited, capacityTierFromSlots(capacityLimitedThreshold))
	require.Equal(t, CapacityAmple, capacityTierFromSlots(capacityLimitedThreshold+1))
	require.Equal(t, CapacityAmple, capacityTierFromSlots(100))
}

func TestProxyAllocator_NoCapacity(t *testing.T) {
	repo := &fakeAllocationRepo{proxy: nil} // 无匹配行
	a := NewProxyAllocator(repo)

	p, err := a.SelectProxy(context.Background(), "JP", "anthropic")
	require.Nil(t, p)
	require.ErrorIs(t, err, ErrRegionNoCapacity)
}

func TestProxyAllocator_EmptyRegion(t *testing.T) {
	a := NewProxyAllocator(&fakeAllocationRepo{})
	_, err := a.SelectProxy(context.Background(), "   ", "anthropic")
	require.ErrorIs(t, err, ErrRegionRequired)
}

func TestProxyAllocator_RepoErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	a := NewProxyAllocator(&fakeAllocationRepo{err: boom})
	_, err := a.SelectProxy(context.Background(), "SG", "anthropic")
	require.ErrorIs(t, err, boom)
}

func TestProxyAllocator_NoCapacityIsNotFound(t *testing.T) {
	// 契约：region_no_capacity 是 NotFound 语义（供 handler 映射 404）。
	require.True(t, infraerrors.IsNotFound(ErrRegionNoCapacity))
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, ErrRegionNoCapacity, &appErr)
	require.Equal(t, "REGION_NO_CAPACITY", appErr.Reason)
}

// AvailableRegions 缓存：TTL 内多次调用只查库一次（进程内缓存生效）。
func TestProxyAllocator_AvailableRegions_Cached(t *testing.T) {
	repo := &fakeAllocationRepo{caps: []RegionCapacity{{Region: "lax", AvailableSlots: 3}}}
	a := NewProxyAllocator(repo)

	for i := 0; i < 5; i++ {
		out, err := a.AvailableRegions(context.Background(), "anthropic")
		require.NoError(t, err)
		require.Len(t, out, 1)
		require.Equal(t, "Los Angeles", out[0].Label)
	}
	require.Equal(t, 1, repo.capCalls, "TTL 内多次调用应只查库一次")
}

// AvailableRegions 缓存：并发调用经 singleflight 收敛为一次查库（防击穿）。
func TestProxyAllocator_AvailableRegions_SingleflightCollapsesConcurrent(t *testing.T) {
	repo := &fakeAllocationRepo{caps: []RegionCapacity{{Region: "lax", AvailableSlots: 1}}}
	a := NewProxyAllocator(repo)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := a.AvailableRegions(context.Background(), "anthropic")
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	// 并发首轮：singleflight 应把 N 个并发请求收敛为极少数几次真实查库
	// （理想 1 次；宽松断言避免调度抖动导致偶发 flake）。
	require.LessOrEqual(t, repo.capCalls, 2, "并发请求应被 singleflight 收敛")
}

// AvailableRegions 缓存：load 出错不写缓存，下次调用会重试查库（fail-open）。
func TestProxyAllocator_AvailableRegions_ErrorNotCached(t *testing.T) {
	boom := errors.New("db down")
	repo := &fakeAllocationRepo{capsErr: boom}
	a := NewProxyAllocator(repo)

	_, err := a.AvailableRegions(context.Background(), "anthropic")
	require.ErrorIs(t, err, boom)
	require.Equal(t, 1, repo.capCalls)

	// 恢复后应能重新查库并成功（错误未被缓存）。
	repo.capsErr = nil
	repo.caps = []RegionCapacity{{Region: "US", AvailableSlots: 2}}
	out, err := a.AvailableRegions(context.Background(), "anthropic")
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, 2, repo.capCalls, "错误不缓存，恢复后应再次查库")
}
