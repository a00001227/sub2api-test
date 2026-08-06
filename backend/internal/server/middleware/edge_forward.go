package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// EdgeForward 是中央网关“执行→转发”中间件（P1 walking skeleton）。
//
// 当 EdgeForward 开启、且当前请求解析出的组 slug 命中配置列表时,把该 /v1 请求
// 手动流式反向代理到边缘 cell（不带中央凭据,cell 用它本地的号执行）,并把 cell 的
// 响应(含 SSE 流,逐块 flush)原样回传给客户端;否则放行,走中央本地选号执行(与
// 今天一致)。
//
// 默认关(Enabled=false / CellURL 空 / Groups 空)= 完全 no-op。WebSocket 升级
// 请求本 skeleton 不转发(放行走本地),留待后续。
//
// 用手写流式代理而非 httputil.ReverseProxy:后者会触碰 gin 的 CloseNotify(在
// 某些 ResponseWriter / h2c 下会 panic),且手写更利于逐块 flush SSE。
func EdgeForward(cfg config.EdgeForwardConfig) gin.HandlerFunc {
	if !cfg.Enabled || strings.TrimSpace(cfg.CellURL) == "" || len(cfg.Groups) == 0 {
		return func(c *gin.Context) { c.Next() }
	}
	target, err := url.Parse(strings.TrimSpace(cfg.CellURL))
	if err != nil || target.Scheme == "" || target.Host == "" {
		slog.Error("edge_forward: 无效的 cell_url,已禁用转发", "cell_url", cfg.CellURL, "err", err)
		return func(c *gin.Context) { c.Next() }
	}

	groupSet := make(map[string]struct{}, len(cfg.Groups))
	for _, g := range cfg.Groups {
		if s := strings.TrimSpace(g); s != "" {
			groupSet[s] = struct{}{}
		}
	}
	forwardKey := strings.TrimSpace(cfg.Key)

	// 流式:不设 Client.Timeout(否则会截断长 SSE);客户端断开由请求 Context 取消传导。
	client := &http.Client{Transport: http.DefaultTransport}

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
		if isWebSocketUpgrade(c.Request) {
			proxyWebSocket(c, target, forwardKey)
			c.Abort()
			return
		}

		// 构造到 cell 的出站请求:同 method/path/query/body,鉴权换成 cell key。
		outURL := *target
		outURL.Path = c.Request.URL.Path
		outURL.RawQuery = c.Request.URL.RawQuery
		outReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, outURL.String(), c.Request.Body)
		if err != nil {
			writeEdgeError(c)
			return
		}
		copyProxyHeaders(outReq.Header, c.Request.Header)
		outReq.ContentLength = c.Request.ContentLength
		if forwardKey != "" {
			outReq.Header.Set("Authorization", "Bearer "+forwardKey)
			outReq.Header.Del("X-Api-Key")
			outReq.Header.Del("x-api-key")
		}

		resp, err := client.Do(outReq)
		if err != nil {
			slog.Error("edge_forward: 转发到 cell 失败", "cell", target.Host, "path", c.Request.URL.Path, "err", err)
			writeEdgeError(c)
			return
		}
		defer resp.Body.Close()

		// 回写响应头 + 状态码,再逐块流式回传(SSE 每块 flush)。
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
		c.Abort()
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
