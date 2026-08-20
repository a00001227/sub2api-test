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

// 疑似蒸馏取证：按「重复模板」聚合捕获（仅观测取证，与计费/风控解耦）。
//
// ⚠️ 这是 Risk 系「只存指纹、绝不存原文」隐私原则的【唯一、刻意的例外】：仅当管理员显式标记
// 某 user/key 时，对其请求算提示词 simhash；只有**同一模板重复出现≥阈值**时才存一份代表性原文
// （脱敏+限大小）+ 重复次数/时间跨度/涉及 Key。**正常一次性请求只留 simhash（不可逆哈希）、不存原文。**
// 前提是服务条款已声明「疑似滥用可留存请求内容用于取证」。默认无 flag = 不采、热路径零开销。只抓请求。

// EvidenceTargetType 捕获目标类型。
type EvidenceTargetType string

const (
	EvidenceTargetUser EvidenceTargetType = "user"
	EvidenceTargetKey  EvidenceTargetType = "key"

	evidenceMaxTemplatesDefault  = 500
	evidenceStoreThresholdMin    = 2
	evidenceBufferTTLDefault     = 7 * 24 * time.Hour
	evidenceMaxBodyBytesDefault  = 16 * 1024
	evidenceStoreOpTimeout       = 3 * time.Second
	evidenceMaxAPIKeysPerTpl     = 32
	evidenceMaxReqIDsPerTpl      = 20
)

var (
	// ErrEvidenceUnavailable 取证捕获未启用（store 缺失 / master 关）。
	ErrEvidenceUnavailable = errors.New("evidence_capture: unavailable")
	// ErrEvidenceBadRequest 入参非法（target 类型/ID/阈值）。
	ErrEvidenceBadRequest = errors.New("evidence_capture: bad request")
)

// evidencePromptSimhash 对请求的「最新一条消息」算归一化 64 位 simhash（避免多轮对话因共享历史误撞）。
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

// EvidenceFlag 一条捕获名单项（对某 user/key 持续观测重复模板，直到管理员停止 / TTL）。
type EvidenceFlag struct {
	TargetKey      string `json:"target_key"` // "u:<id>" 或 "k:<id>"
	TargetType     string `json:"target_type"`
	TargetID       int64  `json:"target_id"`
	StoreThreshold int    `json:"store_threshold"` // 同模板重复几次才存原文
	MaxTemplates   int    `json:"max_templates"`   // 最多追踪多少个不同模板
	StartedAt      int64  `json:"started_at"`
	AdminID        int64  `json:"admin_id"`
}

// EvidenceTemplate 一个「重复模板」的聚合证据。body 已脱敏 + 限大小；绝不含 secrets。
type EvidenceTemplate struct {
	Simhash    string   `json:"simhash"`
	Count      int      `json:"count"`
	FirstSeen  int64    `json:"first_seen"`
	LastSeen   int64    `json:"last_seen"`
	Model      string   `json:"model"`
	Endpoint   string   `json:"endpoint"`
	APIKeyIDs  []int64  `json:"api_key_ids"`
	RequestIDs []string `json:"request_ids"`
	Body       string   `json:"body"`
	Truncated  bool     `json:"truncated"`
	HasBody    bool     `json:"has_body"`
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
	GetTemplate(ctx context.Context, targetKey, simhash string) (*EvidenceTemplate, error)
	PutTemplate(ctx context.Context, targetKey string, t EvidenceTemplate, ttl time.Duration) error
	TemplateCount(ctx context.Context, targetKey string) (int, error)
	ListTemplates(ctx context.Context, targetKey string) ([]EvidenceTemplate, error)
	PurgeEvidence(ctx context.Context, targetKey string) error
}

// EvidenceCaptureConfigView 运行参数（从 config 提取）。
type EvidenceCaptureConfigView struct {
	Enabled        bool
	MaxTemplates   int
	StoreThreshold int
	BufferTTL      time.Duration
	MaxBodyBytes   int
}

// EvidenceCaptureService 管理捕获名单 + 热路径按模板聚合。
type EvidenceCaptureService struct {
	store EvidenceCaptureStore
	cfg   EvidenceCaptureConfigView

	mu          sync.Mutex
	flags       map[string]EvidenceFlag // target_key → flag
	activeCount int32                   // atomic：>0 才需要检查（零开销闸门）
	tplLocks    sync.Map                // target_key → *sync.Mutex（串行化同 target 的模板读改写）
}

// NewEvidenceCaptureService 构造并从 Redis 载入既有捕获名单（重启可恢复）。store 为 nil → 禁用态服务。
func NewEvidenceCaptureService(store EvidenceCaptureStore, cfg EvidenceCaptureConfigView) *EvidenceCaptureService {
	if cfg.MaxTemplates <= 0 {
		cfg.MaxTemplates = evidenceMaxTemplatesDefault
	}
	if cfg.StoreThreshold < evidenceStoreThresholdMin {
		cfg.StoreThreshold = evidenceStoreThresholdMin
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
				s.flags[f.TargetKey] = f
			}
			atomic.StoreInt32(&s.activeCount, int32(len(s.flags)))
		}
	}
	return s
}

func userTargetKey(id int64) string { return "u:" + strconv.FormatInt(id, 10) }
func keyTargetKey(id int64) string  { return "k:" + strconv.FormatInt(id, 10) }

func (s *EvidenceCaptureService) available() bool {
	return s != nil && s.store != nil && s.cfg.Enabled
}

// Active 是热路径零开销闸门：当前有活跃捕获名单才为 true（一次原子读）。
func (s *EvidenceCaptureService) Active() bool {
	return s != nil && atomic.LoadInt32(&s.activeCount) > 0
}

func (s *EvidenceCaptureService) targetLock(targetKey string) *sync.Mutex {
	m, _ := s.tplLocks.LoadOrStore(targetKey, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// CaptureIfFlagged 热路径：命中捕获名单则异步按模板聚合；默认无 flag 时一次原子读即返回（零开销）。
// 同步只做闸门 + 匹配，simhash/脱敏/Redis 读改写放异步 goroutine（不加请求延迟）。
func (s *EvidenceCaptureService) CaptureIfFlagged(userID, apiKeyID int64, rawBody []byte, meta CaptureMeta) {
	if s == nil || atomic.LoadInt32(&s.activeCount) == 0 {
		return // 零开销短路
	}
	if !s.available() || len(rawBody) == 0 {
		return
	}
	s.mu.Lock()
	tk, flag := "", EvidenceFlag{}
	if f, ok := s.flags[keyTargetKey(apiKeyID)]; ok { // key 级更具体，优先
		tk, flag = f.TargetKey, f
	} else if f, ok := s.flags[userTargetKey(userID)]; ok {
		tk, flag = f.TargetKey, f
	}
	s.mu.Unlock()
	if tk == "" {
		return
	}

	bodyCopy := append([]byte(nil), rawBody...)
	go s.aggregate(tk, flag, apiKeyID, bodyCopy, meta)
}

// aggregate 按模板聚合一条请求（异步）。同一 target 串行，保证计数/首次存 body 正确。
func (s *EvidenceCaptureService) aggregate(tk string, flag EvidenceFlag, apiKeyID int64, body []byte, meta CaptureMeta) {
	sim := strconv.FormatUint(evidencePromptSimhash(body), 16)
	now := time.Now().Unix()

	lock := s.targetLock(tk)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), evidenceStoreOpTimeout)
	defer cancel()

	t, err := s.store.GetTemplate(ctx, tk, sim)
	if err != nil {
		logger.L().With(zap.String("component", "service.evidence_capture")).
			Warn("get template failed", zap.String("target", tk), zap.Error(err))
		return
	}
	if t == nil {
		// 新模板：超上限则不再追新。
		if n, _ := s.store.TemplateCount(ctx, tk); n >= flag.MaxTemplates {
			return
		}
		t = &EvidenceTemplate{Simhash: sim, FirstSeen: now, Model: meta.Model, Endpoint: meta.Endpoint}
	}
	t.Count++
	t.LastSeen = now
	if t.Model == "" {
		t.Model = meta.Model
	}
	if t.Endpoint == "" {
		t.Endpoint = meta.Endpoint
	}
	t.APIKeyIDs = appendBoundedInt64(t.APIKeyIDs, apiKeyID, evidenceMaxAPIKeysPerTpl)
	if meta.RequestID != "" {
		t.RequestIDs = appendBoundedStr(t.RequestIDs, meta.RequestID, evidenceMaxReqIDsPerTpl)
	}
	// 达阈值且尚无 body → 存一份代表性脱敏原文。
	if !t.HasBody && t.Count >= flag.StoreThreshold {
		san, trunc, _ := sanitizeAndTrimJSONPayload(body, s.cfg.MaxBodyBytes)
		t.Body, t.Truncated, t.HasBody = san, trunc, true
	}
	if err := s.store.PutTemplate(ctx, tk, *t, s.cfg.BufferTTL); err != nil {
		logger.L().With(zap.String("component", "service.evidence_capture")).
			Warn("put template failed", zap.String("target", tk), zap.Error(err))
	}
}

func appendBoundedInt64(xs []int64, v int64, max int) []int64 {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	if len(xs) >= max {
		return xs
	}
	return append(xs, v)
}

func appendBoundedStr(xs []string, v string, max int) []string {
	if len(xs) >= max {
		return xs
	}
	return append(xs, v)
}

// StartCapture 开始/重置对某 target 的重复模板取证（管理员操作，审计）。
func (s *EvidenceCaptureService) StartCapture(ctx context.Context, targetType EvidenceTargetType, targetID int64, storeThreshold int, adminID int64) (EvidenceFlag, error) {
	if !s.available() {
		return EvidenceFlag{}, ErrEvidenceUnavailable
	}
	if targetID <= 0 || (targetType != EvidenceTargetUser && targetType != EvidenceTargetKey) {
		return EvidenceFlag{}, ErrEvidenceBadRequest
	}
	if storeThreshold < evidenceStoreThresholdMin {
		storeThreshold = s.cfg.StoreThreshold
	}
	var tk string
	if targetType == EvidenceTargetUser {
		tk = userTargetKey(targetID)
	} else {
		tk = keyTargetKey(targetID)
	}
	f := EvidenceFlag{
		TargetKey: tk, TargetType: string(targetType), TargetID: targetID,
		StoreThreshold: storeThreshold, MaxTemplates: s.cfg.MaxTemplates,
		StartedAt: time.Now().Unix(), AdminID: adminID,
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
		Info("capture started", zap.String("target", tk), zap.Int("store_threshold", storeThreshold), zap.Int64("admin_id", adminID))
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

// ListEvidenceTemplates 取某 target 已聚合的重复模板（store 侧按 count 降序）。
func (s *EvidenceCaptureService) ListEvidenceTemplates(ctx context.Context, targetKey string) ([]EvidenceTemplate, error) {
	if !s.available() {
		return nil, ErrEvidenceUnavailable
	}
	return s.store.ListTemplates(ctx, targetKey)
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
	s.tplLocks.Delete(targetKey)
	logger.L().With(zap.String("component", "service.evidence_capture")).
		Info("capture purged", zap.String("target", targetKey), zap.Int64("admin_id", adminID))
	return nil
}
