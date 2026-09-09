package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestParsePaymentConfigUSDTMultiplier(t *testing.T) {
	t.Parallel()

	svc := &PaymentConfigService{}

	t.Run("defaults to 1.0 when unset", func(t *testing.T) {
		t.Parallel()
		cfg := svc.parsePaymentConfig(map[string]string{})
		if cfg.USDTRechargeMultiplier != defaultBalanceRechargeMultiplier {
			t.Fatalf("USDTRechargeMultiplier = %v, want %v", cfg.USDTRechargeMultiplier, defaultBalanceRechargeMultiplier)
		}
	})

	t.Run("parses configured value independent of global multiplier", func(t *testing.T) {
		t.Parallel()
		cfg := svc.parsePaymentConfig(map[string]string{
			SettingBalanceRechargeMult:     "0.14",
			SettingBalanceRechargeMultUSDT: "1.00",
		})
		if cfg.BalanceRechargeMultiplier != 0.14 {
			t.Fatalf("BalanceRechargeMultiplier = %v, want 0.14", cfg.BalanceRechargeMultiplier)
		}
		if cfg.USDTRechargeMultiplier != 1.0 {
			t.Fatalf("USDTRechargeMultiplier = %v, want 1.0", cfg.USDTRechargeMultiplier)
		}
	})
}

func TestPaymentProviderConfigCurrencyEasyPay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		providerKey string
		cfg         map[string]string
		want        string
	}{
		{"easypay no currency → CNY", payment.TypeEasyPay, map[string]string{}, payment.DefaultPaymentCurrency},
		{"easypay empty currency → CNY", payment.TypeEasyPay, map[string]string{"currency": ""}, payment.DefaultPaymentCurrency},
		{"easypay USD → USD", payment.TypeEasyPay, map[string]string{"currency": "USD"}, "USD"},
		{"easypay invalid currency → CNY", payment.TypeEasyPay, map[string]string{"currency": "USDT"}, payment.DefaultPaymentCurrency},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := paymentProviderConfigCurrency(tc.providerKey, tc.cfg); got != tc.want {
				t.Fatalf("paymentProviderConfigCurrency(%q, %v) = %q, want %q", tc.providerKey, tc.cfg, got, tc.want)
			}
		})
	}
}
