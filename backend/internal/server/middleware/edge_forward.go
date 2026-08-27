package middleware

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
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
func EdgeForward(cfg config.EdgeForwardConfig, biller EdgeConsumerBiller, modelAllowed ModelAllowFunc, capture EvidenceCaptureFunc) gin.HandlerFunc {
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

	return newEdgeForwardHandler(resolver, groupSet, groupLanes, strings.TrimSpace(cfg.Key), rand.Float64, biller, checker, capture)
}

// newEdgeForwardHandler 构造转发处理函数(组命中→加权随机选序→WS/失败转移流式回传)。
// 与配置解析分离,便于用注入的 resolver + 确定性 rng 测试选路/失败转移。
func newEdgeForwardHandler(resolver cellResolver, groupSet map[string]struct{}, groupLanes map[string]string, forwardKey string, rng func() float64, biller EdgeConsumerBiller, modelAllowed ModelAllowFunc, capture EvidenceCaptureFunc) gin.HandlerFunc {
	// 流式:不设 Client.Timeout(否则会截断长 SSE);客户端断开由请求 Context 取消传导。
	client := &http.Client{Transport: http.DefaultTransport}
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
		// 消费者工作道(护号):优先读组自身的 lane(后台可视化、免重启、近实时生效);
		// 为 normal/空时再退回 env 映射 EDGE_FORWARD_GROUP_LANES(过渡期兼容,迁完可删)。
		consumerLane := normalizeLane(apiKey.Group.Lane)
		if consumerLane == laneNormal {
			if l, ok := groupLanes[apiKey.Group.Slug]; ok {
				consumerLane = l // 构造期已归一
			}
		}
		slog.Debug("edge_forward: 命中,转发到 cell", "path", c.Request.URL.Path, "key_group_slug", apiKey.Group.Slug, "lane", consumerLane)

		// 严格隔离:先按工作道过滤候选,再做加权随机 —— 使信誉权重只在同道内比较,
		// 且非 normal 消费者绝不落到别道 cell。加权随机选序(P3-3b):首个 = 加权首选,
		// 其余顺位作失败转移候选。
		order := weightedOrder(filterByLane(resolver.poolCandidates(), consumerLane), rng)
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
				slog.Info("edge_forward: 模型不在白名单,拒绝转发", "model", reqModel, "path", c.Request.URL.Path)
				c.Header("Content-Type", "application/json")
				c.String(http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"model not allowed"}}`)
				c.Abort()
				return
			}
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

			resp, err := client.Do(outReq)
			if err != nil {
				// 仅“写任何响应前”的传输错误才转移;一旦开始回传就不再转移
				// (避免把非幂等请求重放到第二个号 → 双执行)。
				lastErr = err
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
				if isCellNoAvailableAccounts(peek) {
					lastErr = errCellNoAvailableAccounts
					slog.Warn("edge_forward: cell 无可用账号(选号前失败),尝试下一候选",
						"cell", target.Host, "idx", i)
					continue
				}
				// 其它 503(cell/上游真实错误)→ 原样回传已缓冲的响应,不转移。
				relayBufferedResponse(c, resp, peek)
				c.Abort()
				return
			}
			// 成功落到某 cell → 绑定会话亲和(下一轮同会话回到这台 cell)。
			if stickyKey != "" {
				affinity.put(stickyKey, target)
			}
			env := streamCellResponse(c, resp)
			// #86b:cell 带回权威用量 → 给消费者计费(占位号,不重复发 provider 用量)。
			// reqBody/reqStart 顺带传给 biller 供 Risk V2 影子采集(不参与计费)。
			if env != nil && biller != nil {
				biller(c, *env, body, reqStart)
			}
			c.Abort()
			return
		}
		slog.Error("edge_forward: 所有候选 cell 均不可达", "candidates", len(order), "err", lastErr)
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

// relayBufferedResponse writes a fully-buffered cell response (headers + status
// + body) to the client. Used for a non-failover 503 whose body was already read
// to classify it — mirrors streamCellResponse's header filtering.
func relayBufferedResponse(c *gin.Context, resp *http.Response, body []byte) {
	h := c.Writer.Header()
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) || strings.EqualFold(k, service.EdgeUsageHeader) {
			continue
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
// 返回捕获到的 envelope(没有则 nil)。
func streamCellResponse(c *gin.Context, resp *http.Response) *service.EdgeUsageEnvelope {
	defer resp.Body.Close()
	h := c.Writer.Header()
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) || strings.EqualFold(k, service.EdgeUsageHeader) {
			continue // 用量头不透传给消费者客户端
		}
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	flusher, _ := c.Writer.(http.Flusher)

	var captured *service.EdgeUsageEnvelope
	// 非流式:用量在响应头(cell 后续会加;body 原样拷)。
	if hv := resp.Header.Get(service.EdgeUsageHeader); hv != "" {
		if env, err := service.ParseEdgeUsageEnvelope([]byte(hv)); err == nil {
			captured = &env
		}
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		// 非 SSE:原样拷贝。
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
		return captured
	}

	// SSE:逐行转发;识别并剥掉 sub2api_usage 事件、捕获其 data。ReadString 对任意长
	// 度的 data 行安全(bufio.Scanner 有 token 上限)。
	reader := bufio.NewReader(resp.Body)
	inSentinel := false
	for {
		line, rerr := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "event: "+service.EdgeUsageEventName:
				inSentinel = true // 该事件所有行都丢弃,不透传
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
				if _, werr := c.Writer.Write([]byte(line)); werr != nil {
					return captured // 客户端断开
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return captured
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
