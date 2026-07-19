package repository

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/providerconnectsession"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Phase 21E-6C-2B-1 / 2C: provider_connect_sessions 仓储（ent 直写）。
type providerConnectSessionRepository struct {
	client *dbent.Client
}

// NewProviderConnectSessionRepository creates the repository.
func NewProviderConnectSessionRepository(client *dbent.Client) service.ProviderConnectSessionRepository {
	return &providerConnectSessionRepository{client: client}
}

func toServiceConnectSession(row *dbent.ProviderConnectSession) *service.ProviderConnectSession {
	return &service.ProviderConnectSession{
		ID:                        row.ID,
		ExternalProviderAccountID: row.ExternalProviderAccountID,
		ProviderType:              row.ProviderType,
		Region:                    row.Region,
		ProxyID:                   row.ProxyID,
		Status:                    row.Status,
		OAuthSessionID:            row.OauthSessionID,
		Sub2apiAccountID:          row.Sub2apiAccountID,
		CallbackURL:               row.CallbackURL,
		ExpiresAt:                 row.ExpiresAt,
		CompletedAt:               row.CompletedAt,
		CreatedAt:                 row.CreatedAt,
	}
}

func (r *providerConnectSessionRepository) Create(
	ctx context.Context, s *service.ProviderConnectSession,
) (*service.ProviderConnectSession, error) {
	builder := r.client.ProviderConnectSession.Create().
		SetExternalProviderAccountID(s.ExternalProviderAccountID).
		SetProviderType(s.ProviderType).
		SetCallbackURL(s.CallbackURL).
		SetExpiresAt(s.ExpiresAt)
	if s.Status != "" {
		builder = builder.SetStatus(s.Status)
	}
	if s.Region != nil && *s.Region != "" {
		builder = builder.SetRegion(*s.Region)
	}
	if s.ProxyID != nil {
		builder = builder.SetProxyID(*s.ProxyID)
	}
	if s.OAuthSessionID != nil && *s.OAuthSessionID != "" {
		builder = builder.SetOauthSessionID(*s.OAuthSessionID)
	}

	row, err := builder.Save(ctx)
	if err != nil {
		return nil, err
	}
	return toServiceConnectSession(row), nil
}

func (r *providerConnectSessionRepository) GetByID(
	ctx context.Context, id int64,
) (*service.ProviderConnectSession, error) {
	row, err := r.client.ProviderConnectSession.Get(ctx, id)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toServiceConnectSession(row), nil
}

// MarkCompleted 条件更新：仅当会话仍为 pending 时置 completed。
// UpdateOne 无法带 WHERE status，故用 Update().Where(...) 批量条件更新，
// 返回受影响行数用于幂等判断（0 = 已非 pending）。
func (r *providerConnectSessionRepository) MarkCompleted(
	ctx context.Context, id int64, sub2apiAccountID int64, completedAt time.Time,
) (int64, error) {
	n, err := r.client.ProviderConnectSession.Update().
		Where(
			providerconnectsession.IDEQ(id),
			providerconnectsession.StatusEQ("pending"),
		).
		SetStatus("completed").
		SetSub2apiAccountID(sub2apiAccountID).
		SetCompletedAt(completedAt).
		Save(ctx)
	return int64(n), err
}

func (r *providerConnectSessionRepository) MarkFailed(ctx context.Context, id int64) error {
	_, err := r.client.ProviderConnectSession.Update().
		Where(
			providerconnectsession.IDEQ(id),
			providerconnectsession.StatusEQ("pending"),
		).
		SetStatus("failed").
		Save(ctx)
	return err
}
