package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// cellResolver 给出转发候选 cell(按优先级排序,best-first)。EdgeForward 依次
// 尝试:传输失败(写响应前)则顺位转移到下一个。空列表 = 无可达 cell → 502。
type cellResolver interface {
	candidates() []*url.URL
}

// staticResolver:固定单 cell(P3-2 之前的行为;RegistryURL 未配时使用)。
type staticResolver struct{ target *url.URL }

func (s *staticResolver) candidates() []*url.URL {
	if s.target == nil {
		return nil
	}
	return []*url.URL{s.target}
}

// dynamicResolver:周期性从 Portal routable 目录拉取存活 cell(已按健康分降序),
// 缓存在内存;可选 static 作为最后兜底候选。读写用 RWMutex 保护。
// 单中央进程内缓存足够;多中央实例共享路由表(Redis)留到需要时再做。
type dynamicResolver struct {
	mu     sync.RWMutex
	cached []*url.URL
	static *url.URL // 可选:registry 为空/不可达时的兜底
}

func (d *dynamicResolver) candidates() []*url.URL {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*url.URL, 0, len(d.cached)+1)
	seen := make(map[string]struct{}, len(d.cached)+1)
	for _, u := range d.cached {
		key := u.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, u)
	}
	// static 作为最后候选(去重):动态池全挂时仍有一条兜底路径。
	if d.static != nil {
		if _, ok := seen[d.static.String()]; !ok {
			out = append(out, d.static)
		}
	}
	return out
}

func (d *dynamicResolver) set(cells []*url.URL) {
	d.mu.Lock()
	d.cached = cells
	d.mu.Unlock()
}

// routableResponse 对应 Portal GET /internal/cells/routable 的响应(只取 baseUrl,
// 顺序即优先级)。
type routableResponse struct {
	Cells []struct {
		BaseURL string `json:"baseUrl"`
	} `json:"cells"`
}

// startRegistryRefresh 后台周期性拉取 routableURL 并刷新 d.cached。首次拉取也在
// goroutine 内(不阻塞构造/路由注册);首个请求若赶在首次刷新前,candidates() 会
// 退回 static 兜底。拉取失败只 warn 并保留上一次缓存(不清空 → 抖动容忍)。
func startRegistryRefresh(ctx context.Context, d *dynamicResolver, routableURL, token string, interval time.Duration) {
	client := &http.Client{Timeout: 8 * time.Second}
	fetch := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, routableURL, nil)
		if err != nil {
			return
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("edge_forward: 拉取 routable cells 失败,保留上次缓存", "err", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			slog.Warn("edge_forward: routable cells 非 200,保留上次缓存", "status", resp.StatusCode)
			return
		}
		var parsed routableResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			slog.Warn("edge_forward: 解析 routable cells 失败", "err", err)
			return
		}
		cells := make([]*url.URL, 0, len(parsed.Cells))
		for _, c := range parsed.Cells {
			u, perr := url.Parse(strings.TrimSpace(c.BaseURL))
			if perr != nil || u.Scheme == "" || u.Host == "" {
				continue
			}
			cells = append(cells, u)
		}
		d.set(cells)
		slog.Debug("edge_forward: routable cells 刷新", "count", len(cells))
	}
	go func() {
		fetch() // 首次拉取(异步,不阻塞启动)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fetch()
			}
		}
	}()
}
