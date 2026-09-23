package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// SSE 流内 event:error 的 error.type → 等价上游状态码映射。
// 过载/限流不能再被硬编码成 403(否则耗尽转移后回 "Upstream access forbidden")。
func TestSSEErrorEventStatusCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"overloaded", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529},
		{"rate_limit", `{"type":"error","error":{"type":"rate_limit_error","message":"..."}}`, 429},
		{"api_error", `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`, 500},
		{"authentication", `{"type":"error","error":{"type":"authentication_error","message":"..."}}`, 401},
		{"permission", `{"type":"error","error":{"type":"permission_error","message":"..."}}`, 403},
		{"unknown_type_keeps_403", `{"type":"error","error":{"type":"something_new","message":"..."}}`, 403},
		{"missing_type_keeps_403", `{"type":"error","error":{"message":"..."}}`, 403},
		{"empty_body_keeps_403", ``, 403},
		{"case_insensitive", `{"error":{"type":"Overloaded_Error"}}`, 529},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sseErrorEventStatusCode([]byte(tc.body)))
		})
	}
}
