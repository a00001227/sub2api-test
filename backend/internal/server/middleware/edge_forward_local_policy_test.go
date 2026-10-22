package middleware

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestEffectiveLocalPolicy(t *testing.T) {
	cases := []struct {
		policy string
		inSet  bool
		want   string
	}{
		{"", true, service.GroupLocalPolicyCellOnly},
		{"", false, service.GroupLocalPolicyLocalOnly},
		{"inherit", true, service.GroupLocalPolicyCellOnly},
		{"garbage", false, service.GroupLocalPolicyLocalOnly},
		{"cell_first", false, service.GroupLocalPolicyCellFirst},
		{"LOCAL_FIRST", true, service.GroupLocalPolicyLocalFirst},
		{"balanced", true, service.GroupLocalPolicyBalanced},
		{"local_only", true, service.GroupLocalPolicyLocalOnly},
		{"cell_only", false, service.GroupLocalPolicyCellOnly},
	}
	for _, c := range cases {
		got := effectiveLocalPolicy(&service.Group{LocalPolicy: c.policy}, c.inSet)
		if got != c.want {
			t.Fatalf("policy=%q inSet=%v: got %q want %q", c.policy, c.inSet, got, c.want)
		}
	}
	if service.NormalizeGroupLocalWeight(-5) != 0 || service.NormalizeGroupLocalWeight(150) != 100 || service.NormalizeGroupLocalWeight(30) != 30 {
		t.Fatal("weight clamp")
	}
}
