package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// 渠道商导入 AWS Bedrock API Key(长期 key 以 "ABSK" 开头;短期 key 以 "bedrock-api-key-" 开头)。
// 凭证串格式:`<api_key>` 或 `<api_key>|<aws_region>`(分隔符也接受 `@` / 空白 / 逗号),
// 缺 region 默认 us-east-1。Bedrock 直连 AWS,不走住宅代理,也没有 5h/7d 窗口。

const bedrockImportDefaultRegion = "us-east-1"

var (
	bedrockAPIKeyRe = regexp.MustCompile(`^(ABSK[A-Za-z0-9+/=_-]{20,}|bedrock-api-key-[A-Za-z0-9+/=_-]{20,})$`)
	awsRegionRe     = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-\d$`)
	bedrockCredSep  = regexp.MustCompile(`[|@,\s]+`)
)

// parseBedrockImportCredential 拆出 api_key 与 aws_region;格式不对返回可直接展示的原因(不含 key)。
func parseBedrockImportCredential(raw string) (apiKey, region string, err error) {
	parts := bedrockCredSep.Split(strings.TrimSpace(raw), -1)
	fields := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			fields = append(fields, p)
		}
	}
	if len(fields) == 0 {
		return "", "", errors.New("empty credential")
	}
	if len(fields) > 2 {
		return "", "", errors.New("expected `<bedrock api key>|<aws region>` on one line")
	}
	apiKey = fields[0]
	if !bedrockAPIKeyRe.MatchString(apiKey) {
		if strings.HasPrefix(apiKey, "AKIA") || strings.HasPrefix(apiKey, "ASIA") {
			return "", "", errors.New("this looks like an IAM access key (AKIA…), not a Bedrock API key; generate one at AWS Console → Bedrock → API keys")
		}
		return "", "", errors.New("bedrock api key format invalid: expected a long-term key starting with ABSK… (AWS Console → Bedrock → API keys)")
	}
	region = bedrockImportDefaultRegion
	if len(fields) == 2 {
		region = strings.ToLower(fields[1])
		if !awsRegionRe.MatchString(region) {
			return "", "", fmt.Errorf("aws region %q invalid: expected e.g. us-east-1 / us-east-2 / eu-west-1", fields[1])
		}
	}
	return apiKey, region, nil
}

// bedrockCredentialProber 建号前用这把 key 真调一次 Bedrock,确认 key 有效且模型访问已开通。
type bedrockCredentialProber interface {
	ProbeBedrockCredential(ctx context.Context, apiKey, region string) error
}

// bedrockProbeModel 探活用最便宜的模型:按 region 前缀自动换(us./eu./apac.…)。
const bedrockProbeModelAlias = "claude-haiku-4-5"

type liveBedrockCredentialProber struct{}

func (liveBedrockCredentialProber) ProbeBedrockCredential(ctx context.Context, apiKey, region string) error {
	modelID, ok := domain.DefaultBedrockModelMapping[bedrockProbeModelAlias]
	if !ok {
		return errors.New("probe model alias missing from DefaultBedrockModelMapping")
	}
	modelID = AdjustBedrockModelRegionPrefix(modelID, region)
	body, _ := json.Marshal(map[string]any{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens":        1,
		"messages":          []map[string]any{{"role": "user", "content": "ping"}},
	})
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BuildBedrockURL(region, modelID, false), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client, err := httpclient.GetClient(httpclient.Options{Timeout: 25 * time.Second, ValidateResolvedIP: true})
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %s", sanitizeUpstreamErrorMessage(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyBedrockProbeResponse(resp.StatusCode, resp.Body, modelID, region)
}

// classifyBedrockProbeResponse 把 Bedrock 的回答压成一句渠道商看得懂的话。
// 200 = 通;429(ThrottlingException)= key 有效只是忙,也算通;其余按状态码点名原因。
func classifyBedrockProbeResponse(status int, body io.Reader, modelID, region string) error {
	if status == http.StatusOK || status == http.StatusTooManyRequests {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(body, 4096))
	msg := strings.TrimSpace(string(raw))
	if m := extractBedrockErrorMessage(raw); m != "" {
		msg = m
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "model") && (strings.Contains(lower, "access") || strings.Contains(lower, "not authorized") || strings.Contains(lower, "don't have")) {
			return fmt.Errorf("AWS %d: key is valid but model access for %s is not enabled in %s (AWS Console → Bedrock → Model access): %s", status, modelID, region, msg)
		}
		return fmt.Errorf("AWS %d: Bedrock rejected the API key (expired / wrong account / missing AmazonBedrock permission): %s", status, msg)
	case http.StatusNotFound, http.StatusBadRequest:
		return fmt.Errorf("AWS %d: model %s not available in %s (enable it under Bedrock → Model access, or pick another region): %s", status, modelID, region, msg)
	default:
		return fmt.Errorf("AWS %d: %s", status, msg)
	}
}

func extractBedrockErrorMessage(raw []byte) string {
	var v struct {
		Message string `json:"message"`
		Msg     string `json:"Message"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	if v.Message != "" {
		return v.Message
	}
	return v.Msg
}
