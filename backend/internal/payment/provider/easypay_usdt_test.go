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
		{"usdc default", map[string]string{}, payment.TypeUSDC, "usdc"},
		{"usdc override", map[string]string{"usdcType": "usdc.solana"}, payment.TypeUSDC, "usdc.solana"},
		{"usdc does not borrow usdt code", map[string]string{"usdtType": "usdt.trc20"}, payment.TypeUSDC, "usdc"},
		{"usdt does not borrow usdc code", map[string]string{"usdcType": "usdc.solana"}, payment.TypeUSDT, "usdt"},
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

func TestEasyPaySupportedTypesIncludesUSDC(t *testing.T) {
	t.Parallel()

	e := &EasyPay{config: map[string]string{}}
	found := false
	for _, pt := range e.SupportedTypes() {
		if pt == payment.TypeUSDC {
			found = true
		}
	}
	if !found {
		t.Fatalf("SupportedTypes should include %q, got %v", payment.TypeUSDC, e.SupportedTypes())
	}
}

func TestEasyPayResolveCIDUSDC(t *testing.T) {
	t.Parallel()

	// usdc must NOT fall through to cidWxpay / cidUsdt.
	e := &EasyPay{config: map[string]string{"cidWxpay": "wx-cid", "cidUsdt": "usdt-cid", "cid": "default-cid"}}
	if got := e.resolveCID(payment.TypeUSDC); got != "default-cid" {
		t.Fatalf("resolveCID(usdc) without cidUsdc = %q, want %q", got, "default-cid")
	}
	e2 := &EasyPay{config: map[string]string{"cidUsdc": "usdc-cid", "cidUsdt": "usdt-cid", "cid": "default-cid"}}
	if got := e2.resolveCID(payment.TypeUSDC); got != "usdc-cid" {
		t.Fatalf("resolveCID(usdc) = %q, want %q", got, "usdc-cid")
	}
	if got := e2.resolveCID(payment.TypeUSDT); got != "usdt-cid" {
		t.Fatalf("resolveCID(usdt) = %q, want %q (regression)", got, "usdt-cid")
	}
}

func TestIsStablecoinType(t *testing.T) {
	t.Parallel()
	for _, pt := range []string{payment.TypeUSDT, payment.TypeUSDC} {
		if !payment.IsStablecoinType(pt) {
			t.Fatalf("IsStablecoinType(%q) should be true", pt)
		}
	}
	for _, pt := range []string{payment.TypeAlipay, payment.TypeWxpay, payment.TypeStripe, ""} {
		if payment.IsStablecoinType(pt) {
			t.Fatalf("IsStablecoinType(%q) should be false", pt)
		}
	}
}
