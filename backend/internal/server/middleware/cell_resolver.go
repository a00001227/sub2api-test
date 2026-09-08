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

// 工作道(risk tier / 号池),对齐 Portal cells/lane.ts。一 cell 一道:蒸馏=可牺牲池
// (护号,绝不碰好号),batch=批量池,normal=好号池。未知/空 → normal(fail-safe)。
const (
	laneNormal       = "normal"
	laneBatch        = "batch"
	laneDistillation = "distillation"
)

// normalizeLane 归一工作道;是全链路的单一真源(config 只做浅解析,规范化都走这里)。
func normalizeLane(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case laneBatch:
		return laneBatch
	case laneDistillation:
		return laneDistillation
	default:
		return laneNormal
	}
}

// cellCandidate 是一个可路由 cell:地址 + 信誉分(DeRouter 口径,来自链上结算历史,
// 中央选 cell 的 40% 权重)+ 工作道(消费者按道过滤,一 cell 一道)+ 可服务平台集合
// (Portal 已把 providerType 归一成中央平台口径:claude→anthropic、codex/openai→openai、
// gemini→gemini)。platforms 为空 = 未上报(旧 Portal / 未知号)→ 平台过滤视为「全平台可服务」。
type cellCandidate struct {
	url        *url.URL
	reputation int
	lane       string   // normal|batch|distillation;"" 视为 normal
	platforms  []string // 该 cell 有可服务号的平台(已归一小写去重);空 = 未知 → 不参与平台过滤
}

// filterByLane 只保留工作道匹配的 cell。严格隔离:非 normal 消费者绝不跨道 —— 过滤在
// 加权抽样之前,使信誉权重只在同道内比较(高信誉好号 cell 不会盖过蒸馏 cell)。
func filterByLane(pool []cellCandidate, want string) []cellCandidate {
	out := make([]cellCandidate, 0, len(pool))
	for _, c := range pool {
		if normalizeLane(c.lane) == want {
			out = append(out, c)
		}
	}
	return out
}

// filterByPlatform 只保留能服务该平台的 cell。两级 fail-open,保证「能精准就精准、不能就
// 退回今天的盲转移」,永不比现状更差:
//   - want 为空(分组无平台)→ 不过滤(旧行为)。
//   - 单 cell 未上报平台(platforms 为空:旧 Portal / 全是未知号)→ 视为「全平台可服务」保留。
//   - 过滤后全空(没有任何 cell 声称支持该平台)→ 回落到未过滤池,退回盲转移,由「no available
//     accounts」失败转移哨兵兜底。使平台过滤纯粹是一层优化,而非新的失败面。
//
// 有了它,openai/codex 请求只会被投到确有 openai 号的 cell(如 cell4);claude 请求只投到有
// claude 号的 cell —— 平台平权,不再靠 503 试错逐个撞。
func filterByPlatform(pool []cellCandidate, want string) []cellCandidate {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return pool
	}
	out := make([]cellCandidate, 0, len(pool))
	for _, c := range pool {
		if len(c.platforms) == 0 || containsString(c.platforms, want) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return pool // 全空 fail-open:绝不比盲转移更差
	}
	return out
}

// normalizePlatforms 归一 Portal 上报的平台集合:小写、去空白、丢空、去重。中央与 Portal
// 的平台口径本应一致,这里仍做一遍防御式归一,使过滤对大小写/空白/重复不敏感。返回 nil
// 表示「未上报」→ filterByPlatform 视为全平台可服务(fail-open)。
func normalizePlatforms(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
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
	// 静态单 cell 视为 normal 道(蒸馏/批量消费者绝不回落到它)。无信誉信息 → base 50。
	return []cellCandidate{{url: s.target, reputation: 50, lane: laneNormal}}
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
		BaseURL    string   `json:"baseUrl"`
		Reputation int      `json:"reputation"`
		Type       string   `json:"type"`      // 工作道;旧 Portal 不发 → "" → 归一为 normal
		Platforms  []string `json:"platforms"` // 该 cell 有可服务号的平台;旧 Portal 不发 → nil → 不参与平台过滤
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
			cells = append(cells, cellCandidate{
				url:        u,
				reputation: c.Reputation,
				lane:       normalizeLane(c.Type),
				platforms:  normalizePlatforms(c.Platforms),
			})
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
