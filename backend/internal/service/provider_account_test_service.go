package service

import (
	"context"
	"time"
)

// #90(admin-only 测试连接): 把 Portal→cell 的 provider-internal "test" 动作桥接到
// AccountTestService —— external_ref → account id → 跑真实平台测试 → JSON 结论。
// "仅 admin"由 Portal 侧的 JwtAuthGuard+AdminGuard 把关;cell 侧仍是同一条
// provider-internal 通道(内部 token 鉴权),与 pause/reauth/pacing 一致。

type providerTestLocator interface {
	FindAccountIDByExternalRef(ctx context.Context, externalRef string) (int64, bool, error)
}

type accountConnectionTester interface {
	RunConnectionTestJSON(ctx context.Context, accountID int64, modelID, prompt, mode string) AccountTestResult
}

// ProviderAccountTestService triggers a single account's real connectivity test.
type ProviderAccountTestService struct {
	locator providerTestLocator
	tester  accountConnectionTester
}

// NewProviderAccountTestService wires the locator + the underlying tester.
func NewProviderAccountTestService(locator providerTestLocator, tester accountConnectionTester) *ProviderAccountTestService {
	return &ProviderAccountTestService{locator: locator, tester: tester}
}

// ProviderAccountTestResult is the desensitized verdict returned to the Portal.
type ProviderAccountTestResult struct {
	Success bool   `json:"success"`
	Model   string `json:"model,omitempty"`
	Message string `json:"message,omitempty"`
}

// providerAccountTestTimeout bounds one test so a hung upstream can't hold the
// admin request open indefinitely (the Portal fetch also has its own timeout).
const providerAccountTestTimeout = 30 * time.Second

// Test resolves the account by external ref and runs the connectivity test.
// NOTE: the test has side effects on failure (SetError/SetRateLimited → #87
// status reflow) — identical to sub2api's own admin "test" button, on purpose.
func (s *ProviderAccountTestService) Test(
	ctx context.Context, externalRef, modelID, prompt, mode string,
) (*ProviderAccountTestResult, error) {
	id, found, err := s.locator.FindAccountIDByExternalRef(ctx, externalRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrProviderAccountNotFound
	}
	tctx, cancel := context.WithTimeout(ctx, providerAccountTestTimeout)
	defer cancel()
	r := s.tester.RunConnectionTestJSON(tctx, id, modelID, prompt, mode)
	return &ProviderAccountTestResult{Success: r.Success, Model: r.Model, Message: r.Message}, nil
}
