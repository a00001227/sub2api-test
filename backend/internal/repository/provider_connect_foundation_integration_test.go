//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/providerconnectsession"
	"github.com/stretchr/testify/suite"
)

// Phase 21E-6C-2A schema foundation tests: new nullable columns must leave
// existing proxy/account behavior untouched; the partial unique constraint
// and the provider_connect_sessions lifecycle must hold.
type ProviderConnectFoundationSuite struct {
	suite.Suite
	ctx context.Context
	tx  *dbent.Tx
}

func (s *ProviderConnectFoundationSuite) SetupTest() {
	s.ctx = context.Background()
	s.tx = testEntTx(s.T())
}

func TestProviderConnectFoundationSuite(t *testing.T) {
	suite.Run(t, new(ProviderConnectFoundationSuite))
}

// 旧 proxy 行为：不带 region 创建（NULL 合法），读取正常。
func (s *ProviderConnectFoundationSuite) TestLegacyProxyWithoutRegionStillWorks() {
	p, err := s.tx.Proxy.Create().
		SetName("legacy-proxy").
		SetProtocol("http").
		SetHost("127.0.0.1").
		SetPort(8080).
		Save(s.ctx)
	s.Require().NoError(err)
	s.Require().Nil(p.Region, "legacy proxy must default to NULL region")

	got, err := s.tx.Proxy.Get(s.ctx, p.ID)
	s.Require().NoError(err)
	s.Require().Equal("legacy-proxy", got.Name)
	s.Require().Nil(got.Region)
}

// region 可写可读；不影响其他字段。
func (s *ProviderConnectFoundationSuite) TestProxyRegionReadWrite() {
	p, err := s.tx.Proxy.Create().
		SetName("jp-node").
		SetProtocol("socks5").
		SetHost("10.0.0.9").
		SetPort(1080).
		SetRegion("JP").
		Save(s.ctx)
	s.Require().NoError(err)

	got, err := s.tx.Proxy.Get(s.ctx, p.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.Region)
	s.Require().Equal("JP", *got.Region)
}

// 旧 account 行为：不带 external_provider_account_id 创建（NULL 合法），
// 多个 NULL 行共存不触发唯一约束。
func (s *ProviderConnectFoundationSuite) TestLegacyAccountsNullReferenceCoexist() {
	for _, name := range []string{"legacy-a", "legacy-b"} {
		_, err := s.tx.Account.Create().
			SetName(name).
			SetPlatform("anthropic").
			SetType("oauth").
			SetCredentials(map[string]any{"access_token": "t"}).
			Save(s.ctx)
		s.Require().NoError(err, "NULL external_provider_account_id rows must coexist")
	}
	n, err := s.tx.Account.Query().
		Where(account.ExternalProviderAccountIDIsNil()).
		Count(s.ctx)
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(n, 2)
}

// 部分唯一约束：同一 pa_ 引用第二次插入必须失败。
func (s *ProviderConnectFoundationSuite) TestExternalProviderAccountIDPartialUnique() {
	const ref = "pa_test_001"
	_, err := s.tx.Account.Create().
		SetName("portal-acc-1").
		SetPlatform("anthropic").
		SetType("oauth").
		SetCredentials(map[string]any{"access_token": "t1"}).
		SetExternalProviderAccountID(ref).
		Save(s.ctx)
	s.Require().NoError(err, "first pa_test_001 must succeed")

	_, err = s.tx.Account.Create().
		SetName("portal-acc-2").
		SetPlatform("anthropic").
		SetType("oauth").
		SetCredentials(map[string]any{"access_token": "t2"}).
		SetExternalProviderAccountID(ref).
		Save(s.ctx)
	s.Require().Error(err, "second pa_test_001 must violate the partial unique index")
}

// provider_connect_sessions 生命周期：pending 创建 → completed 更新。
func (s *ProviderConnectFoundationSuite) TestConnectSessionLifecycle() {
	created, err := s.tx.ProviderConnectSession.Create().
		SetExternalProviderAccountID("pa_session_001").
		SetProviderType("claude").
		SetRegion("US").
		SetCallbackURL("https://portal.example.com/internal/sub2api/events").
		SetExpiresAt(time.Now().Add(10 * time.Minute)).
		Save(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal("pending", created.Status, "default status must be pending")
	s.Require().Nil(created.CompletedAt)
	s.Require().Nil(created.ProxyID)

	now := time.Now()
	updated, err := s.tx.ProviderConnectSession.UpdateOneID(created.ID).
		SetStatus("completed").
		SetProxyID(0). // placeholder id shape; allocation arrives后续阶段
		SetCompletedAt(now).
		Save(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal("completed", updated.Status)
	s.Require().NotNil(updated.CompletedAt)

	// 状态白名单：非法状态被 schema 校验拒绝
	_, err = s.tx.ProviderConnectSession.UpdateOneID(created.ID).
		SetStatus("teleported").
		Save(s.ctx)
	s.Require().Error(err, "invalid status must be rejected by the validator")

	// 索引可查性冒烟
	n, err := s.tx.ProviderConnectSession.Query().
		Where(providerconnectsession.StatusEQ("completed")).
		Count(s.ctx)
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(n, 1)
}
