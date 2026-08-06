package middleware

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

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
func EdgeForward(cfg config.EdgeForwardConfig) gin.HandlerFunc {
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
	return newEdgeForwardHandler(resolver, groupSet, strings.TrimSpace(cfg.Key), rand.Float64)
}

// newEdgeForwardHandler 构造转发处理函数(组命中→加权随机选序→WS/失败转移流式回传)。
// 与配置解析分离,便于用注入的 resolver + 确定性 rng 测试选路/失败转移。
func newEdgeForwardHandler(resolver cellResolver, groupSet map[string]struct{}, forwardKey string, rng func() float64) gin.HandlerFunc {
	// 流式:不设 Client.Timeout(否则会截断长 SSE);客户端断开由请求 Context 取消传导。
	client := &http.Client{Transport: http.DefaultTransport}
	// 会话→cell 亲和(P3-3c),处理函数生命周期内共享。
	affinity := newSessionAffinity(stickyAffinityTTL)

	return func(c *gin.Context) {
		apiKey, ok := GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || apiKey.Group == nil {
			c.Next()
			return
		}
		if _, hit := groupSet[apiKey.Group.Slug]; !hit {
			c.Next()
			return
		}

		// 加权随机选序(P3-3b):按信誉分对存活池加权随机排序,首个 = 加权首选,其余
		// 顺位作失败转移候选;静态兜底 append 到最后(仅动态池全空/全挂时才会用到)。
		order := weightedOrder(resolver.poolCandidates(), rng)
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
		if len(order) == 0 {
			slog.Error("edge_forward: 无可路由 cell", "path", c.Request.URL.Path)
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
			// 成功落到某 cell → 绑定会话亲和(下一轮同会话回到这台 cell)。
			if stickyKey != "" {
				affinity.put(stickyKey, target)
			}
			streamCellResponse(c, resp)
			c.Abort()
			return
		}
		slog.Error("edge_forward: 所有候选 cell 均不可达", "candidates", len(order), "err", lastErr)
		writeEdgeError(c)
	}
}

// streamCellResponse 回写 cell 响应头+状态码,再逐块流式回传(SSE 每块 flush)。
// 一旦调用即已选定某个 cell,不再失败转移(见 EdgeForward 循环内注释)。
func streamCellResponse(c *gin.Context, resp *http.Response) {
	defer resp.Body.Close()
	h := c.Writer.Header()
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := c.Writer.Write(buf[:n]); werr != nil {
				break // 客户端断开
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
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
