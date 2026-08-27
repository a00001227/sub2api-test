package middleware

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// validateAPIKeyGroupAllowed 承载护号焊池的运行时闸:用户只能用与自身 lane 相同的组。
// 它在每个请求都跑(forced-group 解析之后),覆盖账号密钥+客户密钥+无前缀路径。
func TestValidateAPIKeyGroupAllowed_LaneConfinement(t *testing.T) {
	gid := int64(1)
	mk := func(userLane, groupLane string) *service.APIKey {
		return &service.APIKey{
			GroupID: &gid,
			User:    &service.User{Lane: userLane},
			Group:   &service.Group{ID: 1, Lane: groupLane}, // 非专属公开组
		}
	}
	cases := []struct {
		name        string
		userLane    string
		groupLane   string
		wantAllowed bool
	}{
		{"同道-蒸馏放行", "distillation", "distillation", true},
		{"蒸馏用户用好号组-拒绝", "distillation", "normal", false},
		{"normal用户用蒸馏组-拒绝", "normal", "distillation", false},
		{"空lane都归一normal-放行", "", "", true},
		{"批量用户用批量组-放行", "batch", "batch", true},
		{"批量用户用蒸馏组-拒绝", "batch", "distillation", false},
	}
	for _, c := range cases {
		if got := validateAPIKeyGroupAllowed(mk(c.userLane, c.groupLane)); got != c.wantAllowed {
			t.Fatalf("%s: user=%q group=%q allowed=%v, want %v", c.name, c.userLane, c.groupLane, got, c.wantAllowed)
		}
	}
}
