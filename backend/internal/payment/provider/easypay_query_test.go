package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestEasyPayQueryOrderStatusMapping(t *testing.T) {
	t.Parallel()

	const orderID = "order-123"
	tests := []struct {
		name        string
		body        string
		wantErr     string
		wantStatus  string
		wantTradeNo string
		wantAmount  float64
	}{
		{
			name:        "top level trade success is paid",
			body:        `{"code":1,"trade_status":"TRADE_SUCCESS","status":0,"money":"12.34","trade_no":"gateway-123"}`,
			wantStatus:  payment.ProviderStatusPaid,
			wantTradeNo: "gateway-123",
			wantAmount:  12.34,
		},
		{
			name:        "waiting trade status with paid numeric status stays pending",
			body:        `{"code":1,"trade_status":"WAITING","status":1,"money":"12.34","trade_no":"gateway-123"}`,
			wantStatus:  payment.ProviderStatusPending,
			wantTradeNo: "gateway-123",
			wantAmount:  12.34,
		},
		{
			name:        "empty trade status with paid numeric status stays pending",
			body:        `{"code":1,"trade_status":"","status":1,"money":"12.34"}`,
			wantStatus:  payment.ProviderStatusPending,
			wantTradeNo: orderID,
			wantAmount:  12.34,
		},
		{
			name:        "nested data trade success is paid",
			body:        `{"code":1,"data":{"trade_status":"TRADE_SUCCESS","status":0,"money":"9.99","trade_no":"data-456"}}`,
			wantStatus:  payment.ProviderStatusPaid,
			wantTradeNo: "data-456",
			wantAmount:  9.99,
		},
		{
			name:        "legacy numeric paid status remains compatible",
			body:        `{"code":1,"status":1,"money":"3.21"}`,
			wantStatus:  payment.ProviderStatusPaid,
			wantTradeNo: orderID,
			wantAmount:  3.21,
		},
		{
			name:        "legacy numeric non paid status is pending",
			body:        `{"code":1,"status":0,"money":"3.21"}`,
			wantStatus:  payment.ProviderStatusPending,
			wantTradeNo: orderID,
			wantAmount:  3.21,
		},
		{
			// 2026-10-10 生产事故回归:面板不认请求(如 POST 查单被回 No Act!)时
			// 必须报错,不能静默当「未支付」误伤 ConfirmNotification 的真回调。
			name:    "panel rejection surfaces as error not pending",
			body:    `{"code":-5,"msg":"No Act!"}`,
			wantErr: "No Act!",
		},
		{
			name:    "query failure with missing status is an error",
			body:    `{"code":0,"msg":"订单不存在"}`,
			wantErr: "订单不存在",
		},
		{
			// 字符串化数字的面板变体:code/status 都是字符串也要能解析。
			name:        "string typed code and status are tolerated",
			body:        `{"code":"1","status":"1","money":"12.34","trade_no":"gateway-str"}`,
			wantStatus:  payment.ProviderStatusPaid,
			wantTradeNo: "gateway-str",
			wantAmount:  12.34,
		},
		{
			name:        "numeric money is tolerated",
			body:        `{"code":1,"trade_status":"TRADE_SUCCESS","money":12.34}`,
			wantStatus:  payment.ProviderStatusPaid,
			wantTradeNo: orderID,
			wantAmount:  12.34,
		},
		{
			name:        "missing fields are pending",
			body:        `{}`,
			wantStatus:  payment.ProviderStatusPending,
			wantTradeNo: orderID,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotMethod string
			var gotQuery url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				if r.URL.Path != "/api.php" {
					t.Errorf("path = %q, want /api.php", r.URL.Path)
				}
				gotQuery = r.URL.Query()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			provider := newTestEasyPay(t, server.URL)
			resp, err := provider.QueryOrder(context.Background(), orderID)

			// 自建/彩虹系面板的 api.php 只读 $_GET,查单必须 GET + 参数全在 URL query。
			if gotMethod != http.MethodGet {
				t.Fatalf("method = %q, want %q", gotMethod, http.MethodGet)
			}
			for key, want := range map[string]string{
				"act":          "order",
				"pid":          "pid-1",
				"key":          "pkey-1",
				"out_trade_no": orderID,
			} {
				if got := gotQuery.Get(key); got != want {
					t.Fatalf("query[%s] = %q, want %q (query=%v)", key, got, want, gotQuery)
				}
			}

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("QueryOrder error = nil, want containing %q (response=%+v)", tt.wantErr, resp)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("QueryOrder error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("QueryOrder returned error: %v", err)
			}
			if resp.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (response=%+v)", resp.Status, tt.wantStatus, resp)
			}
			if resp.TradeNo != tt.wantTradeNo {
				t.Fatalf("trade_no = %q, want %q", resp.TradeNo, tt.wantTradeNo)
			}
			if resp.Amount != tt.wantAmount {
				t.Fatalf("amount = %v, want %v", resp.Amount, tt.wantAmount)
			}
		})
	}
}
