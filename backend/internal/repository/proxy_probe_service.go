package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func NewProxyExitInfoProber(cfg *config.Config) service.ProxyExitInfoProber {
	insecure := false
	allowPrivate := false
	validateResolvedIP := true
	maxResponseBytes := defaultProxyProbeResponseMaxBytes
	if cfg != nil {
		insecure = cfg.Security.ProxyProbe.InsecureSkipVerify
		allowPrivate = cfg.Security.URLAllowlist.AllowPrivateHosts
		validateResolvedIP = cfg.Security.URLAllowlist.Enabled
		if cfg.Gateway.ProxyProbeResponseReadMaxBytes > 0 {
			maxResponseBytes = cfg.Gateway.ProxyProbeResponseReadMaxBytes
		}
	}
	if insecure {
		log.Printf("[ProxyProbe] Warning: insecure_skip_verify is not allowed and will cause probe failure.")
	}
	return &proxyProbeService{
		insecureSkipVerify: insecure,
		allowPrivateHosts:  allowPrivate,
		validateResolvedIP: validateResolvedIP,
		maxResponseBytes:   maxResponseBytes,
	}
}

const (
	defaultProxyProbeTimeout          = 10 * time.Second
	defaultProxyProbeResponseMaxBytes = int64(1024 * 1024)
)

// probeURLs 按优先级排列的探测 URL 列表（英文，city 作为 region 匹配键）。
// 某些 AI API 专用代理只允许访问特定域名，因此需要多个备选。
var probeURLs = []struct {
	url    string
	parser string // "ip-api" or "httpbin"
}{
	{"http://ip-api.com/json/?lang=en", "ip-api"},
	{"http://httpbin.org/ip", "httpbin"},
}

type proxyProbeService struct {
	insecureSkipVerify bool
	allowPrivateHosts  bool
	validateResolvedIP bool
	maxResponseBytes   int64
}

func (s *proxyProbeService) ProbeProxy(ctx context.Context, proxyURL string) (*service.ProxyExitInfo, int64, error) {
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:           proxyURL,
		Timeout:            defaultProxyProbeTimeout,
		InsecureSkipVerify: s.insecureSkipVerify,
		ValidateResolvedIP: s.validateResolvedIP,
		AllowPrivateHosts:  s.allowPrivateHosts,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create proxy client: %w", err)
	}

	// 逐个探测站尝试:任一解析成功即返回地理信息。全部失败时,区分两类结局:
	//   - 「可达」:经代理拿到了任意 HTTP 响应(哪怕 429/503/无法解析)→ 出口能出网,
	//     只是地理站没给可用数据 → 返回 *service.ProxyReachableError(判活方视为通)。
	//   - 「出不了网」:所有探测站在传输层就失败(经代理建连失败/超时)→ 才算出口真死。
	var (
		anyReachable bool
		reachLatency int64
		reachReason  string
		deadReason   string
	)
	for _, probe := range probeURLs {
		exitInfo, latencyMs, reachable, err := s.probeWithURL(ctx, client, probe.url, probe.parser)
		if err == nil {
			return exitInfo, latencyMs, nil
		}
		if reachable {
			// err 由响应/解析构造,内容安全(不含代理 URL/凭证),可直接用作原因。
			anyReachable = true
			reachLatency = latencyMs
			reachReason = fmt.Sprintf("%s %s", probe.parser, err.Error())
		} else {
			// 传输层错误可能包含代理地址,绝不回显 —— 只归类,不带原始 message。
			deadReason = fmt.Sprintf("%s %s", probe.parser, transportReason(err))
		}
	}

	if anyReachable {
		return nil, reachLatency, &service.ProxyReachableError{Reason: reachReason}
	}
	return nil, 0, fmt.Errorf("proxy unreachable: %s", deadReason)
}

// transportReason 把「经代理建连失败」的原始错误归类成一句脱敏短语,绝不回显原始
// message(它常含代理 IP:端口)。
func transportReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "connect failed"
	}
}

// probeWithURL 探一个 URL。reachable=true 表示经代理拿到了 HTTP 响应(出口能出网),
// 即使随后状态码非 200 或响应无法解析;reachable=false 仅当传输层(经代理建连)失败。
func (s *proxyProbeService) probeWithURL(ctx context.Context, client *http.Client, url string, parser string) (info *service.ProxyExitInfo, latencyMs int64, reachable bool, err error) {
	startTime := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		// 经代理建连/传输失败 —— 出口没出网。
		return nil, 0, false, fmt.Errorf("proxy connection failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	latencyMs = time.Since(startTime).Milliseconds()

	// 到这里已拿到 HTTP 响应 → 出口确实出网了(reachable=true),后续任何失败都只是
	// 「探测站没给可用数据」,不代表代理死。
	if resp.StatusCode != http.StatusOK {
		return nil, latencyMs, true, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	maxResponseBytes := s.maxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultProxyProbeResponseMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, latencyMs, true, fmt.Errorf("read response failed: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, latencyMs, true, fmt.Errorf("response exceeds limit: %d", maxResponseBytes)
	}

	// 到这里 reachable 恒为 true(已拿到 200 响应体),解析失败也只是地理数据不可用。
	switch parser {
	case "ip-api":
		info, lat, perr := s.parseIPAPI(body, latencyMs)
		return info, lat, true, perr
	case "httpbin":
		info, lat, perr := s.parseHTTPBin(body, latencyMs)
		return info, lat, true, perr
	default:
		return nil, latencyMs, true, fmt.Errorf("unknown parser: %s", parser)
	}
}

func (s *proxyProbeService) parseIPAPI(body []byte, latencyMs int64) (*service.ProxyExitInfo, int64, error) {
	var ipInfo struct {
		Status      string `json:"status"`
		Message     string `json:"message"`
		Query       string `json:"query"`
		City        string `json:"city"`
		Region      string `json:"region"`
		RegionName  string `json:"regionName"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
	}

	if err := json.Unmarshal(body, &ipInfo); err != nil {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, latencyMs, fmt.Errorf("failed to parse response: %w (body: %s)", err, preview)
	}
	if strings.ToLower(ipInfo.Status) != "success" {
		if ipInfo.Message == "" {
			ipInfo.Message = "ip-api request failed"
		}
		return nil, latencyMs, fmt.Errorf("ip-api request failed: %s", ipInfo.Message)
	}

	region := ipInfo.RegionName
	if region == "" {
		region = ipInfo.Region
	}
	return &service.ProxyExitInfo{
		IP:          ipInfo.Query,
		City:        ipInfo.City,
		Region:      region,
		Country:     ipInfo.Country,
		CountryCode: ipInfo.CountryCode,
	}, latencyMs, nil
}

func (s *proxyProbeService) parseHTTPBin(body []byte, latencyMs int64) (*service.ProxyExitInfo, int64, error) {
	// httpbin.org/ip 返回格式: {"origin": "1.2.3.4"}
	var result struct {
		Origin string `json:"origin"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, latencyMs, fmt.Errorf("failed to parse httpbin response: %w", err)
	}
	if result.Origin == "" {
		return nil, latencyMs, fmt.Errorf("httpbin: no IP found in response")
	}
	return &service.ProxyExitInfo{
		IP: result.Origin,
	}, latencyMs, nil
}
