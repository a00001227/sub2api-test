package service

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// evidencePromptSimhash 对请求的「最新一条消息」算归一化 64 位 simhash，用于前端「模板重复」红标。
//
// 只取最新一条（而非整个 messages 数组）：多轮对话每轮都带完整历史，若对整个数组算 simhash，
// 会因共享的历史前缀（simhash 只取前 8KB）而把「同一对话的连续多轮」误判为重复。蒸馏的特征是
// 「同一提示模板反复发」——体现在最新用户轮；正常对话每轮最新消息不同，故只哈希最新一条最准。
func evidencePromptSimhash(body []byte) uint64 {
	if last := lastMessageRaw(body); last != "" {
		return ComputeMessagesSimhash([]byte(last))
	}
	return ComputeMessagesSimhash(body)
}

// lastMessageRaw 取 messages / contents 数组的最后一条（最新用户轮）的原始 JSON；无则空。
func lastMessageRaw(body []byte) string {
	for _, field := range []string{"messages", "contents"} {
		if arr := gjson.GetBytes(body, field); arr.IsArray() {
			items := arr.Array()
			if len(items) > 0 {
				return items[len(items)-1].Raw
			}
		}
	}
	return ""
}

var (
	// ErrEvidenceUnavailable 取证捕获未启用（store 缺失 / master 关）。
	ErrEvidenceUnavailable = errors.New("evidence_capture: unavailable")
	// ErrEvidenceBadRequest 入参非法（target 类型/ID/数量）。
	ErrEvidenceBadRequest = errors.New("evidence_capture: bad request")
)

// 疑似蒸馏取证：请求原文捕获（仅观测取证，与计费/风控解耦）。
//
// ⚠️ 这是 Risk 系「只存指纹、绝不存原文」隐私原则的【唯一、刻意的例外】：
// 仅当管理员显式标记某 user/key 时，才捕获其后续 N 条请求的原文（脱敏 + 限大小）供取证，
// 抓够 N 条自动停、管理员查看/导出后手动清除、Redis 兜底 TTL 自动过期。前提是服务条款已声明
// 「疑似滥用可留存请求内容用于取证」。默认无 flag = 不采、热路径零开销。只抓请求，不抓响应。

// EvidenceTargetType 捕获目标类型。
type EvidenceTargetType string

const (
	EvidenceTargetUser EvidenceTargetType = "user"
	EvidenceTargetKey  EvidenceTargetType = "key"

	evidenceMaxCountLimitDefault = 500
	evidenceBufferTTLDefault     = 7 * 24 * time.Hour
	evidenceMaxBodyBytesDefault  = 16 * 1024
	evidenceStoreOpTimeout       = 3 * time.Second
)

// EvidenceFlag 一条捕获名单项（某 user/key 还要抓几条）。
type EvidenceFlag struct {
	TargetKey  string `json:"target_key"` // "u:<id>" 或 "k:<id>"
	TargetType string `json:"target_type"`
	TargetID   int64  `json:"target_id"`
	Remaining  int    `json:"remaining"`
	Max        int    `json:"max"`
	StartedAt  int64  `json:"started_at"`
	AdminID    int64  `json:"admin_id"`
}

// EvidenceEntry 一条已捕获的请求证据（body 已脱敏 + 限大小；绝不含 secrets）。
type EvidenceEntry struct {
	Ts        int64  `json:"ts"`
	UserID    int64  `json:"user_id"`
	APIKeyID  int64  `json:"api_key_id"`
	RequestID string `json:"request_id"`    // 平台生成的 client_request_id（跨日志关联）
	Model     string `json:"model"`
	Endpoint  string `json:"endpoint"`
	IP        string `json:"ip"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated"`
	// PromptSimhash 提示词归一化 64 位 simhash（hex）。同模板不同数字会坍缩成同值，
	// 前端据此把「模板重复」的条目标红（批量蒸馏的典型特征）。空/无消息为 "0"。
	PromptSimhash string `json:"prompt_simhash"`
}

// CaptureMeta 捕获时的请求元信息（由 handler 从 gin.Context 提取，service 不碰 gin）。
type CaptureMeta struct {
	Model     string
	Endpoint  string
	IP        string
	RequestID string
}

// EvidenceCaptureStore 由 repository 实现（Redis）。
type EvidenceCaptureStore interface {
	LoadFlags(ctx context.Context) ([]EvidenceFlag, error)
	SaveFlag(ctx context.Context, f EvidenceFlag) error
	DeleteFlag(ctx context.Context, targetKey string) error
	AppendEvidence(ctx context.Context, targetKey string, e EvidenceEntry, capN int, ttl time.Duration) error
	ListEvidence(ctx context.Context, targetKey string, limit int) ([]EvidenceEntry, error)
	PurgeEvidence(ctx context.Context, targetKey string) error
}

// EvidenceCaptureConfigView 运行参数（从 config 提取）。
type EvidenceCaptureConfigView struct {
	Enabled       bool
	MaxCountLimit int
	BufferTTL     time.Duration
	MaxBodyBytes  int
}

// EvidenceCaptureService 管理捕获名单 + 热路径捕获。
type EvidenceCaptureService struct {
	store EvidenceCaptureStore
	cfg   EvidenceCaptureConfigView

	mu          sync.Mutex
	flags       map[string]EvidenceFlag // target_key → flag
	activeCount int32                   // atomic：>0 才需要检查（零开销闸门）
}

// NewEvidenceCaptureService 构造并从 Redis 载入既有捕获名单（重启可恢复）。store 为 nil → 返回禁用态服务。
func NewEvidenceCaptureService(store EvidenceCaptureStore, cfg EvidenceCaptureConfigView) *EvidenceCaptureService {
	if cfg.MaxCountLimit <= 0 {
		cfg.MaxCountLimit = evidenceMaxCountLimitDefault
	}
	if cfg.BufferTTL <= 0 {
		cfg.BufferTTL = evidenceBufferTTLDefault
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = evidenceMaxBodyBytesDefault
	}
	s := &EvidenceCaptureService{store: store, cfg: cfg, flags: make(map[string]EvidenceFlag)}
	if store != nil && cfg.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), evidenceStoreOpTimeout)
		defer cancel()
		if flags, err := store.LoadFlags(ctx); err == nil {
			for _, f := range flags {
				if f.Remaining > 0 {
					s.flags[f.TargetKey] = f
				}
			}
			atomic.StoreInt32(&s.activeCount, int32(len(s.flags)))
		}
	}
	return s
}

func userTargetKey(id int64) string { return "u:" + strconv.FormatInt(id, 10) }
func keyTargetKey(id int64) string  { return "k:" + strconv.FormatInt(id, 10) }

// available 表示服务是否可用（store 存在 + master 开）。
func (s *EvidenceCaptureService) available() bool {
	return s != nil && s.store != nil && s.cfg.Enabled
}

// Active 是热路径零开销闸门：当前有活跃捕获名单才为 true（一次原子读）。
// handler 据此在提取 meta / 调 CaptureIfFlagged 前短路，默认无 flag 时不做任何解析。
func (s *EvidenceCaptureService) Active() bool {
	return s != nil && atomic.LoadInt32(&s.activeCount) > 0
}

// CaptureIfFlagged 热路径：命中捕获名单则异步脱敏+存储；默认无 flag 时一次原子读即返回（零开销）。
// 同步只做闸门 + 计数扣减（精确抓 N 条），脱敏与 Redis 写入放异步 goroutine（不加请求延迟）。
func (s *EvidenceCaptureService) CaptureIfFlagged(userID, apiKeyID int64, rawBody []byte, meta CaptureMeta) {
	if s == nil || atomic.LoadInt32(&s.activeCount) == 0 {
		return // 零开销短路
	}
	if !s.available() || len(rawBody) == 0 {
		return
	}

	s.mu.Lock()
	// key 级更具体，优先；否则 user 级。
	tk := ""
	if f, ok := s.flags[keyTargetKey(apiKeyID)]; ok && f.Remaining > 0 {
		tk = f.TargetKey
	} else if f, ok := s.flags[userTargetKey(userID)]; ok && f.Remaining > 0 {
		tk = f.TargetKey
	}
	if tk == "" {
		s.mu.Unlock()
		return
	}
	f := s.flags[tk]
	f.Remaining--
	capN := f.Max
	reachedZero := f.Remaining <= 0
	if reachedZero {
		delete(s.flags, tk)
		atomic.AddInt32(&s.activeCount, -1)
	} else {
		s.flags[tk] = f
	}
	s.mu.Unlock()

	// body 复制一份（调用方缓冲可能在返回后复用）；脱敏+存储走异步，不阻塞请求。
	bodyCopy := append([]byte(nil), rawBody...)
	go func() {
		san, trunc, _ := sanitizeAndTrimJSONPayload(bodyCopy, s.cfg.MaxBodyBytes)
		entry := EvidenceEntry{
			Ts: time.Now().Unix(), UserID: userID, APIKeyID: apiKeyID, RequestID: meta.RequestID,
			Model: meta.Model, Endpoint: meta.Endpoint, IP: meta.IP,
			Body: san, Truncated: trunc,
			PromptSimhash: strconv.FormatUint(evidencePromptSimhash(bodyCopy), 16),
		}
		ctx, cancel := context.WithTimeout(context.Background(), evidenceStoreOpTimeout)
		defer cancel()
		if err := s.store.AppendEvidence(ctx, tk, entry, capN, s.cfg.BufferTTL); err != nil {
			logger.L().With(zap.String("component", "service.evidence_capture")).
				Warn("append evidence failed", zap.String("target", tk), zap.Error(err))
		}
		if reachedZero {
			_ = s.store.DeleteFlag(ctx, tk)
		} else {
			_ = s.store.SaveFlag(ctx, f)
		}
	}()
}

// StartCapture 开始/重置对某 target 的捕获（管理员操作，审计）。
func (s *EvidenceCaptureService) StartCapture(ctx context.Context, targetType EvidenceTargetType, targetID int64, maxCount int, adminID int64) (EvidenceFlag, error) {
	if !s.available() {
		return EvidenceFlag{}, ErrEvidenceUnavailable
	}
	if targetID <= 0 || (targetType != EvidenceTargetUser && targetType != EvidenceTargetKey) {
		return EvidenceFlag{}, ErrEvidenceBadRequest
	}
	if maxCount <= 0 {
		return EvidenceFlag{}, ErrEvidenceBadRequest
	}
	if maxCount > s.cfg.MaxCountLimit {
		maxCount = s.cfg.MaxCountLimit
	}
	var tk string
	if targetType == EvidenceTargetUser {
		tk = userTargetKey(targetID)
	} else {
		tk = keyTargetKey(targetID)
	}
	f := EvidenceFlag{
		TargetKey: tk, TargetType: string(targetType), TargetID: targetID,
		Remaining: maxCount, Max: maxCount, StartedAt: time.Now().Unix(), AdminID: adminID,
	}
	if err := s.store.SaveFlag(ctx, f); err != nil {
		return EvidenceFlag{}, err
	}
	s.mu.Lock()
	_, existed := s.flags[tk]
	s.flags[tk] = f
	if !existed {
		atomic.AddInt32(&s.activeCount, 1)
	}
	s.mu.Unlock()
	logger.L().With(zap.String("component", "service.evidence_capture")).
		Info("capture started", zap.String("target", tk), zap.Int("max", maxCount), zap.Int64("admin_id", adminID))
	return f, nil
}

// ListActiveCaptures 返回当前活跃捕获名单（内存快照）。
func (s *EvidenceCaptureService) ListActiveCaptures() []EvidenceFlag {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EvidenceFlag, 0, len(s.flags))
	for _, f := range s.flags {
		out = append(out, f)
	}
	return out
}

// ListEvidence 取某 target 已捕获的证据条目。
func (s *EvidenceCaptureService) ListEvidence(ctx context.Context, targetKey string, limit int) ([]EvidenceEntry, error) {
	if !s.available() {
		return nil, ErrEvidenceUnavailable
	}
	if limit <= 0 || limit > s.cfg.MaxCountLimit {
		limit = s.cfg.MaxCountLimit
	}
	return s.store.ListEvidence(ctx, targetKey, limit)
}

// PurgeEvidence 清除某 target 的证据 + 停止捕获（管理员操作，审计）。
func (s *EvidenceCaptureService) PurgeEvidence(ctx context.Context, targetKey string, adminID int64) error {
	if !s.available() {
		return ErrEvidenceUnavailable
	}
	if err := s.store.PurgeEvidence(ctx, targetKey); err != nil {
		return err
	}
	_ = s.store.DeleteFlag(ctx, targetKey)
	s.mu.Lock()
	if _, ok := s.flags[targetKey]; ok {
		delete(s.flags, targetKey)
		atomic.AddInt32(&s.activeCount, -1)
	}
	s.mu.Unlock()
	logger.L().With(zap.String("component", "service.evidence_capture")).
		Info("capture purged", zap.String("target", targetKey), zap.Int64("admin_id", adminID))
	return nil
}
