package service

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

/*
ProxyLivenessService(代理出口 IP 定时探活,cell 内探测)

背景:代理出口 IP 挂掉 → 绑在该 IP 上的账号全部无法出网 → 下游大量请求失败,而
账号本身 status 仍是 active,运营看不出「是代理死了」。cell 已具备探一个代理的机器
(ProxyExitInfoProber.ProbeProxy + ProxyLatencyCache),但只在 admin 手动点「测试代理」
时触发;没有对在役代理的周期性探活。

这个服务补上这一环:EDGE_MODE 下后台循环,周期性对本 cell「active 且有账号绑定」的
代理探一次出口,把成败/延迟/出口 IP/时间写进 ProxyLatencyCache(与手动 TestProxy 写的
是同一份缓存)。Portal 侧 proxy-liveness worker 通过 GET /internal/proxies/health 每 cell
拉一次这份脱敏健康表,归因到账号并打标(只标记,不改 status、不暂停)。

只探有 binding 的代理(省流:没账号用的 IP 死不死不影响任何人)。本服务不搬运凭证、
不回显代理 URL;Health() 返回的健康表也只含 host/port,绝不含密码。
*/

const (
	// 探活间隔:代理挂掉后最迟这么久被发现(叠加随机 jitter 摊平多 cell 同时打)。
	proxyLivenessInterval = 5 * time.Minute
	// 每轮间隔上叠加的最大随机抖动,避免多 cell 齐步走。
	proxyLivenessJitter = 60 * time.Second
	// 启动后延迟首探,避开 boot 期(等 DB / 代理池稳定)。
	proxyLivenessInitialDelay = 45 * time.Second
	// 一轮最多探的代理数(cell 池很小,宽松上限即可)。
	proxyLivenessMaxProxies = 1000
	// 单个代理探测的超时(prober 自身也有超时,这里兜底)。
	proxyLivenessProbeTimeout = 20 * time.Second

	// 批量限速:探测目标 ip-api.com 免费档约 45 次/分。整轮探测按 minGap(+jitter)
	// 依次隔开,即使一个 cell 挂十几条代理、且它们回落到同一出口 IP,也稳稳压在限额
	// 之下(如 15 条 × ~2s ≈ 30s,≤ ~30 次/分),不会自己把自己打成限流。
	proxyLivenessMinGap    = 1500 * time.Millisecond
	proxyLivenessGapJitter = 1000 * time.Millisecond
	// 传输层探测失败(疑似出口死)先短退避重试 1 次,滤掉单次网络抖动再判死。
	proxyLivenessRetryBackoff = 2 * time.Second
	// 写缓存的独立超时:每条探测结果用它自带的短 ctx 落库,绝不复用「整轮」ctx——
	// 否则整轮一旦超时/取消,排在队尾的代理会连写缓存都失败 → 无记录 → 被误标。
	proxyLivenessWriteTimeout = 10 * time.Second
)

// ProxyHealthEntry 是给 Portal 拉取的单条脱敏健康记录(绝不含凭证/代理 URL)。
type ProxyHealthEntry struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Success   bool   `json:"success"`
	LatencyMs *int64 `json:"latency_ms,omitempty"`
	CheckedAt *int64 `json:"checked_at,omitempty"` // unix 秒;从未探过则为 nil
	Message   string `json:"message,omitempty"`
}

// ProxyLivenessService periodically probes this cell's active, bound proxies and
// exposes a scrubbed health table for the Portal to pull. Cell-side only.
type ProxyLivenessService struct {
	repo    ProxyRepository
	prober  ProxyExitInfoProber
	latency ProxyLatencyCache

	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewProxyLivenessService creates the service. It does not start on its own — the
// provider decides whether to Start() based on EDGE_MODE.
func NewProxyLivenessService(repo ProxyRepository, prober ProxyExitInfoProber, latency ProxyLatencyCache) *ProxyLivenessService {
	return &ProxyLivenessService{repo: repo, prober: prober, latency: latency}
}

// Start launches the probe loop (idempotent — safe to call once).
func (s *ProxyLivenessService) Start() {
	if s == nil || s.repo == nil || s.prober == nil || s.latency == nil {
		return
	}
	s.startOnce.Do(func() {
		s.stopCh = make(chan struct{})
		go s.loop()
		logger.LegacyPrintf("service.proxy_liveness", "[ProxyLiveness] started (interval=%s +jitter<=%s)", proxyLivenessInterval, proxyLivenessJitter)
	})
}

// Stop halts the probe loop.
func (s *ProxyLivenessService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
}

func (s *ProxyLivenessService) loop() {
	select {
	case <-s.stopCh:
		return
	case <-time.After(proxyLivenessInitialDelay):
	}
	s.probeOnce()

	for {
		// 每轮 interval + 随机 jitter,摊平多 cell 的探测尖峰。
		wait := proxyLivenessInterval
		if proxyLivenessJitter > 0 {
			wait += time.Duration(rand.Int63n(int64(proxyLivenessJitter)))
		}
		t := time.NewTimer(wait)
		select {
		case <-s.stopCh:
			t.Stop()
			return
		case <-t.C:
			s.probeOnce()
		}
	}
}

// probeOnce probes every active, bound proxy once and writes the result to the
// latency cache (same cache the manual "test proxy" admin action writes).
func (s *ProxyLivenessService) probeOnce() {
	// 元数据读取(列表 + 绑定计数)用短超时 ctx,绝不跨越整轮探测;整轮的时长由每条
	// 探测各自的超时累加决定,循环本身只靠 stopCh 在两条探测之间及时退出。这样即便一轮
	// 里有多条死/慢代理把整轮拖长,排在队尾的代理照样会被探到并写入缓存——根治「队尾
	// 代理无缓存记录 → 被 Portal worker 误标出口异常」。
	listCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	proxies, err := s.repo.ListActive(listCtx)
	cancel()
	if err != nil {
		logger.LegacyPrintf("service.proxy_liveness", "[ProxyLiveness] list active proxies error: %v", err)
		return
	}
	if len(proxies) > proxyLivenessMaxProxies {
		proxies = proxies[:proxyLivenessMaxProxies]
	}

	probed := 0
	for i := range proxies {
		select {
		case <-s.stopCh:
			return
		default:
		}
		p := &proxies[i]
		// 只探有账号绑定的代理:没账号用的 IP 死活不影响任何下游,省流。
		cntCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		cnt, err := s.repo.CountAccountsByProxyID(cntCtx, p.ID)
		c()
		if err != nil {
			logger.LegacyPrintf("service.proxy_liveness", "[ProxyLiveness] count bindings proxy=%d error: %v", p.ID, err)
			continue
		}
		if cnt <= 0 {
			continue
		}
		// 批量限速:每条探测之间隔开 minGap(+jitter),把整轮请求摊平在限额之下。
		// 放在实际发起探测之前,首条不等待。
		if probed > 0 && !s.sleepOrStop(proxyLivenessMinGap+time.Duration(rand.Int63n(int64(proxyLivenessGapJitter)))) {
			return
		}
		s.probeProxy(p)
		probed++
	}
	if probed > 0 {
		logger.LegacyPrintf("service.proxy_liveness", "[ProxyLiveness] probed %d bound proxy(ies)", probed)
	}
}

// sleepOrStop 等待 d,期间若收到停止信号则返回 false(调用方应尽快退出)。
func (s *ProxyLivenessService) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.stopCh:
		return false
	case <-t.C:
		return true
	}
}

// probeProxy probes one proxy and writes success/failure into the latency cache.
// Mirrors adminService.TestProxy's cache write (single source of truth for health).
// 仅当探测判定「出口真出不了网」时先短退避重试 1 次,滤掉单次网络抖动再判死;
// 「可达但地理站没数据(如 ip-api 限流)」直接判活,不重试也不标异常。
func (s *ProxyLivenessService) probeProxy(p *Proxy) {
	info := s.probeAttempt(p)
	if !info.Success {
		// 疑似出口死:退避后重试一次,任一次判活即采用。
		if s.sleepOrStop(proxyLivenessRetryBackoff) {
			if retry := s.probeAttempt(p); retry.Success {
				info = retry
			}
		}
	}
	// 写缓存用独立短 ctx(不复用整轮/探测 ctx):无论整轮多长、是否收到停止信号,
	// 这条已探出的结果都要落库——否则队尾代理探到了却写不进去,等同没探。
	wctx, cancel := context.WithTimeout(context.Background(), proxyLivenessWriteTimeout)
	defer cancel()
	_ = s.latency.SetProxyLatency(wctx, p.ID, info)
}

// probeAttempt 探一次,产出一条(尚未落库的)健康记录。message 均已脱敏。
// 用自带的独立超时 ctx,和「整轮」无关:整轮再长也不会把某次探测提前取消成假失败。
func (s *ProxyLivenessService) probeAttempt(p *Proxy) *ProxyLatencyInfo {
	pctx, cancel := context.WithTimeout(context.Background(), proxyLivenessProbeTimeout)
	defer cancel()

	exit, latencyMs, err := s.prober.ProbeProxy(pctx, p.URL())
	return buildLatencyInfo(exit, latencyMs, err)
}

// buildLatencyInfo 把 prober 的一次原始结果映射成(脱敏的)缓存记录。定时循环与手动
// 测试共用它 → 两条路径判活口径逐字一致,消除「定时说异常、手动说通」的矛盾。
func buildLatencyInfo(exit *ProxyExitInfo, latencyMs int64, err error) *ProxyLatencyInfo {
	now := time.Now()
	if err != nil {
		// 可达但地理探测失败(如 ip-api 限流)→ 出口能出网 → 判活,不标异常。
		var reachErr *ProxyReachableError
		if errors.As(err, &reachErr) {
			info := &ProxyLatencyInfo{
				Success:   true,
				Message:   "reachable (geo unavailable: " + reachErr.Reason + ")",
				UpdatedAt: now,
			}
			if latencyMs > 0 {
				lat := latencyMs
				info.LatencyMs = &lat
			}
			return info
		}
		// 出口真出不了网。err 已由 prober 脱敏(不含代理 URL/凭证)。
		return &ProxyLatencyInfo{
			Success:   false,
			Message:   err.Error(),
			UpdatedAt: now,
		}
	}
	lat := latencyMs
	info := &ProxyLatencyInfo{
		Success:   true,
		LatencyMs: &lat,
		Message:   "Proxy is accessible",
		UpdatedAt: now,
	}
	if exit != nil {
		info.IPAddress = exit.IP
		info.Country = exit.Country
		info.CountryCode = exit.CountryCode
		info.Region = exit.Region
		info.City = exit.City
	}
	return info
}

// resolveProxyID 在本 cell 的在役代理池里,按 host/port/凭证精确匹配出内部 proxyID。
// 供手动探测把结果归到「定时循环写的同一缓存键」。池极小,内存匹配即可,避免扩接口。
// 匹配不到(尚未入池的候选代理)返回 (0,false)。
func (s *ProxyLivenessService) resolveProxyID(ctx context.Context, host string, port int, username, password string) (int64, bool) {
	proxies, err := s.repo.ListActive(ctx)
	if err != nil {
		return 0, false
	}
	for i := range proxies {
		p := &proxies[i]
		if p.Host == host && p.Port == port && p.Username == username && p.Password == password {
			return p.ID, true
		}
	}
	return 0, false
}

// CacheManualProbe 把一次手动探测(ProbeProxy)的结果写进定时循环所用的同一份 latency
// 缓存——当被探代理确实是池内在役代理(host/port/凭证匹配)时。这样手动「测试」与定时
// 探活永不矛盾,Portal 拉 /health 会立刻反映手动结果,不会下一轮又被打回出口异常。
// 对「尚未入池的候选代理」(绑定前校验,匹配不到)或依赖缺失时静默 no-op。
func (s *ProxyLivenessService) CacheManualProbe(ctx context.Context, host string, port int, username, password string, exit *ProxyExitInfo, latencyMs int64, probeErr error) {
	if s == nil || s.repo == nil || s.latency == nil {
		return
	}
	id, ok := s.resolveProxyID(ctx, host, port, username, password)
	if !ok {
		return
	}
	info := buildLatencyInfo(exit, latencyMs, probeErr)
	wctx, cancel := context.WithTimeout(context.Background(), proxyLivenessWriteTimeout)
	defer cancel()
	_ = s.latency.SetProxyLatency(wctx, id, info)
}

// Health returns a scrubbed health table for every active proxy on this cell:
// [{host, port, success, latency_ms, checked_at, message}]. No credentials, no
// proxy URL — the Portal joins on host|port back to CellProxy. Read-only.
func (s *ProxyLivenessService) Health(ctx context.Context) ([]ProxyHealthEntry, error) {
	proxies, err := s.repo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(proxies))
	for i := range proxies {
		ids = append(ids, proxies[i].ID)
	}
	latencies, err := s.latency.GetProxyLatencies(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ProxyHealthEntry, 0, len(proxies))
	for i := range proxies {
		p := &proxies[i]
		info := latencies[p.ID]
		if info == nil {
			// 缓存里没有这条代理的记录 =「还没探到 / 刚入池 / 记录已过期」,是「无数据」
			// 而非「探测失败」。绝不上报成 success=false —— 那会被 Portal worker 兜底成
			// 「proxy probe failed」误标出口异常(旧 bug 正是这条)。省略该行,worker 侧
			// `if(!entry) continue` 会保留其原判定(NULL=未探,不显示徽标)。
			continue
		}
		e := ProxyHealthEntry{Host: p.Host, Port: p.Port}
		e.Success = info.Success
		e.LatencyMs = info.LatencyMs
		e.Message = info.Message
		if !info.UpdatedAt.IsZero() {
			ts := info.UpdatedAt.Unix()
			e.CheckedAt = &ts
		}
		out = append(out, e)
	}
	// 稳定排序,便于 Portal 侧 diff 与日志比对。
	sort.Slice(out, func(a, b int) bool {
		if out[a].Host != out[b].Host {
			return out[a].Host < out[b].Host
		}
		return out[a].Port < out[b].Port
	})
	return out, nil
}
