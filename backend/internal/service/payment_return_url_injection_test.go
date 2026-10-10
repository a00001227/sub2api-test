package service

import (
	"net/url"
	"testing"
)

// return_url 的用户 query 不得进入下单签名(Wei-Shaw/sub2api#7881 注入口)。
func TestReturnURL_UserQueryCannotReachSignedParams(t *testing.T) {
	canonical, err := CanonicalizeReturnURL("https://app.example.com/payment/result?trade_status=TRADE_SUCCESS&x=1", "app.example.com", "")
	if err != nil {
		t.Fatalf("CanonicalizeReturnURL error: %v", err)
	}
	if canonical != "https://app.example.com/payment/result" {
		t.Fatalf("user query must be dropped, got %q", canonical)
	}

	final, err := buildPaymentReturnURL("https://app.example.com/payment/result?trade_status=TRADE_SUCCESS", 99, "ORDER123", "tok")
	if err != nil {
		t.Fatalf("buildPaymentReturnURL error: %v", err)
	}
	parsed, err := url.Parse(final)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := parsed.Query()
	if q.Has("trade_status") {
		t.Fatalf("injected key leaked into return_url: %q", final)
	}
	for _, k := range []string{"order_id", "out_trade_no", "resume_token", "status"} {
		if !q.Has(k) {
			t.Fatalf("missing expected key %s in %q", k, final)
		}
	}
	if len(q) != 4 {
		t.Fatalf("return_url must carry exactly our 4 keys, got %v", q)
	}
}
