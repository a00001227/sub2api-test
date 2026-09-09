package provider

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestEasyPaySupportedTypesIncludesUSDT(t *testing.T) {
	t.Parallel()

	e := &EasyPay{config: map[string]string{}}
	found := false
	for _, pt := range e.SupportedTypes() {
		if pt == payment.TypeUSDT {
			found = true
		}
	}
	if !found {
		t.Fatalf("SupportedTypes should include %q, got %v", payment.TypeUSDT, e.SupportedTypes())
	}
}

func TestEasyPayResolveType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   map[string]string
		input    string
		expected string
	}{
		{"usdt default", map[string]string{}, payment.TypeUSDT, "usdt"},
		{"usdt override", map[string]string{"usdtType": "trc20"}, payment.TypeUSDT, "trc20"},
		{"usdt override with whitespace", map[string]string{"usdtType": "  usdt_trc20  "}, payment.TypeUSDT, "usdt_trc20"},
		{"usdt empty override falls back", map[string]string{"usdtType": "   "}, payment.TypeUSDT, "usdt"},
		{"alipay untouched", map[string]string{"usdtType": "trc20"}, payment.TypeAlipay, "alipay"},
		{"wxpay untouched", map[string]string{"usdtType": "trc20"}, payment.TypeWxpay, "wxpay"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &EasyPay{config: tc.config}
			if got := e.resolveType(tc.input); got != tc.expected {
				t.Fatalf("resolveType(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestEasyPayResolveCIDUSDT(t *testing.T) {
	t.Parallel()

	// usdt must NOT fall through to cidWxpay.
	e := &EasyPay{config: map[string]string{"cidWxpay": "wx-cid", "cid": "default-cid"}}
	if got := e.resolveCID(payment.TypeUSDT); got != "default-cid" {
		t.Fatalf("resolveCID(usdt) with only cidWxpay/cid = %q, want %q (must not use cidWxpay)", got, "default-cid")
	}

	// usdt prefers cidUsdt when set.
	e2 := &EasyPay{config: map[string]string{"cidUsdt": "usdt-cid", "cidWxpay": "wx-cid", "cid": "default-cid"}}
	if got := e2.resolveCID(payment.TypeUSDT); got != "usdt-cid" {
		t.Fatalf("resolveCID(usdt) = %q, want %q", got, "usdt-cid")
	}

	// wxpay still resolves to cidWxpay (regression guard).
	if got := e2.resolveCID(payment.TypeWxpay); got != "wx-cid" {
		t.Fatalf("resolveCID(wxpay) = %q, want %q", got, "wx-cid")
	}
}
