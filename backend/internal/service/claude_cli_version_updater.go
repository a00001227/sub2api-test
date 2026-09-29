package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// ClaudeCLIVersionCache 伪装 CLI 版本的持久缓存(Redis),跨进程重启沿用上次拉到的值。
type ClaudeCLIVersionCache interface {
	Get(ctx context.Context) (string, error)
	Set(ctx context.Context, version string, ttl time.Duration) error
}

const (
	// claudeCLIVersionCacheTTL 缓存保留期:远大于刷新间隔,npm 长时间拉不到也不回退到常量。
	claudeCLIVersionCacheTTL     = 7 * 24 * time.Hour
	claudeCLIVersionFetchTimeout = 15 * time.Second
	claudeCLIVersionMaxBody      = 1 << 20
)

// ClaudeCLIVersionUpdater 让伪装的 Claude Code CLI 版本自动跟随 npm 官方最新发布版。
//
// 背景:版本原来是代码常量,Claude Code 每次发版、Anthropic 抬高某模型的最低版本要求,
// 都要改代码 + 重建 cell,否则上游回 "Claude Code x.y.z does not support this model"。
// 现在定期 GET npm registry 的 latest 清单,取 version 写进 claude.SetCurrentCLIVersion
// (只接受三段正式版且不低于常量),并缓存到 Redis;指纹/billing/默认头全部改走运行时版本。
// 刻意不采信流量里见过的版本:线上已出现伪造的 "claude-cli/2.5.0",跟着它抬会把所有号顶到
// 一个不存在的版本。
type ClaudeCLIVersionUpdater struct {
	cfg    config.ClaudeCLIVersionConfig
	cache  ClaudeCLIVersionCache
	client *http.Client

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewClaudeCLIVersionUpdater 构造(不启动)。
func NewClaudeCLIVersionUpdater(cfg *config.Config, cache ClaudeCLIVersionCache) *ClaudeCLIVersionUpdater {
	u := &ClaudeCLIVersionUpdater{
		cache:  cache,
		client: &http.Client{Timeout: claudeCLIVersionFetchTimeout},
		stopCh: make(chan struct{}),
	}
	if cfg != nil {
		u.cfg = cfg.Gateway.ClaudeCLIVersion
	}
	return u
}

// ProvideClaudeCLIVersionUpdater 构造并启动(wire 用)。
func ProvideClaudeCLIVersionUpdater(cfg *config.Config, cache ClaudeCLIVersionCache) *ClaudeCLIVersionUpdater {
	u := NewClaudeCLIVersionUpdater(cfg, cache)
	u.Start()
	return u
}

// Start 先用 Redis 缓存的版本兜住,再后台按 refresh_interval 轮询 npm。auto_update 关闭时不做任何事。
func (u *ClaudeCLIVersionUpdater) Start() {
	if u == nil {
		return
	}
	u.mu.Lock()
	if u.started {
		u.mu.Unlock()
		return
	}
	u.started = true
	u.mu.Unlock()

	log := logger.L().With(zap.String("component", "service.claude_cli_version"))
	if !u.cfg.AutoUpdate {
		log.Info("claude_cli_version.auto_update_disabled", zap.String("version", claude.CurrentCLIVersion()))
		return
	}
	u.applyCached(log)

	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.refresh(log)
		interval := u.cfg.RefreshInterval
		if interval < time.Minute {
			interval = time.Hour
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-u.stopCh:
				return
			case <-t.C:
				u.refresh(log)
			}
		}
	}()
}

// Stop 停止后台轮询。
func (u *ClaudeCLIVersionUpdater) Stop() {
	if u == nil {
		return
	}
	u.mu.Lock()
	if !u.started {
		u.mu.Unlock()
		return
	}
	select {
	case <-u.stopCh:
	default:
		close(u.stopCh)
	}
	u.mu.Unlock()
	u.wg.Wait()
}

func (u *ClaudeCLIVersionUpdater) applyCached(log *zap.Logger) {
	if u.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, err := u.cache.Get(ctx)
	if err != nil {
		log.Warn("claude_cli_version.cache_read_failed", zap.Error(err))
		return
	}
	if v == "" {
		return
	}
	if claude.SetCurrentCLIVersion(v) {
		log.Info("claude_cli_version.applied_from_cache", zap.String("version", v))
	}
}

func (u *ClaudeCLIVersionUpdater) refresh(log *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeCLIVersionFetchTimeout+5*time.Second)
	defer cancel()
	v, err := fetchLatestClaudeCLIVersion(ctx, u.client, u.cfg.SourceURL)
	if err != nil {
		log.Warn("claude_cli_version.fetch_failed", zap.String("url", u.cfg.SourceURL), zap.Error(err))
		return
	}
	before := claude.CurrentCLIVersion()
	if !claude.SetCurrentCLIVersion(v) {
		// 低于常量或格式不对:常量是下限,不往回拨。
		log.Info("claude_cli_version.fetched_not_applied", zap.String("fetched", v), zap.String("current", before))
		return
	}
	if v != before {
		log.Info("claude_cli_version.updated", zap.String("from", before), zap.String("to", v))
	}
	if u.cache != nil {
		if err := u.cache.Set(ctx, v, claudeCLIVersionCacheTTL); err != nil {
			log.Warn("claude_cli_version.cache_write_failed", zap.Error(err))
		}
	}
}

// fetchLatestClaudeCLIVersion GET npm registry 的 latest 清单,取 "version"(必须是三段正式版,
// 预发布/带 tag 的一律拒绝)。
func fetchLatestClaudeCLIVersion(ctx context.Context, client *http.Client, url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", errors.New("source url is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-cli-version-check")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, claudeCLIVersionMaxBody)).Decode(&manifest); err != nil {
		return "", fmt.Errorf("decode manifest: %w", err)
	}
	v := strings.TrimSpace(manifest.Version)
	if !isReleaseSemver(v) {
		return "", fmt.Errorf("invalid version %q", manifest.Version)
	}
	return v, nil
}

// isReleaseSemver 三段纯数字版本(拒绝 2.1.300-beta.1 之类的预发布)。
func isReleaseSemver(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
