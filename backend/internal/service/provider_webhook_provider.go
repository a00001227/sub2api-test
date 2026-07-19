package service

import (
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
)

// Phase 21E-6C-2D-1: wire provider — 从配置构造 Provider webhook 通知器。
// 配置未启用（URL/secret 缺失）时返回一个禁用的 notifier（Enabled()=false，
// 完成流程据此跳过通知）。绝不因未配置而报错。
func ProvideProviderWebhookNotifier(cfg *config.Config) ProviderWebhookNotifier {
	sender := providerwebhook.NewSender(providerwebhook.Config{
		URL:    cfg.ProviderConnect.WebhookURL,
		Secret: cfg.ProviderConnect.WebhookSecret,
	}, slog.Default())
	return NewProviderWebhookNotifier(sender)
}
