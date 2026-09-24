package service

import (
	"context"
	"errors"
	"testing"
)

// 定时探活遇到 disabled 号(Portal 软删/孤儿清理后的 cell 侧终态)必须直接跳过:
// 否则 401 → SetError 会把它翻回 error,清理完过一会又出现在监控面板上。
type disabledSkipAccountRepo struct {
	AccountRepository
	acc *Account
}

func (r *disabledSkipAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.acc, nil
}

func TestRunTestBackground_SkipsDisabledAccount(t *testing.T) {
	svc := &AccountTestService{accountRepo: &disabledSkipAccountRepo{acc: &Account{ID: 9, Status: StatusDisabled}}}
	res, err := svc.RunTestBackground(context.Background(), 9, "")
	if !errors.Is(err, ErrScheduledTestAccountDisabled) {
		t.Fatalf("expected ErrScheduledTestAccountDisabled, got res=%v err=%v", res, err)
	}
	if res != nil {
		t.Fatalf("expected nil result for skipped probe, got %+v", res)
	}
}
