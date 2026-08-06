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

// cellCandidate 是一个可路由 cell:地址 + 信誉分(DeRouter 口径,来自链上结算历史,
// 中央选 cell 的 40% 权重)。
type cellCandidate struct {
	url        *url.URL
	reputation int
}

// cellResolver 提供转发候选。
//   - poolCandidates:参与加权随机的存活 cell 池(带信誉分)。
//   - fallback:最后兜底 cell(静态配置);动态池全空/全挂时才用,不参与加权。
type cellResolver interface {
	poolCandidates() []cellCandidate
	fallback() *url.URL
}

// staticResolver:未配 RegistryURL 时,静态单 cell 就是整个池(不是兜底)。
type staticResolver struct{ target *url.URL }

func (s *staticResolver) poolCandidates() []cellCandidate {
	if s.target == nil {
		return nil
	}
	return []cellCandidate{{url: s.target, reputation: 50}} // 无信誉信息 → base 50
}
func (s *staticResolver) fallback() *url.URL { return nil }

// dynamicResolver:周期性从 Portal routable 拉存活 cell(带信誉分)缓存;static 作为
// 独立兜底(不混入加权池)。
type dynamicResolver struct {
	mu     sync.RWMutex
	cached []cellCandidate
	static *url.URL
}

func (d *dynamicResolver) poolCandidates() []cellCandidate {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]cellCandidate, len(d.cached))
	copy(out, d.cached)
	return out
}
func (d *dynamicResolver) fallback() *url.URL { return d.static }

func (d *dynamicResolver) set(cells []cellCandidate) {
	d.mu.Lock()
	d.cached = cells
	d.mu.Unlock()
}

// reputationWeight 把信誉分映射成加权随机的权重(DeRouter 口径):base 50 = ×1.00,
// 每 +10 信誉 ≈ +4% 概率(base→顶 ≈ ×1.17);加权随机而非赢家通吃,新号(base 50)
// 仍有实打实份额。低于 base 递减但保留下限,不彻底排除。
func reputationWeight(rep int) float64 {
	w := 1.0 + 0.004*float64(rep-50)
	if w < 0.1 {
		w = 0.1
	}
	return w
}

// weightedOrder 按信誉权重做「加权随机、不放回」抽样,返回失败转移用的候选顺序:
// 第一个 = 加权随机首选,其余顺位作为转移候选。rng() ∈ [0,1)。
func weightedOrder(pool []cellCandidate, rng func() float64) []*url.URL {
	remaining := make([]cellCandidate, len(pool))
	copy(remaining, pool)
	out := make([]*url.URL, 0, len(remaining))
	for len(remaining) > 0 {
		total := 0.0
		for _, c := range remaining {
			total += reputationWeight(c.reputation)
		}
		r := rng() * total
		idx := len(remaining) - 1
		for i, c := range remaining {
			r -= reputationWeight(c.reputation)
			if r <= 0 {
				idx = i
				break
			}
		}
		out = append(out, remaining[idx].url)
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	return out
}

// moveToFront 若 target 在 order 中则把它移到队首(会话亲和:优先落原 cell),否则
// 原样返回(陈旧绑定:该 cell 已不在存活池中 → 忽略,走加权随机)。按 URL 字符串比较。
func moveToFront(order []*url.URL, target *url.URL) []*url.URL {
	if target == nil {
		return order
	}
	idx := -1
	for i, u := range order {
		if u.String() == target.String() {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return order // 不在列表(-1)或已在队首(0)
	}
	out := make([]*url.URL, 0, len(order))
	out = append(out, order[idx])
	out = append(out, order[:idx]...)
	out = append(out, order[idx+1:]...)
	return out
}

// routableResponse 对应 Portal GET /internal/cells/routable(取 baseUrl + reputation)。
type routableResponse struct {
	Cells []struct {
		BaseURL    string `json:"baseUrl"`
		Reputation int    `json:"reputation"`
	} `json:"cells"`
}

// startRegistryRefresh 后台周期性拉取 routableURL 并刷新缓存。首次拉取也在 goroutine
// 内(不阻塞启动);拉取失败只 warn 并保留上一次缓存。
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
		cells := make([]cellCandidate, 0, len(parsed.Cells))
		for _, c := range parsed.Cells {
			u, perr := url.Parse(strings.TrimSpace(c.BaseURL))
			if perr != nil || u.Scheme == "" || u.Host == "" {
				continue
			}
			cells = append(cells, cellCandidate{url: u, reputation: c.Reputation})
		}
		d.set(cells)
		slog.Debug("edge_forward: routable cells 刷新", "count", len(cells))
	}
	go func() {
		fetch()
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
