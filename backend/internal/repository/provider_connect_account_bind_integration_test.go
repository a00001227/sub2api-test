//go:build integration

package repository

import (
	"context"
	"testing"

	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// Phase 21E-6E group-bind: CreateConnectedAccount 必须把 provider 账号绑定到
// 该 platform 下所有活跃分组，否则账号进不了调度池（不可被 gateway 消费、
// 无收益）。决策 A：无活跃分组时创建失败，不产生死账号。

type ProviderConnectAccountBindSuite struct {
	suite.Suite
	ctx  context.Context
	repo service.ProviderConnectAccountRepository
}

func (s *ProviderConnectAccountBindSuite) SetupTest() {
	s.ctx = context.Background()
	// 用非事务 client：CreateConnectedAccount 内部会自己开 Tx。
	client := testEntClient(s.T())
	s.repo = NewProviderConnectAccountRepository(client)
}

func TestProviderConnectAccountBindSuite(t *testing.T) {
	suite.Run(t, new(ProviderConnectAccountBindSuite))
}

func (s *ProviderConnectAccountBindSuite) seedGroup(name, platform, status string) int64 {
	g, err := testEntClient(s.T()).Group.Create().
		SetName(name).
		SetSlug(name).
		SetPlatform(platform).
		SetStatus(status).
		Save(s.ctx)
	s.Require().NoError(err)
	return g.ID
}

func (s *ProviderConnectAccountBindSuite) accountGroupIDs(accountID int64) []int64 {
	ids, err := testEntClient(s.T()).AccountGroup.Query().
		Where(dbaccountgroup.AccountIDEQ(accountID)).
		Select(dbaccountgroup.FieldGroupID).
		Ints(s.ctx)
	s.Require().NoError(err)
	out := make([]int64, 0, len(ids))
	for _, i := range ids {
		out = append(out, int64(i))
	}
	return out
}

// 成功：claude 账号绑定到所有 anthropic 活跃分组（不含 openai / 非活跃）。
func (s *ProviderConnectAccountBindSuite) TestBindsAllActiveAnthropicGroups() {
	g1 := s.seedGroup("anthropic-a", service.PlatformAnthropic, service.StatusActive)
	g2 := s.seedGroup("anthropic-b", service.PlatformAnthropic, service.StatusActive)
	// 干扰项：openai 组 + 非活跃 anthropic 组，都不应被绑定
	s.seedGroup("openai-a", service.PlatformOpenAI, service.StatusActive)
	s.seedGroup("anthropic-disabled", service.PlatformAnthropic, service.StatusDisabled)

	accID, err := s.repo.CreateConnectedAccount(s.ctx, service.CreateConnectedAccountInput{
		Name:                      "pa_bind_1",
		Platform:                  service.PlatformAnthropic,
		Credentials:               map[string]any{"access_token": "at"},
		ExternalProviderAccountID: "pa_bind_1",
	})
	s.Require().NoError(err)
	s.Require().NotZero(accID)

	bound := s.accountGroupIDs(accID)
	s.Require().ElementsMatch([]int64{g1, g2}, bound, "must bind exactly the active anthropic groups")
}

// 决策 A：该 platform 无活跃分组 → 创建失败，且不留下账号行。
func (s *ProviderConnectAccountBindSuite) TestNoActiveGroupRejects() {
	// 只有 openai 组;anthropic 无活跃组
	s.seedGroup("openai-only", service.PlatformOpenAI, service.StatusActive)

	accID, err := s.repo.CreateConnectedAccount(s.ctx, service.CreateConnectedAccountInput{
		Name:                      "pa_nogroup",
		Platform:                  service.PlatformAnthropic,
		Credentials:               map[string]any{"access_token": "at"},
		ExternalProviderAccountID: "pa_nogroup",
	})
	s.Require().Error(err)
	s.Require().Zero(accID)

	// 决策 A 关键:失败即无账号行(先查组再建账号,不产生死账号)
	_, found, ferr := s.repo.FindAccountIDByExternalRef(s.ctx, "pa_nogroup")
	s.Require().NoError(ferr)
	s.Require().False(found, "no orphan account may be left when group binding is impossible")
}

// 绑定后账号可被 external_ref 反查（幂等收敛前提）。
func (s *ProviderConnectAccountBindSuite) TestFindAfterCreate() {
	s.seedGroup("anthropic-x", service.PlatformAnthropic, service.StatusActive)
	accID, err := s.repo.CreateConnectedAccount(s.ctx, service.CreateConnectedAccountInput{
		Name:                      "pa_find",
		Platform:                  service.PlatformAnthropic,
		Credentials:               map[string]any{"access_token": "at"},
		ExternalProviderAccountID: "pa_find",
	})
	s.Require().NoError(err)

	id, found, err := s.repo.FindAccountIDByExternalRef(s.ctx, "pa_find")
	s.Require().NoError(err)
	s.Require().True(found)
	s.Require().Equal(accID, id)
}
