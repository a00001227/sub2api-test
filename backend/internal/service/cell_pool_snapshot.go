package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 账号池快照(cell → Portal 推送):Portal 的「账号池实况」看板只读 Portal 存的最新一份,
// 不再逐号回拉 cell。每 PoolSnapshotIntervalSeconds 推一次本 cell 全部 Portal 归属账号的
// 实时状态;Portal 按 baseUrl 认 cell(与心跳同一口径),存最新一份带接收时间。

// AccountErrorActivity 是按账号聚合的近期错误活动。
type AccountErrorActivity struct {
	Count          int64  `json:"count"`
	LastAt         string `json:"last_at"`
	LastStatusCode int    `json:"last_status_code"`
	LastMessage    string `json:"last_message"`
}

// PoolAccountSnapshot 是单个账号在快照里的形态(脱敏:无凭据、无代理密码)。
type PoolAccountSnapshot struct {
	ExternalRef  string `json:"external_ref"`
	Name         string `json:"name"`
	Platform     string `json:"platform"`
	Type         string `json:"type"`
	Status       string `json:"status"`                  // active | disabled | error
	Schedulable  bool   `json:"schedulable"`             // false = 已暂停(Portal 恢复)
	RejectReason string `json:"reject_reason,omitempty"` // SchedulableRejectReason:rate_limited / util_dormant_5h / temp_unschedulable …
	RunState     string `json:"run_state"`               // paused | error | rate_limited | temp_unschedulable | overloaded | dormant | resting | busy | idle
	Priority     int    `json:"priority"`
	ProxyID      *int64 `json:"proxy_id,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	Metrics *ProviderAccountMetrics `json:"metrics"`
	Errors  *AccountErrorActivity   `json:"errors_1h,omitempty"`
}

// CellPoolSnapshot 是一次推送的载荷。
type CellPoolSnapshot struct {
	BaseURL   string                `json:"baseUrl"`
	Region    string                `json:"region"`
	Node      string                `json:"node,omitempty"`
	At        string                `json:"at"`
	Accounts  []PoolAccountSnapshot `json:"accounts"`
	BuildTime int64                 `json:"build_ms"`
}

// ProviderLinkedAccount 是 Portal 归属账号及其 externalRef(Account 结构本身不带该字段)。
type ProviderLinkedAccount struct {
	ExternalRef string
	Account     Account
}

type poolSnapshotAccountLister interface {
	ListProviderLinked(ctx context.Context) ([]ProviderLinkedAccount, error)
}

type poolSnapshotErrorCounter interface {
	CountErrorsByAccountSince(ctx context.Context, since time.Time) (map[int64]AccountErrorActivity, error)
}

// CellPoolSnapshotService 组装并推送本 cell 的账号池快照。
type CellPoolSnapshotService struct {
	cfg     *config.Config
	lister  poolSnapshotAccountLister
	errors  poolSnapshotErrorCounter
	metrics *ProviderAccountMetricsService
	client  *http.Client
	now     func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
}

// NewCellPoolSnapshotService 组装服务;accountRepo/opsRepo 通过可选接口取能力(不改主接口)。
func NewCellPoolSnapshotService(cfg *config.Config, accountRepo AccountRepository, opsRepo OpsRepository, metrics *ProviderAccountMetricsService) *CellPoolSnapshotService {
	svc := &CellPoolSnapshotService{cfg: cfg, metrics: metrics, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
	if l, ok := accountRepo.(poolSnapshotAccountLister); ok {
		svc.lister = l
	}
	if e, ok := opsRepo.(poolSnapshotErrorCounter); ok {
		svc.errors = e
	}
	return svc
}

// Build 组装一份快照(不推送)。
func (s *CellPoolSnapshotService) Build(ctx context.Context) (*CellPoolSnapshot, error) {
	start := s.now()
	accounts, err := s.lister.ListProviderLinked(ctx)
	if err != nil {
		return nil, err
	}
	var errMap map[int64]AccountErrorActivity
	if s.errors != nil {
		if m, eerr := s.errors.CountErrorsByAccountSince(ctx, start.Add(-time.Hour)); eerr == nil {
			errMap = m
		} else {
			slog.Warn("pool_snapshot: 按账号聚合错误失败", "err", eerr)
		}
	}
	rc := s.cfg.CellRegistry
	out := &CellPoolSnapshot{
		BaseURL:  strings.TrimSpace(rc.AdvertiseAddr),
		Region:   strings.ToLower(strings.TrimSpace(rc.Region)),
		Node:     strings.TrimSpace(rc.Node),
		At:       start.UTC().Format(time.RFC3339),
		Accounts: make([]PoolAccountSnapshot, 0, len(accounts)),
	}
	for i := range accounts {
		acc := &accounts[i].Account
		ref := strings.TrimSpace(accounts[i].ExternalRef)
		if ref == "" {
			continue
		}
		// Portal 软删(removed)→ cell deactivate 置 disabled:这是 cell 侧的软删除态,不再上报,
		// 否则 Portal 已删的号会在「平台监控」里当孤儿反复出现。硬删路径漏通知的历史孤儿由
		// Portal 侧 orphan 清理调 deactivate 收敛到同一状态。
		if poolSnapshotSkipAccount(acc) {
			continue
		}
		m := s.metrics.MetricsForAccount(ctx, acc)
		item := PoolAccountSnapshot{
			ExternalRef:  ref,
			Name:         acc.Name,
			Platform:     acc.Platform,
			Type:         acc.Type,
			Status:       acc.Status,
			Schedulable:  acc.Schedulable,
			RejectReason: acc.SchedulableRejectReason(),
			Priority:     acc.Priority,
			ProxyID:      acc.ProxyID,
			ErrorMessage: truncateProviderErrorMessage(acc.ErrorMessage),
			Metrics:      m,
		}
		if ea, ok := errMap[acc.ID]; ok {
			e := ea
			item.Errors = &e
		}
		item.RunState = derivePoolRunState(acc, m, start)
		out.Accounts = append(out.Accounts, item)
	}
	out.BuildTime = s.now().Sub(start).Milliseconds()
	return out, nil
}

// derivePoolRunState 把账号的多个状态字段归一成看板用的单一运行态(优先级从高到低)。
func derivePoolRunState(acc *Account, m *ProviderAccountMetrics, now time.Time) string {
	switch {
	case acc.Status == StatusError:
		return "error"
	case acc.Status != StatusActive:
		return "disabled"
	case !acc.Schedulable:
		return "paused"
	}
	if _, source, ok := activeUnschedulableWindow(acc, now); ok {
		switch source {
		case ProviderRateLimitSourceTempUnschedulable:
			return "temp_unschedulable"
		case ProviderRateLimitSourceOverload:
			return "overloaded"
		default:
			return "rate_limited"
		}
	}
	if acc.IsUtilizationDormant() {
		return "dormant"
	}
	if acc.GetPacingMode() != "" && acc.IsHumanizedDormant(now) {
		return "resting"
	}
	if m != nil && m.ConcurrencyUsed > 0 {
		return "busy"
	}
	return "idle"
}

// snapshotURL 由注册地址派生推送地址:…/internal/cells/register → …/internal/cells/pool-snapshot。
func (s *CellPoolSnapshotService) snapshotURL() string {
	u := strings.TrimSpace(s.cfg.CellRegistry.URL)
	if u == "" {
		return ""
	}
	if strings.HasSuffix(u, "/register") {
		return strings.TrimSuffix(u, "/register") + "/pool-snapshot"
	}
	return strings.TrimRight(u, "/") + "/pool-snapshot"
}

// Push 组装并推送一次。
func (s *CellPoolSnapshotService) Push(ctx context.Context) error {
	snap, err := s.Build(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.snapshotURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(s.cfg.ProviderConnect.InternalToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		slog.Warn("pool_snapshot: Portal 非 2xx", "status", resp.StatusCode, "accounts", len(snap.Accounts))
	}
	return nil
}

// Start 在边缘模式且配置齐全时启动定时推送;重复调用无效。
func (s *CellPoolSnapshotService) Start(ctx context.Context) {
	if s == nil || s.cfg == nil || !s.cfg.EdgeMode || s.lister == nil || s.metrics == nil {
		return
	}
	rc := s.cfg.CellRegistry
	interval := time.Duration(rc.PoolSnapshotIntervalSeconds) * time.Second
	if interval <= 0 || s.snapshotURL() == "" || strings.TrimSpace(rc.AdvertiseAddr) == "" {
		return
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, interval)
				if err := s.Push(pctx); err != nil {
					slog.Warn("pool_snapshot: 推送失败", "err", err)
				}
				cancel()
			}
		}
	}()
	slog.Info("pool_snapshot: 账号池快照推送已启动", "url", s.snapshotURL(), "interval", interval.String())
}

// Stop 停止推送(nil-safe)。
func (s *CellPoolSnapshotService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.mu.Unlock()
}

// ProvideCellPoolSnapshotService 供 wire:构造并启动。
func ProvideCellPoolSnapshotService(cfg *config.Config, accountRepo AccountRepository, opsRepo OpsRepository, metrics *ProviderAccountMetricsService) *CellPoolSnapshotService {
	svc := NewCellPoolSnapshotService(cfg, accountRepo, opsRepo, metrics)
	svc.Start(context.Background())
	return svc
}

// poolSnapshotSkipAccount 报告某账号是否不进快照:disabled = Portal 软删后 cell 侧的终态。
func poolSnapshotSkipAccount(acc *Account) bool {
	return acc != nil && acc.Status == StatusDisabled
}
