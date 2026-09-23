package middleware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// EdgeConsumerBiller 由中央在转发成功、从 cell 响应剥出权威用量后调用,给消费者计费
// (#86b)。nil = 不计费(默认关时无意义)。实现见 handler.GatewayHandler。
//
// reqBody 为缓冲的原始请求体(供 Risk V2 影子采集从中算请求侧特征;转发路径本地采集会漏)，
// startedAt 为本次转发开始时间。二者仅用于观测,不参与计费。
type EdgeConsumerBiller func(c *gin.Context, env service.EdgeUsageEnvelope, reqBody []byte, startedAt time.Time)

// EvidenceCaptureFunc 疑似蒸馏取证：转发命中后回调,命中捕获名单则记一条请求原文(脱敏)。
// nil = 不采;内部有零开销闸门,默认无 flag 时一次原子读即返回。仅取证,不影响转发/计费。
type EvidenceCaptureFunc func(c *gin.Context, userID, apiKeyID int64, reqBody []byte)

// ModelAllowFunc 判断请求 model 是否允许转发(转发模型白名单)。
// 由 pricing_models 的"启用模型"集合驱动。nil = 不校验(白名单关)。
type ModelAllowFunc func(ctx context.Context, model string) bool

// EdgeUserSlotFunc 在转发到 cell 之前获取「消费者用户级并发槽位」。
//
// 为什么必须在中央这层限:转发到 cell 的流量在 cell 侧走的是无限并发的可信占位身份
// (api_key_auth EDGE 分支 Concurrency=1<<20),cell 不会限消费者并发;而中央命中转发就
// c.Abort() 短路了 gateway handler,handler 里的 AcquireUserSlotWithWait 也走不到——
// 于是 edge 模式下给用户设的并发上限两头落空。由本回调在转发前补上这一层。
//
// 返回 (release, ok):ok=true → 已持槽,release 用于本次转发结束时释放(可能是 no-op
// 空函数,如无身份/不限并发);ok=false → 已按客户端协议写好「并发超限」错误响应,调用方
// 直接 c.Abort() 收尾。isStream 供排队等待时决定是否发 SSE ping 保活(与 handler 本地
// 路径一致)。nil = 不接线(默认 no-op,不限并发)。
type EdgeUserSlotFunc func(c *gin.Context, isStream bool) (release func(), ok bool)

// EdgeRPMCheckFunc 在转发到 cell 之前执行「消费者 RPM 限流」(override → group → user/平台)。
//
// 与 EdgeUserSlotFunc 同因:RPM 的唯一检查点在 handler 的 CheckBillingEligibility 里,中央命中
// 转发即 c.Abort() 短路 handler,cell 侧对转发流量又是免检的可信身份——不在这里补,edge 模式下
// 用户级/分组级/覆盖级 RPM 三层全部失效。返回 true = 放行;false = 已按客户端协议写好 429
// (含 Retry-After),调用方直接 c.Abort() 收尾。nil = 不接线(不限 RPM)。
type EdgeRPMCheckFunc func(c *gin.Context) bool

// EdgeForward 是中央网关“执行→转发”中间件。
//
// 开启且请求组 slug 命中配置列表时,把该 /v1 请求手动流式反向代理到边缘 cell(不带
// 中央凭据,cell 用它本地的号执行),SSE/WS 逐块原样回传;否则放行走中央本地执行。
//
// 选路(P3-2):配了 RegistryURL 就从 Portal 动态拉取存活 cell(按健康分降序)并
// 缓存,按分数选最优;传输失败(写任何响应前)顺位转移到下一候选,全挂才 502。未配
// RegistryURL 则退回静态单 CellURL(旧行为);动态模式下 static 作为最后兜底候选。
//
// 默认关(Enabled=false / Groups 空 / 既无 CellURL 也无 RegistryURL)= 完全 no-op。
//
// 用手写流式代理而非 httputil.ReverseProxy:后者会触碰 gin 的 CloseNotify(在某些
// ResponseWriter / h2c 下会 panic),且手写更利于逐块 flush SSE。
func EdgeForward(cfg config.EdgeForwardConfig, biller EdgeConsumerBiller, modelAllowed ModelAllowFunc, capture EvidenceCaptureFunc, acquireUserSlot EdgeUserSlotFunc, checkRPM EdgeRPMCheckFunc) gin.HandlerFunc {
	noop := func(c *gin.Context) { c.Next() }
	if !cfg.Enabled || len(cfg.Groups) == 0 {
		return noop
	}

	// 静态 cell(可选):RegistryURL 为空时是唯一目标;有 Registry 时作兜底候选。
	var static *url.URL
	if s := strings.TrimSpace(cfg.CellURL); s != "" {
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" || u.Host == "" {
			slog.Error("edge_forward: 无效的 cell_url,已忽略", "cell_url", cfg.CellURL, "err", err)
		} else {
			static = u
		}
	}

	var resolver cellResolver
	if ru := strings.TrimSpace(cfg.RegistryURL); ru != "" {
		d := &dynamicResolver{static: static}
		interval := time.Duration(cfg.RefreshSeconds) * time.Second
		if interval <= 0 {
			interval = 15 * time.Second
		}
		startRegistryRefresh(context.Background(), d, ru, strings.TrimSpace(cfg.RegistryToken), interval)
		resolver = d
		slog.Info("edge_forward: 动态选路启用", "registry", ru, "refresh", interval.String())
	} else if static != nil {
		resolver = &staticResolver{target: static}
		slog.Info("edge_forward: 静态选路启用", "cell_url", static.String(), "groups", cfg.Groups, "has_key", strings.TrimSpace(cfg.Key) != "")
	} else {
		slog.Error("edge_forward: 已启用但既无 cell_url 也无 registry_url,转发禁用(no-op)")
		return noop
	}

	groupSet := make(map[string]struct{}, len(cfg.Groups))
	for _, g := range cfg.Groups {
		if s := strings.TrimSpace(g); s != "" {
			groupSet[s] = struct{}{}
		}
	}
	// 转发模型白名单(可选):仅当开关开且注入了校验器时生效。
	var checker ModelAllowFunc
	if cfg.ModelWhitelist {
		if modelAllowed == nil {
			slog.Error("edge_forward: 模型白名单已开但未注入校验器,白名单不生效")
		} else {
			checker = modelAllowed
			slog.Info("edge_forward: 模型白名单启用(仅转发 pricing_models 启用模型)")
		}
	}
	// 组→工作道映射(护号需求侧路由):构造期规范化一次,避免每请求重复归一。
	// 防呆:原值非空、非 normal,却被归一成 normal(多半是 lane 拼错)→ warn —— 否则
	// 会把一个本应隔离的组静默路由到好号 cell。
	groupLanes := make(map[string]string, len(cfg.GroupLanes))
	for slug, lane := range cfg.GroupLanes {
		s := strings.TrimSpace(slug)
		if s == "" {
			continue
		}
		nl := normalizeLane(lane)
		if raw := strings.TrimSpace(lane); nl == laneNormal && raw != "" && !strings.EqualFold(raw, laneNormal) {
			slog.Warn("edge_forward: 未识别的 lane,已按 normal 处理(护号风险,请检查 EDGE_FORWARD_GROUP_LANES)", "group", s, "lane", lane)
		}
		groupLanes[s] = nl
	}
	if len(groupLanes) > 0 {
		slog.Info("edge_forward: 组→工作道路由启用", "group_lanes", groupLanes)
	}

	return newEdgeForwardHandler(resolver, groupSet, groupLanes, strings.TrimSpace(cfg.Key), rand.Float64, biller, checker, capture, acquireUserSlot, checkRPM, edgeEarlyPing{after: time.Duration(cfg.EarlyPingAfterSeconds) * time.Second, interval: time.Duration(cfg.EarlyPingIntervalSeconds) * time.Second})
}

// edgeCellDialTimeout 是 central→cell 转发的连接建立(dial)超时。内部机房跳,健康 cell
// 毫秒级连上;设短是为了让「刚死但仍在缓存」的 cell 快速失败、快速失败转移,而不是拖满
// DefaultTransport 默认的 30s。只收紧连接阶段,不影响已建立连接后的流式回传。
const edgeCellDialTimeout = 5 * time.Second

// newEdgeCellTransport 克隆 http.DefaultTransport,仅把 dial 超时收紧到 edgeCellDialTimeout。
// 其余(连接池、keep-alive、代理、TLS、HTTP/2 等)沿用默认;刻意不设 ResponseHeaderTimeout /
// 整体超时,以免截断慢首字/长 SSE。
func newEdgeCellTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// 理论不可达(标准库 DefaultTransport 恒为 *http.Transport);兜底给一个等价实现。
		return &http.Transport{
			DialContext:           (&net.Dialer{Timeout: edgeCellDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	t := base.Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   edgeCellDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return t
}

// newEdgeForwardHandler 构造转发处理函数(组命中→加权随机选序→WS/失败转移流式回传)。
// 与配置解析分离,便于用注入的 resolver + 确定性 rng 测试选路/失败转移。
// edgeEarlyPing:流式请求等 cell 响应头超过 after 仍无结果时,中央先回 200+SSE 头并每
// interval 发一个 SSE 注释帧(": \n\n",任何 SSE 客户端都会忽略)保活,避免 Cloudflare 100s
// 源站超时(524)。after<=0 关闭。
type edgeEarlyPing struct {
	after    time.Duration
	interval time.Duration
}

func newEdgeForwardHandler(resolver cellResolver, groupSet map[string]struct{}, groupLanes map[string]string, forwardKey string, rng func() float64, biller EdgeConsumerBiller, modelAllowed ModelAllowFunc, capture EvidenceCaptureFunc, acquireUserSlot EdgeUserSlotFunc, checkRPM EdgeRPMCheckFunc, earlyPing edgeEarlyPing) gin.HandlerFunc {
	// 流式:不设 Client.Timeout(否则会截断长 SSE);客户端断开由请求 Context 取消传导。
	// 但把 central→cell 的 dial(连接建立)超时从 DefaultTransport 的 30s 收紧到
	// edgeCellDialTimeout:cell 池每 15s 才从 Portal 刷新一次 routable,存在「cell 刚死、
	// 缓存未刷掉」的陈旧窗口;若选中刚死的 cell,30s dial 会拖满才失败转移,多个死 cell
	// 叠加逼近 CF 100s 会话窗、把本可在后续 cell 成功的请求拖成 502。健康 cell 是机房内
	// 毫秒级连上,收紧 dial 不误伤,只影响连接阶段(response/读侧不设超时,长 SSE 不受影响)。
	client := &http.Client{Transport: newEdgeCellTransport()}
	// 会话→cell 亲和(P3-3c),处理函数生命周期内共享。
	affinity := newSessionAffinity(stickyAffinityTTL)

	return func(c *gin.Context) {
		reqStart := time.Now() // 转发开始时间;仅供 Risk V2 观测,不影响计费/响应。
		apiKey, ok := GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || apiKey.Group == nil {
			slog.Debug("edge_forward: 跳过(无 apiKey/分组)", "path", c.Request.URL.Path,
				"has_apikey", ok && apiKey != nil, "group_nil", apiKey == nil || apiKey.Group == nil)
			c.Next()
			return
		}
		if _, hit := groupSet[apiKey.Group.Slug]; !hit {
			slog.Debug("edge_forward: 跳过(组 slug 未命中转发列表)", "path", c.Request.URL.Path,
				"key_group_slug", apiKey.Group.Slug, "forward_groups", func() []string {
					gs := make([]string, 0, len(groupSet))
					for g := range groupSet {
						gs = append(gs, g)
					}
					return gs
				}())
			c.Next()
			return
		}
		// 只读元数据端点(列模型 /v1/models、查用量 /v1/usage)由中央直接服务,不转发到 cell:
		// 这类请求不调用模型、请求体无 model,若走转发+模型白名单会被误判 403 model not allowed。
		// 列模型菜单归中央(受分组「自定义 /v1/models 列表」等展示配置控制),与调用/调度无关。
		if c.Request.Method == http.MethodGet {
			if p := c.Request.URL.Path; strings.HasSuffix(p, "/models") || strings.HasSuffix(p, "/usage") {
				c.Next()
				return
			}
		}

		// 消费者工作道(护号):优先读组自身的 lane(后台可视化、免重启、近实时生效);
		// 为 normal/空时再退回 env 映射 EDGE_FORWARD_GROUP_LANES(过渡期兼容,迁完可删)。
		consumerLane := normalizeLane(apiKey.Group.Lane)
		if consumerLane == laneNormal {
			if l, ok := groupLanes[apiKey.Group.Slug]; ok {
				consumerLane = l // 构造期已归一
			}
		}
		slog.Debug("edge_forward: 命中,转发到 cell", "path", c.Request.URL.Path, "key_group_slug", apiKey.Group.Slug, "lane", consumerLane)

		// 严格隔离:先按工作道过滤候选,再按平台过滤,最后加权随机 —— 使信誉权重只在同道内
		// 比较,且非 normal 消费者绝不落到别道 cell。平台过滤(filterByPlatform,两级 fail-open)
		// 把请求只投给确有该平台号的 cell(openai 请求→有 ChatGPT 号的 cell),不再靠 503 试错
		// 逐个撞;cell 未上报平台或全池都不支持时自动退回未过滤池,永不比盲转移更差。加权随机
		// 选序(P3-3b):首个 = 加权首选,其余顺位作失败转移候选。
		reqPlatform := apiKey.Group.Platform // 组平台口径(anthropic|openai|gemini|antigravity);空 → 不过滤
		order := weightedOrder(filterByPlatform(filterByLane(resolver.poolCandidates(), consumerLane), reqPlatform), rng)
		// 静态兜底仅对 normal 消费者生效(静态 cell 视为 normal 道):蒸馏/批量消费者
		// 绝不回落到它。
		if consumerLane == laneNormal {
			if fb := resolver.fallback(); fb != nil {
				present := false
				for _, u := range order {
					if u.String() == fb.String() {
						present = true
						break
					}
				}
				if !present {
					order = append(order, fb)
				}
			}
		}
		if len(order) == 0 {
			// 对应工作道无可用 cell → 502,绝不跨道、绝不本地执行(fail-closed 护号)。
			slog.Error("edge_forward: 无可路由 cell", "path", c.Request.URL.Path, "lane", consumerLane)
			writeEdgeError(c)
			return
		}

		if isWebSocketUpgrade(c.Request) {
			// WS 无请求体可重放,失败转移意义有限:用加权首选,拨号失败即收尾。
			proxyWebSocket(c, order[0], forwardKey)
			c.Abort()
			return
		}

		// 缓冲请求体以支持失败转移(同一请求重放给下一候选)。上游 bodyLimit 已封顶,
		// 内存可控;响应仍是流式,不缓冲。
		var body []byte
		if c.Request.Body != nil {
			b, rerr := io.ReadAll(c.Request.Body)
			_ = c.Request.Body.Close()
			if rerr != nil {
				writeEdgeError(c)
				return
			}
			body = b
		}

		// 疑似蒸馏取证：命中捕获名单则记一条请求原文(内部零开销闸门 + 异步脱敏存储)。
		// 放在此处 → 覆盖成功/失败/白名单拒绝所有情况;仅取证,绝不影响转发。
		if capture != nil {
			capture(c, apiKey.UserID, apiKey.ID, body)
		}

		// 转发模型白名单(可选):请求 model 不在"价格展示启用模型"内 → 直接 403,
		// 不转发 cell(从源头避免未配价模型下游/Portal 失败)。
		if modelAllowed != nil {
			reqModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
			if !modelAllowed(c.Request.Context(), reqModel) {
				reqURL := ClientRequestURL(c)
				slog.Info("edge_forward: 模型不在白名单,拒绝转发", "model", reqModel, "path", c.Request.URL.Path, "url", reqURL)
				// 模型白名单拒绝是中转自己的策略闸门(该 model 未配价/未启用),非可用性故障 →
				// 标记业务限制,排除出 SLA/健康分(仍留错误列表可见)。与内容审核拦截同一套路。
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
				// 运维行的 model 列 + 错误文案都带上客户端实际发的模型和 Base URL,否则只见一句
				// "model not allowed",分不清是模型名写错还是通道选错。
				if reqModel != "" {
					c.Set(service.OpsRequestModelKey, reqModel)
				}
				c.Header("Content-Type", "application/json")
				c.String(http.StatusForbidden, modelNotAllowedBody(reqModel, c.Request.Method, reqURL))
				c.Abort()
				return
			}
		}

		// 模型平台 ≠ 分组平台(如 key 绑在 Claude 组、请求 gpt-* 模型却没带 GPT 组的 slug 前缀):
		// 转到 cell 也只会被拒回一条看不懂的 404 "model: gpt-xxx"。在源头回 400 并说明怎么改
		// (带上分组 slug 前缀选对通道),不占 cell、不占并发槽。只在两侧平台都能判定时拦;
		// 模型前缀不认识(PlatformForModelName 返回空)或分组无平台时放行,交由既有链路处理。
		if apiKey.Group != nil {
			reqModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
			if msg := edgeModelPlatformMismatch(reqModel, apiKey.Group); msg != "" {
				slog.Info("edge_forward: 模型平台与分组平台不一致,拒绝转发", "model", reqModel, "group_slug", apiKey.Group.Slug, "group_platform", apiKey.Group.Platform)
				// 客户端选错通道,非可用性故障 → 标记业务限制,排除出 SLA/健康分(仍留错误列表可见)。
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
				writeEdgeInvalidRequest(c, apiKey.Group.Platform, msg)
				return
			}
		}

		// 消费者用户级并发限制(中央职责):转发前获取消费者的用户并发槽位。cell 侧对
		// 转发流量不限并发,不在这里限,edge 模式下用户并发上限就完全失效。槽位持有到本次
		// 转发结束(defer 释放),客户端断开由 wrapReleaseOnDone 兜底。放在白名单校验之后、
		// 选号/转发之前:被白名单/无可路由 cell 拒掉的请求不占槽。acquire 失败(队列满/超时)
		// 时回调已按客户端协议写好错误响应,这里直接收尾。
		isStream := gjson.GetBytes(body, "stream").Bool()
		if acquireUserSlot != nil {
			release, ok := acquireUserSlot(c, isStream)
			if !ok {
				c.Abort()
				return
			}
			if release != nil {
				defer release()
			}
		}

		// 消费者 RPM 限流(中央职责,与并发同因):handler 里的 CheckBillingEligibility 被
		// c.Abort() 短路,cell 侧对转发流量免检,这里补上 RPM 级联(override → group → user/平台)。
		// 放在持槽之后,与本地 handler「先占槽、再校验」的顺序一致;超限时回调已写好 429。
		if checkRPM != nil && !checkRPM(c) {
			c.Abort()
			return
		}

		// 会话亲和(P3-3c):进行中会话固定回同一 cell —— sub2api 的号级粘性只有在
		// 请求先落到同一 cell 时才生效,所以这层的 cell 亲和是整个 Sticky 的前提。
		// 仅当绑定的 cell 仍在本轮候选池中(存活可路由)才生效,否则忽略陈旧绑定。
		stickyKey := stickySessionKey(body)
		if stickyKey != "" {
			if bound, okBound := affinity.get(stickyKey); okBound {
				order = moveToFront(order, bound)
			}
		}

		var lastErr error
		// 逐候选结果:host=no_accounts(cell 选号前失败) / host=transport_err(连不上)。
		// 全候选耗尽时一并打出 —— 用来区分「好 cell 压根没进候选」(控制面/心跳排除,
		// 此处根本不会出现该 host)与「进了候选但回没号」(cell 侧选号问题)。
		outcomes := make([]string, 0, len(order))
		earlyStarted := false
		for i, target := range order {
			outURL := *target
			outURL.Path = c.Request.URL.Path
			outURL.RawQuery = c.Request.URL.RawQuery
			outReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, outURL.String(), bytes.NewReader(body))
			if err != nil {
				writeEdgeError(c)
				return
			}
			copyProxyHeaders(outReq.Header, c.Request.Header)
			outReq.ContentLength = int64(len(body))
			if forwardKey != "" {
				outReq.Header.Set("Authorization", "Bearer "+forwardKey)
				outReq.Header.Del("X-Api-Key")
				outReq.Header.Del("x-api-key")
			}
			// 携带消费者平台到 cell(仅非默认 anthropic):cell 的 edge 信任分支据此设
			// ForcePlatform → 选对平台的号并路由到对应处理器。先 Del(清掉 copyProxyHeaders
			// 可能透传的客户端伪造值)再按真实分组平台 Set;claude(anthropic)不打标 → cell
			// 走原「无分组默认 claude」路径，零改动。
			outReq.Header.Del(EdgeForwardPlatformHeader)
			if apiKey.Group != nil {
				if p := apiKey.Group.Platform; p != "" && p != service.PlatformAnthropic {
					outReq.Header.Set(EdgeForwardPlatformHeader, p)
				}
			}

			// 早期心跳:等 cell 响应头期间若超过阈值,先给客户端回 SSE 头 + 心跳(防 524)。
			// earlyStarted=true 之后本次请求已对客户端"承诺"了 200 流,不能再转移候选,
			// 后续任何失败都改写成 event: error 帧收尾。
			resp, err := doCellRequestWithEarlyPing(c, client, outReq, earlyPing, isStream, &earlyStarted)
			if err != nil {
				// 客户端已断开(用户按 ESC / 新一轮 / 客户端超时重试)→ 请求 Context 被取消,
				// client.Do 对每个候选都会瞬间返回 context canceled。这不是 cell 的问题:
				// 立即收尾,绝不再空跑其它候选,也不记 502 / 不打 ERROR「所有候选均不可达」
				// (等价 nginx 499「客户端主动关闭」)。否则会灌满错误面板、淹没真实的
				// cell 不可达告警。
				if c.Request.Context().Err() != nil || errors.Is(err, context.Canceled) {
					slog.Info("edge_forward: 客户端取消请求,停止转发",
						"cell", target.Host, "idx", i)
					c.Abort()
					return
				}
				if earlyStarted {
					slog.Warn("edge_forward: 心跳已开始后转发到 cell 失败,以 SSE 错误帧收尾",
						"cell", target.Host, "idx", i, "err", err)
					writeSSETerminalErrorMessage(c, "edge cell unreachable")
					c.Abort()
					return
				}
				// 仅“写任何响应前”的传输错误才转移;一旦开始回传就不再转移
				// (避免把非幂等请求重放到第二个号 → 双执行)。
				lastErr = err
				outcomes = append(outcomes, target.Host+"=transport_err")
				slog.Warn("edge_forward: 转发到 cell 失败,尝试下一候选",
					"cell", target.Host, "idx", i, "err", err)
				continue
			}
			// 选号前失败(503「no available accounts」):cell 在调上游之前就没挑到
			// 账号 —— 它压根没执行这次请求,所以改投下一个 cell 不会双执行。视同该
			// 候选不可用、顺位转移,而不是把 503 透传给客户端。这样只要任一 cell 有
			// 空号,请求就不会失败(解决 cell 间容量不均 / 单号被限流)。其它 503 /
			// 正常响应仍按下方原样回传,绝不转移。
			if resp.StatusCode == http.StatusServiceUnavailable {
				peek, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				if isCellNoAvailableAccounts(peek) && !earlyStarted {
					lastErr = errCellNoAvailableAccounts
					outcomes = append(outcomes, target.Host+"=no_accounts")
					slog.Warn("edge_forward: cell 无可用账号(选号前失败),尝试下一候选",
						"cell", target.Host, "idx", i)
					continue
				}
				if earlyStarted {
					// 心跳已开始:不能再转移,把 503 body 改写成 SSE 错误帧。
					recordEdgeSideChannelHeaders(c, resp.Header)
					writeSSETerminalErrorBody(c, peek)
					c.Abort()
					return
				}
				// 其它 503(cell/上游真实错误)→ 原样回传已缓冲的响应,不转移。
				relayBufferedResponse(c, resp, peek)
				c.Abort()
				return
			}
			// 诊断:透传 cell 的非 2xx 响应前点名是哪台 cell + 什么状态码。否则中央
			// 把 cell 的 502(如 gpt 号出口传输耗尽的 "Upstream request failed")静默
			// 二传给客户端,运维只看到中央一条空账号/空模型的 502,却不知道是哪台 cell
			// 出的错 —— 要逐台 grep 才能定位。有了这条即可直接去对应 cell 捞
			// openai.upstream_transport_error / *_failover_exhausted 的逐号真死因。
			if resp.StatusCode >= http.StatusBadRequest {
				slog.Warn("edge_forward: cell 回传上游错误,原样透传",
					"cell", target.Host, "idx", i, "status", resp.StatusCode,
					"path", c.Request.URL.Path, "group_slug", apiKey.Group.Slug)
			}
			// 成功落到某 cell → 绑定会话亲和(下一轮同会话回到这台 cell)。
			if stickyKey != "" {
				affinity.put(stickyKey, target)
			}
			idlePing := earlyPing.interval
			if earlyPing.after <= 0 {
				idlePing = 0 // 早期心跳整体关闭时,透传阶段心跳一并关闭(同一开关)
			}
			env, interrupted := streamCellResponse(c, resp, earlyStarted, idlePing)
			if interrupted && stickyKey != "" {
				// 中央↔cell 流在中途断掉(cell 重启/网络抖动/GOAWAY):streamCellResponse
				// 已补发一帧 event: error 终止帧,这里再驱逐会话亲和,下一轮同会话重试就
				// 改选其它 cell,而不是被粘回这台死 cell —— 否则客户端会一直 api_error,
				// 只能新开对话才能逃(edge 版补 cell 侧 d9f780793 没覆盖的那半)。
				affinity.delete(stickyKey)
			}
			// #86b:cell 带回权威用量 → 给消费者计费(占位号,不重复发 provider 用量)。
			// reqBody/reqStart 顺带传给 biller 供 Risk V2 影子采集(不参与计费)。
			if env != nil && biller != nil {
				biller(c, *env, body, reqStart)
			}
			c.Abort()
			return
		}
		slog.Error("edge_forward: 所有候选 cell 均不可达", "candidates", len(order), "outcomes", outcomes, "err", lastErr)
		writeEdgeError(c)
	}
}

// errCellNoAvailableAccounts marks a candidate cell that returned 503
// 「no available accounts」— a pre-upstream scheduling miss. Used only for the
// final all-candidates-exhausted log line.
var errCellNoAvailableAccounts = errors.New("cell has no available accounts")

// isCellNoAvailableAccounts reports whether a 503 body is the gateway's
// "no available accounts" scheduling failure. That failure happens BEFORE the
// cell calls upstream, so retrying the request on another cell is
// idempotency-safe (the request was never executed).
func isCellNoAvailableAccounts(body []byte) bool {
	return bytes.Contains(bytes.ToLower(body), []byte("no available accounts"))
}

// recordEdgeUpstreamCause 解析 cell 回传的脱敏错误分类头(EdgeUpstreamCauseHeader,
// 值为 "<upstreamStatus>|<slug>"),写入中央 ops context —— 转发请求在中央不走 gateway
// handler,故中央原本只能看到被压平的笼统 502;此处把 cell 已知的真实分类补进 ops,
// 由 OpsErrorLoggerMiddleware 落库。该头不透传给消费者客户端(调用方已 continue 跳过拷贝)。
func recordEdgeUpstreamCause(c *gin.Context, headerVal string) {
	status, slug, ok := service.ParseEdgeUpstreamCauseHeader(headerVal)
	if !ok {
		return
	}
	// slug 走权威 key(分类器优先读它);message 只在摘要头没先到时用 slug 兜底——
	// 响应头 map 遍历无序,Detail 头可能先/后到,两种顺序都要让 message 最终是上游原话。
	service.SetOpsUpstreamCauseSlug(c, slug)
	if service.HasOpsUpstreamErrorMessage(c) {
		service.SetOpsUpstreamError(c, status, "", "")
		return
	}
	service.SetOpsUpstreamError(c, status, slug, "")
}

// recordEdgeUpstreamDetail 解析 cell 回传的上游摘要头(EdgeUpstreamDetailHeader):把脱敏后的
// 上游原话写成中央 ops 行的 upstream_error_message,并追加一条上游事件(带 cell 上的账号名/
// 平台/上游 request id),让运维弹窗不用翻 cell 日志就能看到"哪个号、上游说了什么"。
// 注意 cell 的 account_id 是 cell 本地库主键,与中央 accounts 表无关,只进事件 JSON、不进
// ops 行的 account_id 列(否则 JOIN 到中央无关的账号)。该头不透传给客户端。
func recordEdgeUpstreamDetail(c *gin.Context, headerVal string) {
	d := service.ParseEdgeUpstreamDetailHeader(headerVal)
	if d == nil {
		return
	}
	service.SetOpsUpstreamError(c, d.Status, d.Message, "")
	kind := d.Kind
	if kind == "" {
		kind = "edge_relay"
	}
	service.AppendOpsUpstreamError(c, service.OpsUpstreamErrorEvent{
		Platform:           d.Platform,
		AccountID:          d.AccountID,
		AccountName:        d.AccountName,
		UpstreamStatusCode: d.Status,
		UpstreamRequestID:  d.RequestID,
		Kind:               kind,
		Message:            d.Message,
	})
}

// relayBufferedResponse writes a fully-buffered cell response (headers + status
// + body) to the client. Used for a non-failover 503 whose body was already read
// to classify it — mirrors streamCellResponse's header filtering.
func relayBufferedResponse(c *gin.Context, resp *http.Response, body []byte) {
	h := c.Writer.Header()
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) || strings.EqualFold(k, service.EdgeUsageHeader) {
			continue
		}
		if strings.EqualFold(k, service.EdgeUpstreamCauseHeader) {
			recordEdgeUpstreamCause(c, resp.Header.Get(k))
			continue // 错误分类边信道:写入中央 ops,不透传给客户端
		}
		if strings.EqualFold(k, service.EdgeUpstreamDetailHeader) {
			recordEdgeUpstreamDetail(c, resp.Header.Get(k))
			continue // 上游摘要边信道:写入中央 ops,不透传给客户端
		}
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(body)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

// streamCellResponse 回写 cell 响应头+状态码,再逐块流式回传(SSE 每块 flush)。
// 一旦调用即已选定某个 cell,不再失败转移(见 EdgeForward 循环内注释)。
//
// #86b:顺带取出 cell 带回的权威用量给中央做消费者计费:
//   - 非流式:X-Sub2api-Usage 响应头(不透传给客户端);
//   - 流式:末尾的 `event: sub2api_usage` 事件——**剥掉不透传**,只捕获它的 data。
//
// 返回 (捕获到的 envelope, interrupted)。interrupted=true 表示 SSE 流被异常截断
// (中央↔cell 中途断),此时已向客户端补发一帧终止错误,上层应据此驱逐会话亲和。
//
// headersSent=true 表示早期心跳已向客户端写出 200+SSE 头:此时不再拷贝 cell 响应头/状态码,
// cell 回的非 2xx 改写成一帧 event: error 收尾;2xx SSE 正文照常逐行透传。
//
// idlePing>0 时,SSE 透传阶段只要 cell 连续 idlePing 没有字节过来,就给客户端补一个 SSE 注释帧
// (": ping")保活 —— 覆盖"cell 已回头(如账号排队时先发了心跳)、随后等上游首字节几十秒到
// 几分钟"的空窗,否则 Cloudflare 的 Proxy Read Timeout(两次字节间隔上限)照样 524。
// 只对 text/event-stream 生效,JSON 响应绝不插字节。
func streamCellResponse(c *gin.Context, resp *http.Response, headersSent bool, idlePing time.Duration) (*service.EdgeUsageEnvelope, bool) {
	defer resp.Body.Close()
	flusher, _ := c.Writer.(http.Flusher)
	if headersSent {
		recordEdgeSideChannelHeaders(c, resp.Header)
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if resp.StatusCode >= http.StatusBadRequest || !strings.Contains(ct, "text/event-stream") {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if resp.StatusCode < http.StatusBadRequest {
				// 心跳已按 SSE 承诺,cell 却回了非 SSE 的 2xx(理论上不会:早期心跳只对
				// stream:true 请求开启)。无法把 JSON 塞进 SSE 流,按错误终止。
				slog.Warn("edge_forward: 心跳已开始后 cell 回非 SSE 响应,按错误终止", "status", resp.StatusCode, "content_type", ct)
				writeSSETerminalErrorMessage(c, "edge cell returned non-stream response")
				return nil, false
			}
			slog.Warn("edge_forward: 心跳已开始后 cell 回上游错误,改写为 SSE 错误帧", "status", resp.StatusCode)
			writeSSETerminalErrorBody(c, body)
			return nil, false
		}
	} else {
		h := c.Writer.Header()
		for k, vv := range resp.Header {
			if isHopByHopHeader(k) || strings.EqualFold(k, service.EdgeUsageHeader) {
				continue // 用量头不透传给消费者客户端
			}
			if strings.EqualFold(k, service.EdgeUpstreamCauseHeader) {
				recordEdgeUpstreamCause(c, resp.Header.Get(k))
				continue // 错误分类边信道:写入中央 ops,不透传给客户端
			}
			if strings.EqualFold(k, service.EdgeUpstreamDetailHeader) {
				recordEdgeUpstreamDetail(c, resp.Header.Get(k))
				continue // 上游摘要边信道:写入中央 ops,不透传给客户端
			}
			for _, v := range vv {
				h.Add(k, v)
			}
		}
		c.Writer.WriteHeader(resp.StatusCode)
	}

	var captured *service.EdgeUsageEnvelope
	// 非流式:用量在响应头(cell 后续会加;body 原样拷)。
	if hv := resp.Header.Get(service.EdgeUsageHeader); hv != "" {
		if env, err := service.ParseEdgeUsageEnvelope([]byte(hv)); err == nil {
			captured = &env
		}
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		// 非 SSE:原样拷贝。截断无法用 SSE error 帧兜底(会破坏 JSON body),故不合成、
		// 不回报 interrupted。
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := c.Writer.Write(buf[:n]); werr != nil {
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}
		return captured, false
	}

	// SSE:逐行转发;识别并剥掉 sub2api_usage 事件、捕获其 data。ReadString 对任意长
	// 度的 data 行安全(bufio.Scanner 有 token 上限)。
	//
	// 终止判定(sawTerminal):edge 可信流「成功收尾」一定以权威用量哨兵
	// event: sub2api_usage 结束(b3c15858f);message_stop / [DONE] / cell 自发的
	// event: error 也算已正常终止。若循环退出前一个终止标记都没见到(中央↔cell 连接
	// 在流中途断:cell 重启 / 网络抖动 / HTTP2 GOAWAY),说明流被截断 —— 补一帧
	// event: error 让客户端认得是「错误终止」而非「断流」,不再静默断线重连,并回报
	// interrupted=true 让上层驱逐会话亲和。这是 cell 侧 gateway_handler d9f780793 修复
	// 在「中央中继」这半的对应补丁。
	reader := bufio.NewReader(resp.Body)
	inSentinel := false
	sawTerminal := false
	clientGone := false

	// 读 cell 的 goroutine:逐行送进 channel;函数返回时 defer 关闭 resp.Body 会让它以错误退出。
	type cellLine struct {
		line string
		err  error
	}
	lines := make(chan cellLine, 64)
	go func() {
		for {
			line, rerr := reader.ReadString('\n')
			lines <- cellLine{line: line, err: rerr}
			if rerr != nil {
				return
			}
		}
	}()
	var idleC <-chan time.Time
	var idleTicker *time.Ticker
	if idlePing > 0 {
		idleTicker = time.NewTicker(idlePing)
		defer idleTicker.Stop()
		idleC = idleTicker.C
	}
	for {
		var line string
		var rerr error
		select {
		case cl := <-lines:
			line, rerr = cl.line, cl.err
			if idleTicker != nil {
				idleTicker.Reset(idlePing) // 有字节就重新计时
			}
		case <-idleC:
			// cell 空闲超过 idlePing:补心跳。写失败 = 客户端断开。
			if _, werr := c.Writer.Write([]byte(": ping\n\n")); werr != nil {
				return captured, false
			}
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "event: "+service.EdgeUsageEventName:
				inSentinel = true  // 该事件所有行都丢弃,不透传
				sawTerminal = true // 用量哨兵 = cell 正常收尾的权威标记
			case inSentinel:
				if strings.HasPrefix(trimmed, "data: ") {
					if env, err := service.ParseEdgeUsageEnvelope([]byte(strings.TrimPrefix(trimmed, "data: "))); err == nil {
						captured = &env
					}
				}
				if trimmed == "" {
					inSentinel = false // 空行 = 事件结束
				}
			default:
				if trimmed == "event: message_stop" || trimmed == "event: error" || trimmed == "data: [DONE]" {
					sawTerminal = true // 正常终止事件(或 cell 自发的错误终止)
				}
				if _, werr := c.Writer.Write([]byte(line)); werr != nil {
					clientGone = true // 客户端断开
				} else if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if clientGone {
			return captured, false // 客户端断开:非 cell 问题,不合成、不驱逐亲和
		}
		if rerr != nil {
			interrupted := !sawTerminal
			if interrupted {
				writeSSETerminalError(c, flusher)
			}
			return captured, interrupted
		}
	}
}

// writeSSETerminalError 在 SSE 流被异常截断时补发一帧标准终止错误。必须带 event: error
// 行:缺了它 SSE 事件名默认为 message,客户端 SDK 不认作终止事件,会当成断流 → 断线
// 重连(见 gateway_handler d9f780793)。message 用通用文案,绝不含任何内部地址/凭据。
func writeSSETerminalError(c *gin.Context, flusher http.Flusher) {
	const frame = "event: error\n" +
		`data: {"type":"error","error":{"type":"upstream_error","message":"edge cell stream interrupted"}}` + "\n\n"
	if _, err := c.Writer.Write([]byte(frame)); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// recordEdgeSideChannelHeaders 只把 cell 的两条脱敏边信道头写进中央 ops 上下文,不向
// 客户端拷贝任何头(早期心跳已写出响应头之后用)。
func recordEdgeSideChannelHeaders(c *gin.Context, hdr http.Header) {
	if v := hdr.Get(service.EdgeUpstreamCauseHeader); v != "" {
		recordEdgeUpstreamCause(c, v)
	}
	if v := hdr.Get(service.EdgeUpstreamDetailHeader); v != "" {
		recordEdgeUpstreamDetail(c, v)
	}
}

// writeSSETerminalErrorMessage 以给定文案写一帧 event: error 收尾(心跳已开始后用)。
func writeSSETerminalErrorMessage(c *gin.Context, msg string) {
	payload, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "upstream_error", "message": msg},
	})
	writeSSEErrorFrame(c, payload)
}

// writeSSETerminalErrorBody 把 cell 回的错误 JSON body 原样(压成单行)作为 event: error
// 的 data 收尾;body 不是合法 JSON 时退化为 upstream_error + 截断文案。cell 的错误体本就是
// 客户端协议的错误对象(Anthropic {"type":"error","error":{…}} / OpenAI {"error":{…}}),
// 放进 event: error 后 Claude Code / codex 都按流内错误处理。
func writeSSETerminalErrorBody(c *gin.Context, body []byte) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, bytes.TrimSpace(body)); err != nil || compact.Len() == 0 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" {
			msg = "upstream request failed"
		}
		writeSSETerminalErrorMessage(c, msg)
		return
	}
	writeSSEErrorFrame(c, compact.Bytes())
}

func writeSSEErrorFrame(c *gin.Context, data []byte) {
	frame := "event: error\ndata: " + string(data) + "\n\n"
	if _, err := c.Writer.Write([]byte(frame)); err != nil {
		return
	}
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

// doCellRequestWithEarlyPing 执行 client.Do,同时在流式请求等 cell 响应头超过 ep.after
// 时先给客户端写 200 + SSE 头并按 ep.interval 发注释帧心跳,直到 cell 回头。
// *started 置 true 表示已向客户端写出响应头(调用方据此禁止转移、改用 SSE 错误帧收尾)。
// 心跳写失败(客户端已断)→ 取消对 cell 的请求并返回 context.Canceled。
func doCellRequestWithEarlyPing(c *gin.Context, client *http.Client, req *http.Request, ep edgeEarlyPing, isStream bool, started *bool) (*http.Response, error) {
	if !isStream || ep.after <= 0 {
		return client.Do(req)
	}
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		done <- result{resp, err}
	}()

	firstTimer := time.NewTimer(ep.after)
	defer firstTimer.Stop()
	var ticker *time.Ticker
	var tickC <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	interval := ep.interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	writePing := func() bool {
		if _, err := c.Writer.Write([]byte(": ping\n\n")); err != nil {
			return false
		}
		if f, ok := c.Writer.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}
	for {
		select {
		case r := <-done:
			// 心跳期间 cell 回来了:cancel 留给调用方 resp.Body 关闭后再无意义,但绝不能
			// 在这里 cancel(会截断正在透传的 body),交给 req.Context 的父 ctx 自然收尾。
			_ = cancel
			return r.resp, r.err
		case <-firstTimer.C:
			h := c.Writer.Header()
			h.Set("Content-Type", "text/event-stream")
			h.Set("Cache-Control", "no-cache")
			h.Set("Connection", "keep-alive")
			h.Set("X-Accel-Buffering", "no")
			c.Writer.WriteHeader(http.StatusOK)
			*started = true
			slog.Info("edge_forward: 等 cell 响应超过阈值,提前回 SSE 头并开始心跳",
				"cell", req.URL.Host, "path", c.Request.URL.Path, "after", ep.after.String())
			if !writePing() {
				cancel()
				r := <-done
				if r.resp != nil {
					_ = r.resp.Body.Close()
				}
				return nil, context.Canceled
			}
			ticker = time.NewTicker(interval)
			tickC = ticker.C
		case <-tickC:
			if !writePing() {
				cancel()
				r := <-done
				if r.resp != nil {
					_ = r.resp.Body.Close()
				}
				return nil, context.Canceled
			}
		}
	}
}

// proxyWebSocket 把客户端 WS 双向代理到 cell 的 WS 端点。
// 接受客户端升级 → 拨号 cell(http→ws / https→wss,同 path/query,鉴权换 cell key)
// → 两个方向逐帧转发,任一侧结束即收尾。读上限置 -1(不限),避免大消息被截断。
func proxyWebSocket(c *gin.Context, target *url.URL, forwardKey string) {
	clientConn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{
		CompressionMode: coderws.CompressionContextTakeover,
	})
	if err != nil {
		slog.Warn("edge_forward: 接受客户端 WS 失败", "err", err)
		return
	}
	defer func() { _ = clientConn.CloseNow() }()
	clientConn.SetReadLimit(-1)

	wsURL := *target
	if strings.EqualFold(target.Scheme, "https") {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	wsURL.Path = c.Request.URL.Path
	wsURL.RawQuery = c.Request.URL.RawQuery

	hdr := http.Header{}
	if forwardKey != "" {
		hdr.Set("Authorization", "Bearer "+forwardKey)
	}
	ctx := c.Request.Context()
	cellConn, _, err := coderws.Dial(ctx, wsURL.String(), &coderws.DialOptions{HTTPHeader: hdr})
	if err != nil {
		slog.Error("edge_forward: 拨号 cell WS 失败", "cell", target.Host, "path", wsURL.Path, "err", err)
		_ = clientConn.Close(coderws.StatusTryAgainLater, "edge cell unreachable")
		return
	}
	defer func() { _ = cellConn.CloseNow() }()
	cellConn.SetReadLimit(-1)

	errc := make(chan error, 2)
	go pumpWS(ctx, clientConn, cellConn, errc) // client → cell
	go pumpWS(ctx, cellConn, clientConn, errc) // cell → client
	<-errc
	// 任一侧结束:正常关闭两端(另一 goroutine 的 Read 会因 conn 关闭而解除阻塞)。
	_ = clientConn.Close(coderws.StatusNormalClosure, "")
	_ = cellConn.Close(coderws.StatusNormalClosure, "")
}

func pumpWS(ctx context.Context, src, dst *coderws.Conn, errc chan error) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			errc <- err
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			errc <- err
			return
		}
	}
}

// edgeModelPlatformMismatch 判断请求模型所属平台与分组平台是否冲突;冲突时返回给客户端的
// 说明文案,否则返回 ""。只比较 anthropic / openai 两侧都能由前缀判定的情况。
func edgeModelPlatformMismatch(reqModel string, group *service.Group) string {
	if group == nil || reqModel == "" {
		return ""
	}
	groupPlatform := strings.TrimSpace(group.Platform)
	modelPlatform := service.PlatformForModelName(reqModel)
	if groupPlatform == "" || modelPlatform == "" || groupPlatform == modelPlatform {
		return ""
	}
	channel := strings.TrimSpace(group.Name)
	if channel == "" {
		channel = strings.TrimSpace(group.Slug)
	}
	return fmt.Sprintf(
		"Model '%s' belongs to platform '%s', but this request went to channel '%s' (platform '%s'). "+
			"Select the matching channel by adding its slug prefix to the base URL: https://<host>/<channel-slug>/v1/...",
		reqModel, modelPlatform, channel, groupPlatform,
	)
}

// writeEdgeInvalidRequest 按分组平台的协议形状写 400 invalid_request_error。
func writeEdgeInvalidRequest(c *gin.Context, groupPlatform, message string) {
	c.Abort()
	if groupPlatform == service.PlatformOpenAI {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"type": "invalid_request_error", "message": message},
		})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{
		"type":  "error",
		"error": gin.H{"type": "invalid_request_error", "message": message},
	})
}

// modelNotAllowedBody 生成白名单拒绝的错误体:带客户端实际请求的模型与 URL。
// 例:model "gpt-5.6-luna" is not allowed on this channel (POST https://api.eirouter.ai/openai/v1/responses)
func modelNotAllowedBody(model, method, reqURL string) string {
	var msg string
	if model == "" {
		msg = "model not allowed: request has no \"model\" field"
	} else {
		msg = "model " + strconv.Quote(model) + " is not allowed on this channel"
	}
	if reqURL != "" {
		msg += " (" + method + " " + reqURL + ")"
	}
	payload, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "permission_error", "message": msg},
	})
	return string(payload)
}

func writeEdgeError(c *gin.Context) {
	c.Abort()
	c.Header("Content-Type", "application/json")
	c.String(http.StatusBadGateway, `{"type":"error","error":{"type":"upstream_error","message":"edge cell unreachable"}}`)
}

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func isHopByHopHeader(k string) bool {
	_, ok := hopByHopHeaders[strings.ToLower(k)]
	return ok
}

func copyProxyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, tok := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}
