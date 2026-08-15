package service

import (
	"context"
	"time"
)

// Risk Phase 0（仅观测/影子模式）：每用户 Redis 特征草图。
//
// 在响应后的用量记录路径异步更新（绝不进入请求热路径）。评分 worker 定期读取
// 这些草图 + 免费计数器 + usage_logs 24h 聚合计算 score/tier。
//
// 隐私：草图只含计数与 64 位 simhash，绝不含 prompt/message 原文。

// RiskSketchTTL 是草图键的 TTL（24h 滚动窗口）。
const RiskSketchTTL = 24 * time.Hour

// RiskSketchUpdate 是一次请求产生的、要合入用户草图的增量（响应后计算）。
type RiskSketchUpdate struct {
	UserID       int64
	Simhash      uint64 // 0 表示无（不计入 sim 草图）
	SingleTurn   bool   // 是否单轮请求
	InputTokens  int64
	OutputTokens int64
	Model        string
}

// RiskSketchCache 是每用户风险特征草图的读写面（Redis 实现）。
// 键（均 24h TTL，聚合到 user_id）：
//
//	risk:sim:{uid}    — 近期 simhash 的有界集合（distinct/total 比值）
//	risk:turns:{uid}  — hash {single,total} 计数
//	risk:io:{uid}     — hash {input,output} 累计 token
//	risk:model:{uid}  — hash model→count 直方图
//	risk:count:{uid}  — 窗口内请求总数
//
// 全部更新用 pipeline，尽量廉价；在响应后调用。
type RiskSketchCache interface {
	// UpdateSketch 合入一次请求的增量（best-effort，错误吞掉/仅日志）。
	UpdateSketch(ctx context.Context, u RiskSketchUpdate)
	// ReadSketch 读取某用户的草图聚合（评分 worker 用）。
	ReadSketch(ctx context.Context, userID int64) (RiskSketchSnapshot, error)
	// ActiveUsers 返回窗口内有活动（risk:count 存在）的用户 ID（评分 worker 遍历用）。
	ActiveUsers(ctx context.Context) ([]int64, error)
	// StoreScore 缓存评分结果到 risk:user:{uid}（TTL）。
	StoreScore(ctx context.Context, userID int64, payload []byte, ttl time.Duration) error
}

// RiskSketchSnapshot 是评分 worker 从草图读到的聚合。
type RiskSketchSnapshot struct {
	DistinctSim   int
	TotalSim      int
	SingleTurn    int
	TotalTurns    int
	InputTokens   int64
	OutputTokens  int64
	RequestCount  int
	TopModelCount int
	ModelVariety  int
}

// updateRiskSketchAsync 在响应后异步更新用户草图（不阻塞任何用户可见路径）。
// 由 recordUsageCore 在完成计费/落库后调用；simhash 已在 buildRecordUsageLog 阶段
// 由 UsageLog.PromptSimhash 计算，这里复用之，避免重复计算。
func (s *GatewayService) updateRiskSketchAsync(usageLog *UsageLog, feat RiskUsageFeatures) {
	if s == nil || s.riskSketchCache == nil || usageLog == nil {
		return
	}
	var sim uint64
	if usageLog.PromptSimhash != nil {
		sim = uint64(*usageLog.PromptSimhash)
	}
	// 单轮判定：消息条数 <= 1。
	singleTurn := feat.MessageCount > 0 && feat.MessageCount <= 1
	u := RiskSketchUpdate{
		UserID:       usageLog.UserID,
		Simhash:      sim,
		SingleTurn:   singleTurn,
		InputTokens:  int64(usageLog.InputTokens + usageLog.CacheReadTokens + usageLog.CacheCreationTokens),
		OutputTokens: int64(usageLog.OutputTokens),
		Model:        usageLog.Model,
	}
	// 已在响应后（worker 池 background ctx 或同步 after-stream）；直接调用，
	// 缓存实现内部用 pipeline + 短超时，best-effort。
	go s.riskSketchCache.UpdateSketch(context.Background(), u)
}
