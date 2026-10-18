package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// 渠道商导入 Azure OpenAI key。凭证串一行一把:`<endpoint>|<api-key>`,endpoint 形如
// https://<资源名>.openai.azure.com(也接受直接给 …/openai/v1)。建成 OpenAI 平台的 API Key 账号,
// base_url = endpoint + /openai/v1(Azure 的 OpenAI 兼容 v1 面,Bearer key),直连 Azure 不走代理。
// Azure 的 model 是「部署名」:渠道商把部署命名成标准模型名即可,不一致的在账号配置里写重映射。

var (
	azureEndpointHostRe = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9-]{1,62}\.(openai\.azure\.com|cognitiveservices\.azure\.com|services\.ai\.azure\.com)$`)
	azureCredSep        = regexp.MustCompile(`[|,\s]+`)
)

// parseAzureImportCredential 拆出 base_url(…/openai/v1)与 key;格式不对返回可直接展示的原因(不含 key)。
func parseAzureImportCredential(raw string) (baseURL, apiKey string, err error) {
	parts := azureCredSep.Split(strings.TrimSpace(raw), -1)
	fields := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			fields = append(fields, p)
		}
	}
	if len(fields) != 2 {
		return "", "", errors.New("expected `<https://RESOURCE.openai.azure.com>|<api-key>` on one line")
	}
	endpoint, key := fields[0], fields[1]
	// 两段顺序写反也认:key 不会以 http 开头。
	if !strings.HasPrefix(strings.ToLower(endpoint), "http") && strings.HasPrefix(strings.ToLower(key), "http") {
		endpoint, key = key, endpoint
	}
	u, perr := url.Parse(endpoint)
	if perr != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", errors.New("azure endpoint must be an https URL like https://RESOURCE.openai.azure.com")
	}
	if !azureEndpointHostRe.MatchString(u.Hostname()) {
		return "", "", fmt.Errorf("azure endpoint host %q is not an Azure OpenAI resource (*.openai.azure.com / *.cognitiveservices.azure.com / *.services.ai.azure.com)", u.Hostname())
	}
	if len(key) < 16 || strings.ContainsAny(key, " \t") {
		return "", "", errors.New("azure api-key looks invalid (copy it from Azure Portal → the resource → Keys and Endpoint)")
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case path == "" || path == "/openai":
		path = "/openai/v1"
	case path == "/openai/v1":
	default:
		return "", "", fmt.Errorf("azure endpoint path %q not supported: give the resource root (https://RESOURCE.openai.azure.com), the /openai/v1 suffix is added automatically", u.Path)
	}
	u.Path, u.RawQuery, u.Fragment = path, "", ""
	return u.String(), key, nil
}

// azureCredentialProber 建号前真调一次 Azure,确认 endpoint 存在、key 有效。
type azureCredentialProber interface {
	ProbeAzureCredential(ctx context.Context, baseURL, apiKey string) error
}

type liveAzureCredentialProber struct{}

// ProbeAzureCredential:先 GET /models(v1 面有此端点);401/403 = key 错;404 时退一步 POST 一个不存在的
// 部署名到 /responses —— 仍 401/403 = key 错,404 DeploymentNotFound = endpoint 与 key 都对(只是没这个部署)。
func (liveAzureCredentialProber) ProbeAzureCredential(ctx context.Context, baseURL, apiKey string) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	client, err := httpclient.GetClient(httpclient.Options{Timeout: 25 * time.Second, ValidateResolvedIP: true})
	if err != nil {
		return err
	}
	do := func(method, path string, body []byte) (int, string, error) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, rd)
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("api-key", apiKey) // 旧面只认这个头;v1 面两个都认
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", fmt.Errorf("request failed: %s", sanitizeUpstreamErrorMessage(err.Error()))
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, azureErrorMessage(raw), nil
	}

	status, msg, err := do(http.MethodGet, "/models", nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("Azure %d: api-key rejected (wrong key, wrong resource, or key disabled): %s", status, msg)
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("Azure %d: %s", status, msg)
	}
	body, _ := json.Marshal(map[string]any{"model": "sub2api-probe-no-such-deployment", "input": "ping", "max_output_tokens": 1})
	status, msg, err = do(http.MethodPost, "/responses", body)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("Azure %d: api-key rejected (wrong key, wrong resource, or key disabled): %s", status, msg)
	case status == http.StatusNotFound && strings.Contains(strings.ToLower(msg), "deployment"):
		return nil // 认证通过,只是探活用的部署名不存在 —— 正是我们要的信号
	case status == http.StatusNotFound:
		return fmt.Errorf("Azure 404: endpoint has no OpenAI v1 surface (check the resource URL): %s", msg)
	case status >= 200 && status < 300, status == http.StatusBadRequest, status == http.StatusTooManyRequests:
		return nil
	default:
		return fmt.Errorf("Azure %d: %s", status, msg)
	}
}

func azureErrorMessage(raw []byte) string {
	var v struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &v) == nil {
		if v.Error.Message != "" {
			if v.Error.Code != "" {
				return v.Error.Code + ": " + v.Error.Message
			}
			return v.Error.Message
		}
		if v.Message != "" {
			return v.Message
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
