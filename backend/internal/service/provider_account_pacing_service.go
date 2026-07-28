package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21H provider-pacing: Portal 设置账号调度档位（steady/smart/burst）。
// 档位存 accounts.extra["pacing_mode"]（JSONB key 级合并，不碰其它 Extra 键），
// 调度侧 account_pacing.go 读取生效。幂等：重复设同档位是无害写。

// ErrInvalidPacingMode 非法档位。
var ErrInvalidPacingMode = infraerrors.BadRequest(
	"INVALID_PACING_MODE", "pacing_mode must be one of humanized/standard/speed_2x/speed_3x/speed_5x")

// providerPacingAccountRepo 写 Extra 的最小依赖面（账号 repo 天然满足）。
type providerPacingAccountRepo interface {
	UpdateExtra(ctx context.Context, id int64, updates map[string]any) error
}

// ProviderAccountPacingService 设置 provider 账号的调度档位。
type ProviderAccountPacingService struct {
	locator providerDeactivateLocator // 复用：按 external_ref 定位账号
	repo    providerPacingAccountRepo
}

// NewProviderAccountPacingService builds the service.
func NewProviderAccountPacingService(
	locator providerDeactivateLocator,
	repo providerPacingAccountRepo,
) *ProviderAccountPacingService {
	return &ProviderAccountPacingService{locator: locator, repo: repo}
}

// SetPacingMode 按 external_ref 设置档位。
func (s *ProviderAccountPacingService) SetPacingMode(
	ctx context.Context, externalRef, mode string,
) (*struct {
	Status string `json:"status"`
	Mode   string `json:"mode"`
}, error) {
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		return nil, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !IsValidPacingMode(mode) {
		return nil, ErrInvalidPacingMode
	}

	id, found, err := s.locator.FindAccountIDByExternalRef(ctx, externalRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrProviderAccountNotFound
	}

	if err := s.repo.UpdateExtra(ctx, id, map[string]any{"pacing_mode": mode}); err != nil {
		return nil, err
	}
	return &struct {
		Status string `json:"status"`
		Mode   string `json:"mode"`
	}{Status: "updated", Mode: mode}, nil
}
