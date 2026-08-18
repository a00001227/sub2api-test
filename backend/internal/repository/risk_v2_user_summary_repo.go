package repository

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 切片 5：Risk V2 Admin 列表/详情用的批量用户摘要读取（避免 N+1）。
// 只读安全字段（id/email/username/status）——绝不返回密码 hash / API Key / Session / OAuth Token / 凭据 / 私钥。

type riskV2UserSummaryRepo struct {
	client *dbent.Client
}

// NewRiskV2UserSummaryReader 构造批量用户摘要读取器。client 为 nil → nil。
func NewRiskV2UserSummaryReader(client *dbent.Client) service.RiskV2UserSummaryReader {
	if client == nil {
		return nil
	}
	return &riskV2UserSummaryRepo{client: client}
}

// GetSummariesByIDs 单次查询批量取用户摘要（IDIn），只选安全字段。
func (r *riskV2UserSummaryRepo) GetSummariesByIDs(ctx context.Context, ids []int64) (map[int64]service.RiskV2UserSummary, error) {
	out := make(map[int64]service.RiskV2UserSummary, len(ids))
	if r == nil || r.client == nil || len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := r.client.User.Query().
		Where(dbuser.IDIn(ids...)).
		Select(dbuser.FieldID, dbuser.FieldEmail, dbuser.FieldUsername, dbuser.FieldStatus).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range rows {
		out[u.ID] = service.RiskV2UserSummary{
			UserID:   u.ID,
			Email:    u.Email,
			Username: u.Username,
			Status:   u.Status,
			Known:    true,
		}
	}
	return out, nil
}
