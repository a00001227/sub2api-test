package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func newForgedTestEasyPay(t *testing.T, apiBase string, extra map[string]string) *EasyPay {
	t.Helper()
	cfg := map[string]string{
		"pid": "1000", "pkey": "MERCHANT_SECRET_KEY",
		"apiBase":   apiBase,
		"notifyUrl": "https://site.example.com/api/v1/payment/webhook/easypay",
		"returnUrl": "https://site.example.com/payment/result",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	e, err := NewEasyPay("inst-1", cfg)
	require.NoError(t, err)
	return e
}

// 公开漏洞 PoC(Wei-Shaw/sub2api#7881):把 trade_status 藏进 return_url,回放 popup 下单签名。
// 修复后必须验签失败。
func TestEasyPayVerifyNotification_RejectsForgedCallbackReusingCreateSignature(t *testing.T) {
	e := newForgedTestEasyPay(t, "https://pay.example.com", nil)

	returnURL := "https://site.example.com/payment/result?order_id=99&out_trade_no=ORDER123&status=success&trade_status=TRADE_SUCCESS"
	createParams := map[string]string{
		"pid": "1000", "type": "alipay", "out_trade_no": "ORDER123",
		"notify_url": e.config["notifyUrl"], "return_url": returnURL,
		"name": "balance recharge", "money": "650.00",
	}
	sign := easyPaySign(createParams, e.config["pkey"])

	prefix := "https://site.example.com/payment/result?order_id=99&out_trade_no=ORDER123&status=success"
	cb := url.Values{}
	cb.Set("pid", "1000")
	cb.Set("type", "alipay")
	cb.Set("out_trade_no", "ORDER123")
	cb.Set("notify_url", e.config["notifyUrl"])
	cb.Set("name", "balance recharge")
	cb.Set("money", "650.00")
	cb.Set("return_url", prefix)
	rawCallback := cb.Encode() + "&trade_status=TRADE_SUCCESS" + "&sign=" + sign + "&sign_type=MD5"

	// 前提自证:签名串确实能对上(漏洞成立),拦截必须来自参数名检查而不是签名碰巧不对
	forged := map[string]string{}
	for k := range cb {
		forged[k] = cb.Get(k)
	}
	forged["trade_status"] = "TRADE_SUCCESS"
	require.True(t, easyPayVerifySign(forged, e.config["pkey"], sign), "PoC 前提:伪造参数集的签名串应与下单签名一致")

	n, err := e.VerifyNotification(context.Background(), rawCallback, nil)
	require.Error(t, err)
	require.Nil(t, n)
	require.Contains(t, err.Error(), "request-only parameter")
}

// 真实回调(协议定义的参数集)照常通过。
func TestEasyPayVerifyNotification_AcceptsGenuineCallback(t *testing.T) {
	e := newForgedTestEasyPay(t, "https://pay.example.com", nil)
	params := map[string]string{
		"pid": "1000", "trade_no": "2026101012345", "out_trade_no": "ORDER123",
		"type": "alipay", "name": "balance recharge", "money": "650.00",
		"trade_status": "TRADE_SUCCESS",
	}
	sign := easyPaySign(params, e.config["pkey"])
	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	v.Set("sign", sign)
	v.Set("sign_type", "MD5")

	n, err := e.VerifyNotification(context.Background(), v.Encode(), nil)
	require.NoError(t, err)
	require.Equal(t, payment.ProviderStatusSuccess, n.Status)
	require.Equal(t, "ORDER123", n.OrderID)
	require.Equal(t, "2026101012345", n.TradeNo)
	require.InDelta(t, 650.0, n.Amount, 1e-9)
}

func successNotification() *payment.PaymentNotification {
	return &payment.PaymentNotification{OrderID: "ORDER123", TradeNo: "T1", Amount: 650, Status: payment.ProviderStatusSuccess}
}

// 查单确认:面板说已支付且金额一致 → 放行;未支付 / 金额不符 / 查单失败 → 拒绝。
func TestEasyPayConfirmNotification(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		wantErr string
	}{
		{"paid and amount matches", `{"code":1,"trade_status":"TRADE_SUCCESS","money":"650.00","trade_no":"T1"}`, 200, ""},
		{"paid via status=1", `{"code":1,"status":1,"money":"650.00"}`, 200, ""},
		{"not paid", `{"code":1,"trade_status":"TRADE_CLOSED","money":"650.00"}`, 200, "not paid"},
		{"order missing", `{"code":-1,"msg":"订单不存在"}`, 200, "not paid"},
		{"amount mismatch", `{"code":1,"trade_status":"TRADE_SUCCESS","money":"1.00"}`, 200, "amount mismatch"},
		{"panel error", `oops`, 500, "query failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotOutTradeNo, gotAct string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				gotOutTradeNo = r.Form.Get("out_trade_no")
				gotAct = r.Form.Get("act")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			e := newForgedTestEasyPay(t, srv.URL, nil)

			err := e.ConfirmNotification(context.Background(), successNotification())
			require.Equal(t, "order", gotAct)
			require.Equal(t, "ORDER123", gotOutTradeNo)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// 非成功回调不查单;skipQueryConfirm=true 显式关闭时不查单。
func TestEasyPayConfirmNotification_Skips(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := newForgedTestEasyPay(t, srv.URL, nil)
	failed := successNotification()
	failed.Status = payment.ProviderStatusFailed
	require.NoError(t, e.ConfirmNotification(context.Background(), failed))
	require.False(t, called)

	e2 := newForgedTestEasyPay(t, srv.URL, map[string]string{"skipQueryConfirm": "true"})
	require.NoError(t, e2.ConfirmNotification(context.Background(), successNotification()))
	require.False(t, called)

	var _ payment.NotificationConfirmer = e
}
