package service

import "strings"

// 工作道 / 号池(护号):一个组的流量路由到哪个 lane 的 cell。normal=好号池,
// batch=批量池,distillation=可牺牲/高危池。未知/空 → normal(fail-safe:绝不把
// 未知组静默当高危,也绝不漏进好号以外的池)。与 provider-portal cells/lane.ts 对齐。
const (
	GroupLaneNormal       = "normal"
	GroupLaneBatch        = "batch"
	GroupLaneDistillation = "distillation"
)

// normalizeGroupLane 归一工作道;写入前的单一真源(handler 已用 oneof 兜住取值,这里
// 再兜一层空值/大小写/历史值)。
func NormalizeGroupLane(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case GroupLaneBatch:
		return GroupLaneBatch
	case GroupLaneDistillation:
		return GroupLaneDistillation
	default:
		return GroupLaneNormal
	}
}
