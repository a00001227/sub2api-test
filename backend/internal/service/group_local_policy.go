package service

import "strings"

// 本地渠道策略:中央收到某组的请求后,在「转发到 cell」与「用该组在中央本地绑定的账号
// (如第三方中转站 API Key)」之间怎么分。
//   - inherit    默认。沿用 EDGE_FORWARD_GROUPS:组在名单里 = cell_only,不在 = local_only。
//   - cell_only  只转 cell;本地账号不参与。
//   - local_only 只走本地;不转 cell。
//   - cell_first 先转 cell;所有 cell 都没号 / 无可路由时兜底到本地。
//   - local_first 先查本地有没有能接的账号,有就本地跑;没有才转 cell。
//   - balanced   每条请求按 LocalWeight(0-100,%)随机先走本地或 cell,走不通换另一边。
const (
	GroupLocalPolicyInherit    = "inherit"
	GroupLocalPolicyCellOnly   = "cell_only"
	GroupLocalPolicyLocalOnly  = "local_only"
	GroupLocalPolicyCellFirst  = "cell_first"
	GroupLocalPolicyLocalFirst = "local_first"
	GroupLocalPolicyBalanced   = "balanced"
)

// NormalizeGroupLocalPolicy 归一策略值;未知/空 → inherit(行为与改动前完全一致)。
func NormalizeGroupLocalPolicy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case GroupLocalPolicyCellOnly:
		return GroupLocalPolicyCellOnly
	case GroupLocalPolicyLocalOnly:
		return GroupLocalPolicyLocalOnly
	case GroupLocalPolicyCellFirst:
		return GroupLocalPolicyCellFirst
	case GroupLocalPolicyLocalFirst:
		return GroupLocalPolicyLocalFirst
	case GroupLocalPolicyBalanced:
		return GroupLocalPolicyBalanced
	default:
		return GroupLocalPolicyInherit
	}
}

// NormalizeGroupLocalWeight 夹到 0-100。
func NormalizeGroupLocalWeight(w int) int {
	if w < 0 {
		return 0
	}
	if w > 100 {
		return 100
	}
	return w
}
