//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

// Phase 21E-6C-2B-1: 代理分配仓储的真库集成测试，重点验证
// SELECT ... FOR UPDATE SKIP LOCKED 的并发错开行为。
// 直写共享 integrationDB（非事务隔离，用唯一 region 标签 + 清理避免串扰）。
type ProxyAllocationRepoSuite struct {
	suite.Suite
	ctx    context.Context
	repo   *proxyAllocationRepository
	region string
}

func (s *ProxyAllocationRepoSuite) SetupTest() {
	s.ctx = context.Background()
	s.repo = &proxyAllocationRepository{sqlDB: integrationDB}
	s.region = "T" + time.Now().Format("150405.000000000")
}

func (s *ProxyAllocationRepoSuite) TearDownTest() {
	_, _ = integrationDB.ExecContext(s.ctx,
		`DELETE FROM accounts WHERE proxy_id IN (SELECT id FROM proxies WHERE region = $1)`, s.region)
	_, _ = integrationDB.ExecContext(s.ctx, `DELETE FROM proxies WHERE region = $1`, s.region)
}

func TestProxyAllocationRepoSuite(t *testing.T) {
	suite.Run(t, new(ProxyAllocationRepoSuite))
}

func (s *ProxyAllocationRepoSuite) insertProxy(name, status string, expiresAt *time.Time) int64 {
	return s.insertProxyCap(name, status, expiresAt, 1)
}

// insertProxyCap inserts a proxy with an explicit max_bindings capacity.
func (s *ProxyAllocationRepoSuite) insertProxyCap(name, status string, expiresAt *time.Time, maxBindings int) int64 {
	var id int64
	err := integrationDB.QueryRowContext(s.ctx, `
		INSERT INTO proxies (name, protocol, host, port, status, region, expires_at, max_bindings)
		VALUES ($1,'http','127.0.0.1',8080,$2,$3,$4,$5)
		RETURNING id`, name, status, s.region, expiresAt, maxBindings).Scan(&id)
	s.Require().NoError(err)
	return id
}

func (s *ProxyAllocationRepoSuite) bindAccounts(proxyID int64, n int) {
	s.bindAccountsPlatform(proxyID, n, "anthropic")
}

func (s *ProxyAllocationRepoSuite) bindAccountsPlatform(proxyID int64, n int, platform string) {
	for i := 0; i < n; i++ {
		_, err := integrationDB.ExecContext(s.ctx, `
			INSERT INTO accounts (name, platform, type, credentials, proxy_id)
			VALUES ($1,$2,'oauth','{}',$3)`, s.T().Name(), platform, proxyID)
		s.Require().NoError(err)
	}
}

// 1c) 容量按 platform 分桶（B3）：max_bindings=1 的代理被 anthropic 占满后，
// openai 仍可选到它 —— 两个平台各自独立计数、互不挤占。
func (s *ProxyAllocationRepoSuite) TestCapacityIsPerPlatform() {
	proxy := s.insertProxy("shared-x", "active", nil) // max_bindings=1
	s.bindAccountsPlatform(proxy, 1, "anthropic")     // anthropic 占满

	// anthropic 视角：已满 → 无候选。
	p, err := s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "anthropic")
	s.Require().NoError(err)
	s.Require().Nil(p, "anthropic is full on this proxy")

	// openai 视角：独立名额，仍可选中同一个代理。
	p, err = s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "openai")
	s.Require().NoError(err)
	s.Require().NotNil(p)
	s.Require().Equal(proxy, p.ID, "openai has its own quota on the same shared proxy")
}

// 1) region 匹配 + 容量已满的代理被排除（max_bindings=1，busy 已绑 → 选 idle）。
func (s *ProxyAllocationRepoSuite) TestSelectsAvailableInRegion() {
	busy := s.insertProxy("busy", "active", nil) // max_bindings=1
	idle := s.insertProxy("idle", "active", nil)
	s.bindAccounts(busy, 1) // busy now at capacity

	p, err := s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "anthropic")
	s.Require().NoError(err)
	s.Require().NotNil(p)
	s.Require().Equal(idle, p.ID, "a full proxy must be excluded, the free one wins")
}

// 1b) max_bindings=N：容量未满时仍可选，达到 N 后被排除。
func (s *ProxyAllocationRepoSuite) TestCapacityRespectsMaxBindings() {
	shared := s.insertProxyCap("shared", "active", nil, 2)
	// 1 bound: still under capacity (2) → selectable.
	s.bindAccounts(shared, 1)
	p, err := s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "anthropic")
	s.Require().NoError(err)
	s.Require().NotNil(p)
	s.Require().Equal(shared, p.ID, "proxy under max_bindings must be selectable")

	// 2 bound: at capacity → excluded, no candidate left.
	s.bindAccounts(shared, 1)
	p, err = s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "anthropic")
	s.Require().NoError(err)
	s.Require().Nil(p, "proxy at max_bindings must be excluded")
}

// 2) 无可用容量：nil, nil。
func (s *ProxyAllocationRepoSuite) TestNoCapacityReturnsNil() {
	s.insertProxy("inactive", "expired", nil)
	p, err := s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "anthropic")
	s.Require().NoError(err)
	s.Require().Nil(p)
}

// 3) expired 过滤。
func (s *ProxyAllocationRepoSuite) TestExpiredProxyFiltered() {
	past := time.Now().Add(-time.Hour)
	s.insertProxy("stale", "active", &past)
	fresh := s.insertProxy("fresh", "active", nil)

	p, err := s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "anthropic")
	s.Require().NoError(err)
	s.Require().NotNil(p)
	s.Require().Equal(fresh, p.ID, "expired proxy must be excluded")
}

// 4) 并发错开：A 事务锁住一个候选未提交时，B 的 SKIP LOCKED 必须跳到另一个。
func (s *ProxyAllocationRepoSuite) TestConcurrentSelectSkipsLocked() {
	p1 := s.insertProxy("c1", "active", nil)
	p2 := s.insertProxy("c2", "active", nil)

	// A：手动事务选一行并保持锁（不提交）。
	txA, err := integrationDB.BeginTx(s.ctx, nil)
	s.Require().NoError(err)
	defer func() { _ = txA.Rollback() }()

	var aID int64
	var name, protocol, host, username, password, status string
	var port int
	var expiresAt sql.NullTime
	err = txA.QueryRowContext(s.ctx, selectLeastLoadedProxySQL, s.region).Scan(
		&aID, &name, &protocol, &host, &port, &username, &password, &status, &expiresAt)
	s.Require().NoError(err)

	// B：并发选择——必须跳过 A 锁定的行，拿到另一个候选。
	bProxy, err := s.repo.SelectLeastLoadedActiveProxyForUpdate(s.ctx, s.region, "anthropic")
	s.Require().NoError(err)
	s.Require().NotNil(bProxy)
	s.Require().NotEqual(aID, bProxy.ID, "concurrent selects must not collide")
	s.Require().Contains([]int64{p1, p2}, bProxy.ID)

	_ = txA.Rollback()
}
