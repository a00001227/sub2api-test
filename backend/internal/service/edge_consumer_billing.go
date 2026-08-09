package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

/*
中央「执行→转发」消费者计费(#86b)

中央转发到 cell 后,消费者要在中央计费,但账号/执行都在 cell —— 中央没有本地 account。
本文件用「占位 account + 复用现成 RecordUsage」把这件事做成**零 money 代码改动**:

  - 占位 account:一个 external_provider_account_id 为空、schedulable=false 的账号行。
    · usage_logs.account_id 是 Required FK → 占位行满足;
    · provider outbox 那步按 account_id 查 external ref,占位号为空 → 天然跳过(不重复
      发 provider 用量,那是 cell 的职责);
    · schedulable=false → 中央永不把它当号选中服务。
  - 计费:直接调现成 RecordUsage(SkipConsumerBilling=false),消费者照常扣费。

usage 由 cell 权威带回(见 edge_usage.go 的 EdgeUsageEnvelope),中央不自己解析 SSE。
*/

// forwardPlaceholderExtraKey 是占位 account 的 extra 标记键(用于 find-or-create)。
const forwardPlaceholderExtraKey = "edge_forward_placeholder"
const forwardPlaceholderName = "__edge_forward_placeholder__"

// ForwardedConsumerUsageInput 中央转发消费者计费入参。usage 来自 cell 的权威 envelope。
type ForwardedConsumerUsageInput struct {
	Env              EdgeUsageEnvelope
	APIKey           *APIKey
	User             *User
	Subscription     *UserSubscription
	QuotaPlatform    string
	InboundEndpoint  string
	UpstreamEndpoint string
	UserAgent        string
	IPAddress        string
	APIKeyService    APIKeyQuotaUpdater
}

// EnsureForwardPlaceholderAccount 懒建/加载中央转发计费用的占位 account(缓存)。
func (s *GatewayService) EnsureForwardPlaceholderAccount(ctx context.Context) (*Account, error) {
	s.forwardPlaceholderMu.Lock()
	defer s.forwardPlaceholderMu.Unlock()
	if s.forwardPlaceholderAcc != nil {
		return s.forwardPlaceholderAcc, nil
	}
	// 先按 extra 标记找现有的(避免重启后重复建)。
	if found, err := s.accountRepo.FindByExtraField(ctx, forwardPlaceholderExtraKey, "1"); err == nil && len(found) > 0 {
		acc := found[0]
		s.forwardPlaceholderAcc = &acc
		return s.forwardPlaceholderAcc, nil
	}
	// 建一个:external ref 空、schedulable=false、非 active,永不被选号。
	acc := &Account{
		Name:        forwardPlaceholderName,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Status:      StatusDisabled,
		Schedulable: false,
		Credentials: map[string]any{},
		Extra:       map[string]any{forwardPlaceholderExtraKey: "1"},
	}
	if err := s.accountRepo.Create(ctx, acc); err != nil {
		return nil, err
	}
	s.forwardPlaceholderAcc = acc
	return s.forwardPlaceholderAcc, nil
}

// RecordForwardedConsumerUsage 用 cell 带回的权威 usage 给消费者计费。复用现成
// RecordUsage(占位 account + SkipConsumerBilling=false):消费者扣费照常,provider
// outbox 因占位号无 external ref 天然跳过。best-effort 由调用方决定;这里返回错误。
func (s *GatewayService) RecordForwardedConsumerUsage(ctx context.Context, in *ForwardedConsumerUsageInput) error {
	if in == nil || in.APIKey == nil || in.User == nil {
		return nil
	}
	placeholder, err := s.EnsureForwardPlaceholderAccount(ctx)
	if err != nil {
		logger.LegacyPrintf("service.edge_consumer_billing", "[EdgeForwardBilling] ensure placeholder failed: %v", err)
		return err
	}
	return s.RecordUsage(ctx, &RecordUsageInput{
		Result:              in.Env.ToForwardResult(),
		QuotaPlatform:       in.QuotaPlatform,
		APIKey:              in.APIKey,
		User:                in.User,
		Account:             placeholder,
		Subscription:        in.Subscription,
		InboundEndpoint:     in.InboundEndpoint,
		UpstreamEndpoint:    in.UpstreamEndpoint,
		UserAgent:           in.UserAgent,
		IPAddress:           in.IPAddress,
		APIKeyService:       in.APIKeyService,
		SkipConsumerBilling: false, // 中央要给消费者计费(与 cell 侧 edge-trusted 相反)
	})
}
