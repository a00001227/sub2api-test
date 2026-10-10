package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

type confirmingProviderStub struct {
	payment.Provider
	key        string
	verifyErr  error
	confirmErr error
	confirmed  int
}

func (p *confirmingProviderStub) ProviderKey() string { return p.key }
func (p *confirmingProviderStub) VerifyNotification(context.Context, string, map[string]string) (*payment.PaymentNotification, error) {
	if p.verifyErr != nil {
		return nil, p.verifyErr
	}
	return &payment.PaymentNotification{OrderID: "ORDER123", Status: payment.ProviderStatusSuccess}, nil
}
func (p *confirmingProviderStub) ConfirmNotification(context.Context, *payment.PaymentNotification) error {
	p.confirmed++
	return p.confirmErr
}

// 验签成功的那个 provider 实例要原样返回,这样 webhook 才能对它做查单确认。
func TestVerifyNotificationWithProvidersResolved_ReturnsVerifyingInstance(t *testing.T) {
	bad := &confirmingProviderStub{key: "easypay", verifyErr: errors.New("invalid signature")}
	good := &confirmingProviderStub{key: "easypay"}

	prov, key, n, err := verifyNotificationWithProvidersResolved(context.Background(), []payment.Provider{bad, good}, "x=1", nil)
	require.NoError(t, err)
	require.Equal(t, "easypay", key)
	require.NotNil(t, n)
	require.Same(t, good, prov)

	confirmer, ok := prov.(payment.NotificationConfirmer)
	require.True(t, ok)
	require.NoError(t, confirmer.ConfirmNotification(context.Background(), n))
	require.Equal(t, 1, good.confirmed)
	require.Equal(t, 0, bad.confirmed)
}
