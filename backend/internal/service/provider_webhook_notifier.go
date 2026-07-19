package service

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
)

// Phase 21E-6C-2D-1: 把 providerwebhook.Sender 适配为完成流程需要的
// ProviderWebhookNotifier。放在 service 包内以避免包循环
// （providerwebhook 不依赖 service）。
type providerWebhookNotifier struct {
	sender *providerwebhook.Sender
	now    func() time.Time
}

// NewProviderWebhookNotifier wraps a sender as the completion service's notifier.
func NewProviderWebhookNotifier(sender *providerwebhook.Sender) ProviderWebhookNotifier {
	return &providerWebhookNotifier{sender: sender, now: time.Now}
}

func (n *providerWebhookNotifier) Enabled() bool {
	return n.sender != nil && n.sender.Enabled()
}

func (n *providerWebhookNotifier) SendActivatedAsync(in ActivatedWebhookInput) {
	if n.sender == nil {
		return
	}
	// Stable event id: an explicit EventID (import flow) wins; otherwise fall
	// back to the connect session-derived id (OAuth flow, unchanged).
	eventID := in.EventID
	if eventID == "" {
		eventID = providerwebhook.ConnectActivatedEventID(in.SessionID)
	}
	ev := providerwebhook.BuildActivated(providerwebhook.ActivatedInput{
		EventID:                   eventID,
		CreatedAt:                 n.now().UTC().Format(time.RFC3339),
		ExternalProviderAccountID: in.ExternalProviderAccountID,
		Sub2apiAccountID:          strconv.FormatInt(in.Sub2apiAccountID, 10),
		ProviderType:              in.ProviderType,
		Platform:                  in.Platform,
		Region:                    in.Region,
		Email:                     in.Email,
	})
	n.sender.SendAsync(ev)
}
