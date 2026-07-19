package service

import (
	"context"
	"errors"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2B-1: ProxyAllocator 单元测试（假 repo，region 匹配 /
// 无容量 / 大小写 / 错误透传）。SKIP LOCKED 的真实并发行为由集成测试
// 覆盖（proxy_allocation_repo_integration_test.go）。

type fakeAllocationRepo struct {
	got     string // 记录传入的 region（已归一化）
	proxy   *Proxy
	err     error
	caps    []RegionCapacity
	capsErr error
}

func (f *fakeAllocationRepo) SelectLeastLoadedActiveProxyForUpdate(_ context.Context, region string) (*Proxy, error) {
	f.got = region
	return f.proxy, f.err
}

func (f *fakeAllocationRepo) RegionCapacity(_ context.Context) ([]RegionCapacity, error) {
	return f.caps, f.capsErr
}

func TestProxyAllocator_RegionMatch(t *testing.T) {
	repo := &fakeAllocationRepo{proxy: &Proxy{ID: 7, Name: "us-1", Status: StatusActive}}
	a := NewProxyAllocator(repo)

	p, err := a.SelectProxy(context.Background(), "us")
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

	out, err := a.AvailableRegions(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 4)

	byID := map[string]AvailableRegion{}
	for _, r := range out {
		byID[r.ID] = r
	}
	require.Equal(t, "Los Angeles", byID["lax"].Label)
	require.True(t, byID["lax"].Available)
	require.Equal(t, 3, byID["lax"].AvailableSlots)

	require.False(t, byID["sgp"].Available, "0 slots → not available")
	require.Equal(t, "United States", byID["US"].Label)
	require.Equal(t, "zzz", byID["zzz"].Label, "unknown code falls back to code")
}

func TestProxyAllocator_NoCapacity(t *testing.T) {
	repo := &fakeAllocationRepo{proxy: nil} // 无匹配行
	a := NewProxyAllocator(repo)

	p, err := a.SelectProxy(context.Background(), "JP")
	require.Nil(t, p)
	require.ErrorIs(t, err, ErrRegionNoCapacity)
}

func TestProxyAllocator_EmptyRegion(t *testing.T) {
	a := NewProxyAllocator(&fakeAllocationRepo{})
	_, err := a.SelectProxy(context.Background(), "   ")
	require.ErrorIs(t, err, ErrRegionRequired)
}

func TestProxyAllocator_RepoErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	a := NewProxyAllocator(&fakeAllocationRepo{err: boom})
	_, err := a.SelectProxy(context.Background(), "SG")
	require.ErrorIs(t, err, boom)
}

func TestProxyAllocator_NoCapacityIsNotFound(t *testing.T) {
	// 契约：region_no_capacity 是 NotFound 语义（供 handler 映射 404）。
	require.True(t, infraerrors.IsNotFound(ErrRegionNoCapacity))
	var appErr *infraerrors.ApplicationError
	require.ErrorAs(t, ErrRegionNoCapacity, &appErr)
	require.Equal(t, "REGION_NO_CAPACITY", appErr.Reason)
}
