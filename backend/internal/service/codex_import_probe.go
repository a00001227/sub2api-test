package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	openaipkg "github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// codexCredentialProber 导入 Codex 凭据时的在线校验:拿 access_token 经指定出口打一次 Codex 后端。
type codexCredentialProber interface {
	ProbeCodexCredential(ctx context.Context, accessToken, chatgptAccountID, proxyURL string) error
}

// liveCodexCredentialProber 真实实现:与 AccountUsageService.probeOpenAICodexSnapshot 同一请求形态
// (同 URL、同 CLI 身份头、同最小 payload),只是不落快照、不需要账号对象。
type liveCodexCredentialProber struct{}

const codexImportProbeTimeout = 15 * time.Second

func (liveCodexCredentialProber) ProbeCodexCredential(ctx context.Context, accessToken, chatgptAccountID, proxyURL string) error {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return fmt.Errorf("empty access_token")
	}
	payloadBytes, err := json.Marshal(createOpenAITestPayload(openaipkg.DefaultTestModel, true))
	if err != nil {
		return fmt.Errorf("marshal probe payload: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, codexImportProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, chatgptCodexURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("create probe request: %w", err)
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("Version", openAICodexProbeVersion)
	req.Header.Set("User-Agent", codexCLIUserAgent)
	if id := strings.TrimSpace(chatgptAccountID); id != "" {
		req.Header.Set("chatgpt-account-id", id)
	}
	client, err := httppool.GetClient(httppool.Options{
		ProxyURL:              proxyURL,
		Timeout:               codexImportProbeTimeout,
		ResponseHeaderTimeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("build probe client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe request failed before any upstream response (network/proxy): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		// 上游认得这个 token,只是额度/频率受限 —— 凭据本身有效,放行建号,跑量时按正常限流处理。
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	md := map[string]string{}
	return fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, summarizeUpstreamErrorBody(string(body), md))
}
