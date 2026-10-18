package service

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// 风控白名单(用户 + API 分组),三条线共用一份,存在内容审计配置里
// (allowed_user_ids / allowed_group_ids):
//   - 内容审核:命中直接放行,不解析正文、不记录;
//   - 提示词审计:命中不留存原文;
//   - 蒸馏检测(Risk V2):命中不进评分队列。
//
// 加入白名单的那一刻把该用户(或该分组下所有用户)已存下的三类数据一并清掉:
// 提示词留存、审核命中记录、蒸馏评分行。白名单的语义是「彻底不留」,不是「不显示」。

// RiskAllowlistChecker 供提示词审计 / Risk V2 入队判断是否跳过。
type RiskAllowlistChecker interface {
	IsAllowlisted(ctx context.Context, userID int64, groupID *int64) bool
}

// riskAllowlistCacheTTL 热路径上的白名单快照缓存:配置改动后最多这么久生效
// (UpdateConfig 会直接失效缓存,正常是立刻生效)。
const riskAllowlistCacheTTL = 5 * time.Second

type riskAllowlistSnapshot struct {
	users  map[int64]struct{}
	groups map[int64]struct{}
	at     time.Time
}

func newRiskAllowlistSnapshot(userIDs, groupIDs []int64, now time.Time) *riskAllowlistSnapshot {
	s := &riskAllowlistSnapshot{
		users:  make(map[int64]struct{}, len(userIDs)),
		groups: make(map[int64]struct{}, len(groupIDs)),
		at:     now,
	}
	for _, id := range userIDs {
		s.users[id] = struct{}{}
	}
	for _, id := range groupIDs {
		s.groups[id] = struct{}{}
	}
	return s
}

func (s *riskAllowlistSnapshot) hit(userID int64, groupID *int64) bool {
	if s == nil {
		return false
	}
	if userID > 0 {
		if _, ok := s.users[userID]; ok {
			return true
		}
	}
	if groupID != nil && *groupID > 0 {
		if _, ok := s.groups[*groupID]; ok {
			return true
		}
	}
	return false
}

// IsAllowlisted 用户或其分组在风控白名单内。读配置带 5s 缓存;读失败按「不在白名单」处理
// (fail-closed:宁可多审一次,不能因为配置读不到就漏审)。
func (s *ContentModerationService) IsAllowlisted(ctx context.Context, userID int64, groupID *int64) bool {
	if s == nil {
		return false
	}
	if userID <= 0 && (groupID == nil || *groupID <= 0) {
		return false
	}
	now := time.Now()
	if snap := s.allowlistCache.Load(); snap != nil && now.Sub(snap.at) < riskAllowlistCacheTTL {
		return snap.hit(userID, groupID)
	}
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		slog.Warn("risk_allowlist.load_config_failed", "error", err)
		return false
	}
	snap := newRiskAllowlistSnapshot(cfg.AllowedUserIDs, cfg.AllowedGroupIDs, now)
	s.allowlistCache.Store(snap)
	return snap.hit(userID, groupID)
}

// SetAllowlistCleanupDeps 注入「加入白名单即清理」要动的另外两张表(提示词留存、蒸馏评分)。
// 任一为 nil 时对应清理跳过,不影响白名单生效本身。
func (s *ContentModerationService) SetAllowlistCleanupDeps(promptAudit PromptAuditRepository, userRisk UserRiskV2Repository) {
	if s == nil {
		return
	}
	s.promptAuditRepo = promptAudit
	s.userRiskRepo = userRisk
}

// RiskAllowlistPurgeResult 一次清理的计数(写日志,也回给后台)。
type RiskAllowlistPurgeResult struct {
	Users             int   `json:"users"`
	Groups            int   `json:"groups"`
	ModerationLogs    int64 `json:"moderation_logs"`
	PromptAuditEvents int64 `json:"prompt_audit_events"`
	RiskAssessments   int64 `json:"risk_assessments"`
}

// purgeAllowlisted 清掉新加入白名单的用户 / 分组已有的留存数据。best-effort:某一张表失败
// 只记日志,不回滚配置(白名单已经生效,留存数据最晚也会按保留期过期)。
func (s *ContentModerationService) purgeAllowlisted(ctx context.Context, addedUsers, addedGroups []int64) *RiskAllowlistPurgeResult {
	res := &RiskAllowlistPurgeResult{Users: len(addedUsers), Groups: len(addedGroups)}
	if len(addedUsers) == 0 && len(addedGroups) == 0 {
		return res
	}

	// 分组 → 该分组下所有用户(按 API key 归属);评分表只按用户存,所以要先展开。
	userSet := make(map[int64]struct{}, len(addedUsers))
	for _, id := range addedUsers {
		userSet[id] = struct{}{}
	}
	if len(addedGroups) > 0 && s.repo != nil {
		ids, err := s.repo.ListUserIDsByGroupIDs(ctx, addedGroups)
		if err != nil {
			slog.Warn("risk_allowlist.purge.list_group_users_failed", "groups", addedGroups, "error", err)
		}
		for _, id := range ids {
			userSet[id] = struct{}{}
		}
	}
	allUsers := make([]int64, 0, len(userSet))
	for id := range userSet {
		allUsers = append(allUsers, id)
	}

	if s.repo != nil {
		if len(allUsers) > 0 {
			if n, err := s.repo.DeleteLogsByUserIDs(ctx, allUsers); err != nil {
				slog.Warn("risk_allowlist.purge.moderation_logs_by_user_failed", "error", err)
			} else {
				res.ModerationLogs += n
			}
		}
		if len(addedGroups) > 0 {
			if n, err := s.repo.DeleteLogsByGroupIDs(ctx, addedGroups); err != nil {
				slog.Warn("risk_allowlist.purge.moderation_logs_by_group_failed", "error", err)
			} else {
				res.ModerationLogs += n
			}
		}
	}
	if s.promptAuditRepo != nil {
		if len(allUsers) > 0 {
			if n, err := s.promptAuditRepo.DeleteEventsByUserIDs(ctx, allUsers); err != nil {
				slog.Warn("risk_allowlist.purge.prompt_audit_by_user_failed", "error", err)
			} else {
				res.PromptAuditEvents += n
			}
		}
		if len(addedGroups) > 0 {
			if n, err := s.promptAuditRepo.DeleteEventsByGroupIDs(ctx, addedGroups); err != nil {
				slog.Warn("risk_allowlist.purge.prompt_audit_by_group_failed", "error", err)
			} else {
				res.PromptAuditEvents += n
			}
		}
	}
	if s.userRiskRepo != nil {
		for _, id := range allUsers {
			if err := s.userRiskRepo.DeleteByUserID(ctx, id); err != nil {
				slog.Warn("risk_allowlist.purge.user_risk_v2_failed", "user_id", id, "error", err)
				continue
			}
			res.RiskAssessments++
		}
	}
	slog.Info("risk_allowlist.purged",
		"users", res.Users, "groups", res.Groups, "expanded_users", len(allUsers),
		"moderation_logs", res.ModerationLogs, "prompt_audit_events", res.PromptAuditEvents, "risk_assessments", res.RiskAssessments)
	return res
}

// diffAddedInt64 返回 next 中有、prev 中没有的 ID(保序)。
func diffAddedInt64(prev, next []int64) []int64 {
	seen := make(map[int64]struct{}, len(prev))
	for _, id := range prev {
		seen[id] = struct{}{}
	}
	out := make([]int64, 0)
	for _, id := range next {
		if _, ok := seen[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// allowlistCachePtr 让 struct 字段声明处不用引入 atomic 包名冲突;与 content_moderation.go 同包。
type allowlistCachePtr = atomic.Pointer[riskAllowlistSnapshot]
