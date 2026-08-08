package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeTestLocator struct {
	id    int64
	found bool
	err   error
}

func (f *fakeTestLocator) FindAccountIDByExternalRef(_ context.Context, _ string) (int64, bool, error) {
	return f.id, f.found, f.err
}

type fakeConnTester struct {
	gotID int64
	res   AccountTestResult
}

func (f *fakeConnTester) RunConnectionTestJSON(_ context.Context, accountID int64, _, _, _ string) AccountTestResult {
	f.gotID = accountID
	return f.res
}

// external_ref → id 解析后跑测试, 结果原样透出。
func TestProviderAccountTest_ResolvesAndReturns(t *testing.T) {
	tester := &fakeConnTester{res: AccountTestResult{Success: true, Model: "claude-x", Message: "ok"}}
	svc := NewProviderAccountTestService(&fakeTestLocator{id: 42, found: true}, tester)

	out, err := svc.Test(context.Background(), "pa_abc", "claude-x", "hi", "text")
	require.NoError(t, err)
	require.Equal(t, int64(42), tester.gotID)
	require.True(t, out.Success)
	require.Equal(t, "claude-x", out.Model)
	require.Equal(t, "ok", out.Message)
}

// 账号不存在 → ErrProviderAccountNotFound, 不调用 tester。
func TestProviderAccountTest_NotFound(t *testing.T) {
	tester := &fakeConnTester{}
	svc := NewProviderAccountTestService(&fakeTestLocator{found: false}, tester)

	_, err := svc.Test(context.Background(), "pa_missing", "", "", "")
	require.ErrorIs(t, err, ErrProviderAccountNotFound)
	require.Equal(t, int64(0), tester.gotID)
}

// locator 出错 → 透出错误。
func TestProviderAccountTest_LocatorError(t *testing.T) {
	boom := errors.New("db down")
	svc := NewProviderAccountTestService(&fakeTestLocator{err: boom}, &fakeConnTester{})

	_, err := svc.Test(context.Background(), "pa_x", "", "", "")
	require.ErrorIs(t, err, boom)
}
