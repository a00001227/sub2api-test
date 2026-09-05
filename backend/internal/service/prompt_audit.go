package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

const (
	defaultPromptAuditWorkerCount = 4
	maxPromptAuditQueueSize       = 32768
	// 单条留存的提示词文本上限（rune）；防止单个超大请求撑爆一行。远大于内容审核 240 摘要。
	maxPromptAuditFullPromptRunes = 200000

	defaultPromptAuditRetentionDays = 30
	maxPromptAuditRetentionDays     = 3650

	promptAuditConfigCacheTTL = 10 * time.Second
	promptAuditCleanupInterval = 24 * time.Hour
	promptAuditCleanupTimeout  = 30 * time.Minute
	promptAuditPersistTimeout  = 10 * time.Second
)

// PromptAuditConfig 提示词审计配置（存 settings，JSON）。
type PromptAuditConfig struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days"`
}

func defaultPromptAuditConfig() PromptAuditConfig {
	return PromptAuditConfig{Enabled: false, RetentionDays: defaultPromptAuditRetentionDays}
}

func (c *PromptAuditConfig) normalize() {
	if c.RetentionDays <= 0 {
		c.RetentionDays = defaultPromptAuditRetentionDays
	}
	if c.RetentionDays > maxPromptAuditRetentionDays {
		c.RetentionDays = maxPromptAuditRetentionDays
	}
}

// PromptAuditEvent 一条提示词审计留存记录。
type PromptAuditEvent struct {
	ID           int64     `json:"id"`
	RequestID    string    `json:"request_id"`
	UserID       *int64    `json:"user_id"`
	UserEmail    string    `json:"user_email"`
	APIKeyID     *int64    `json:"api_key_id"`
	APIKeyName   string    `json:"api_key_name"`
	GroupID      *int64    `json:"group_id"`
	GroupName    string    `json:"group_name"`
	Provider     string    `json:"provider"`
	Endpoint     string    `json:"endpoint"`
	Protocol     string    `json:"protocol"`
	Model        string    `json:"model"`
	PromptHash   string    `json:"prompt_hash"`
	PromptLength int       `json:"prompt_length"`
	MessageCount int       `json:"message_count"`
	FullPrompt   string    `json:"full_prompt"`
	UserStatus   string    `json:"user_status"`
	CreatedAt    time.Time `json:"created_at"`
}

// PromptAuditEventFilter 列表查询过滤条件。
type PromptAuditEventFilter struct {
	Search     string
	GroupID    *int64
	APIKeyID   *int64
	UserID     *int64
	From       *time.Time
	To         *time.Time
	Pagination pagination.PaginationParams
}

// PromptAuditStatus 面板状态展示。
type PromptAuditStatus struct {
	Enabled       bool  `json:"enabled"`
	RetentionDays int   `json:"retention_days"`
	QueueLength   int   `json:"queue_length"`
	QueueCapacity int   `json:"queue_capacity"`
	Stored        int64 `json:"stored"`
	Dropped       int64 `json:"dropped"`
}

// PromptAuditRepository 提示词审计仓储。
type PromptAuditRepository interface {
	CreateEvent(ctx context.Context, event *PromptAuditEvent) error
	ListEvents(ctx context.Context, filter PromptAuditEventFilter) ([]PromptAuditEvent, *pagination.PaginationResult, error)
	GetEvent(ctx context.Context, id int64) (*PromptAuditEvent, error)
	DeleteEvent(ctx context.Context, id int64) error
	DeleteAll(ctx context.Context) (int64, error)
	CleanupExpired(ctx context.Context, before time.Time) (int64, error)
}

// promptAuditTask 异步落库任务（只带原始 body + 元数据，解析在 worker 里做，避免占用请求热路径）。
type promptAuditTask struct {
	requestID  string
	userID     *int64
	userEmail  string
	apiKeyID   *int64
	apiKeyName string
	groupID    *int64
	groupName  string
	provider   string
	endpoint   string
	protocol   string
	model      string
	body       []byte
}

type promptAuditCachedConfig struct {
	cfg PromptAuditConfig
	at  time.Time
}

// PromptAuditService 提示词审计服务：与内容审核解耦，独立开关/队列/保留期。
type PromptAuditService struct {
	settingRepo SettingRepository
	repo        PromptAuditRepository

	queue chan promptAuditTask

	cfgCache atomic.Pointer[promptAuditCachedConfig]

	stored  atomic.Int64
	dropped atomic.Int64
}

// NewPromptAuditService 构造并在后台拉起 worker 与清理循环（与内容审核同构）。
func NewPromptAuditService(settingRepo SettingRepository, repo PromptAuditRepository) *PromptAuditService {
	s := &PromptAuditService{
		settingRepo: settingRepo,
		repo:        repo,
		queue:       make(chan promptAuditTask, maxPromptAuditQueueSize),
	}
	if settingRepo != nil && repo != nil {
		for i := 0; i < defaultPromptAuditWorkerCount; i++ {
			go s.worker(i)
		}
		go s.cleanupLoop()
	}
	return s
}

// Active 是否启用（供中间件热路径快速判定；带 10s 内存缓存，避免每请求查库）。
func (s *PromptAuditService) Active(ctx context.Context) bool {
	if s == nil || s.settingRepo == nil || s.repo == nil {
		return false
	}
	return s.loadConfig(ctx).Enabled
}

func (s *PromptAuditService) loadConfig(ctx context.Context) PromptAuditConfig {
	if cached := s.cfgCache.Load(); cached != nil && time.Since(cached.at) < promptAuditConfigCacheTTL {
		return cached.cfg
	}
	cfg := defaultPromptAuditConfig()
	if s.settingRepo != nil {
		raw, err := s.settingRepo.GetValue(ctx, SettingKeyPromptAuditConfig)
		if err == nil && strings.TrimSpace(raw) != "" {
			var parsed PromptAuditConfig
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				cfg = parsed
			}
		}
	}
	cfg.normalize()
	s.cfgCache.Store(&promptAuditCachedConfig{cfg: cfg, at: time.Now()})
	return cfg
}

func (s *PromptAuditService) invalidateConfigCache() {
	s.cfgCache.Store(nil)
}

// Capture 请求热路径调用：仅拷贝 body + 元数据后非阻塞入队，队列满即丢，绝不阻塞请求。
func (s *PromptAuditService) Capture(ctx context.Context, task promptAuditTask) {
	if s == nil || s.repo == nil {
		return
	}
	if !s.Active(ctx) {
		return
	}
	select {
	case s.queue <- task:
	default:
		s.dropped.Add(1)
		slog.Warn("prompt_audit.queue_full", "queue_len", len(s.queue))
	}
}

// BuildTask 供中间件构造任务（body 由调用方保证已拷贝，worker 只读）。
func (s *PromptAuditService) BuildTask(requestID string, userID *int64, userEmail string, apiKeyID *int64, apiKeyName string, groupID *int64, groupName, provider, endpoint, protocol, model string, body []byte) promptAuditTask {
	return promptAuditTask{
		requestID:  requestID,
		userID:     userID,
		userEmail:  userEmail,
		apiKeyID:   apiKeyID,
		apiKeyName: apiKeyName,
		groupID:    groupID,
		groupName:  groupName,
		provider:   provider,
		endpoint:   endpoint,
		protocol:   protocol,
		model:      model,
		body:       body,
	}
}

func (s *PromptAuditService) worker(id int) {
	for task := range s.queue {
		s.process(task)
	}
}

func (s *PromptAuditService) process(task promptAuditTask) {
	text, msgCount := ExtractPromptAuditContent(task.protocol, task.body)
	text = strings.TrimSpace(text)
	if text == "" {
		return // 无可留存的提示词文本（如纯图片/空请求）
	}
	text = redactContentModerationSecrets(text)
	text = trimRunes(text, maxPromptAuditFullPromptRunes)
	sum := sha256.Sum256([]byte(text))
	event := &PromptAuditEvent{
		RequestID:    task.requestID,
		UserID:       task.userID,
		UserEmail:    task.userEmail,
		APIKeyID:     task.apiKeyID,
		APIKeyName:   task.apiKeyName,
		GroupID:      task.groupID,
		GroupName:    task.groupName,
		Provider:     task.provider,
		Endpoint:     task.endpoint,
		Protocol:     task.protocol,
		Model:        task.model,
		PromptHash:   hex.EncodeToString(sum[:]),
		PromptLength: len([]rune(text)),
		MessageCount: msgCount,
		FullPrompt:   text,
	}
	ctx, cancel := context.WithTimeout(context.Background(), promptAuditPersistTimeout)
	defer cancel()
	if err := s.repo.CreateEvent(ctx, event); err != nil {
		slog.Warn("prompt_audit.create_event_failed", "request_id", task.requestID, "error", err)
		return
	}
	s.stored.Add(1)
}

func (s *PromptAuditService) cleanupLoop() {
	ticker := time.NewTicker(promptAuditCleanupInterval)
	defer ticker.Stop()
	// 启动后延迟片刻先跑一次，避免与其它启动任务抢资源。
	time.Sleep(5 * time.Minute)
	s.runCleanup()
	for range ticker.C {
		s.runCleanup()
	}
}

func (s *PromptAuditService) runCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), promptAuditCleanupTimeout)
	defer cancel()
	cfg := s.loadConfig(ctx)
	before := time.Now().AddDate(0, 0, -cfg.RetentionDays)
	deleted, err := s.repo.CleanupExpired(ctx, before)
	if err != nil {
		slog.Warn("prompt_audit.cleanup_failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("prompt_audit.cleanup_done", "deleted", deleted, "retention_days", cfg.RetentionDays)
	}
}

// ===== Admin API =====

func (s *PromptAuditService) GetConfig(ctx context.Context) (PromptAuditConfig, error) {
	if s == nil || s.settingRepo == nil {
		return defaultPromptAuditConfig(), nil
	}
	return s.loadConfig(ctx), nil
}

func (s *PromptAuditService) UpdateConfig(ctx context.Context, cfg PromptAuditConfig) (PromptAuditConfig, error) {
	if s == nil || s.settingRepo == nil {
		return defaultPromptAuditConfig(), infraerrors.BadRequest("PROMPT_AUDIT_UNAVAILABLE", "提示词审计服务不可用")
	}
	cfg.normalize()
	raw, err := json.Marshal(cfg)
	if err != nil {
		return cfg, infraerrors.BadRequest("INVALID_PROMPT_AUDIT_CONFIG", "提示词审计配置无效")
	}
	if err := s.settingRepo.Set(ctx, SettingKeyPromptAuditConfig, string(raw)); err != nil {
		return cfg, err
	}
	s.invalidateConfigCache()
	return cfg, nil
}

func (s *PromptAuditService) ListEvents(ctx context.Context, filter PromptAuditEventFilter) ([]PromptAuditEvent, *pagination.PaginationResult, error) {
	if s == nil || s.repo == nil {
		return nil, nil, infraerrors.BadRequest("PROMPT_AUDIT_UNAVAILABLE", "提示词审计服务不可用")
	}
	return s.repo.ListEvents(ctx, filter)
}

func (s *PromptAuditService) GetEvent(ctx context.Context, id int64) (*PromptAuditEvent, error) {
	if s == nil || s.repo == nil {
		return nil, infraerrors.BadRequest("PROMPT_AUDIT_UNAVAILABLE", "提示词审计服务不可用")
	}
	return s.repo.GetEvent(ctx, id)
}

func (s *PromptAuditService) DeleteEvent(ctx context.Context, id int64) error {
	if s == nil || s.repo == nil {
		return infraerrors.BadRequest("PROMPT_AUDIT_UNAVAILABLE", "提示词审计服务不可用")
	}
	return s.repo.DeleteEvent(ctx, id)
}

func (s *PromptAuditService) DeleteAll(ctx context.Context) (int64, error) {
	if s == nil || s.repo == nil {
		return 0, infraerrors.BadRequest("PROMPT_AUDIT_UNAVAILABLE", "提示词审计服务不可用")
	}
	return s.repo.DeleteAll(ctx)
}

func (s *PromptAuditService) Status(ctx context.Context) PromptAuditStatus {
	cfg := s.loadConfig(ctx)
	return PromptAuditStatus{
		Enabled:       cfg.Enabled,
		RetentionDays: cfg.RetentionDays,
		QueueLength:   len(s.queue),
		QueueCapacity: cap(s.queue),
		Stored:        s.stored.Load(),
		Dropped:       s.dropped.Load(),
	}
}
