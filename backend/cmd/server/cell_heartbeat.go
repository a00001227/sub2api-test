package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// startCellHeartbeat 在边缘 cell 模式下,周期性向 Portal 自注册 + 心跳(P3)。
// 仅当 EDGE_MODE 且 CELL_REGISTRY_URL / CELL_ADVERTISE_ADDR / CELL_REGION 配齐时
// 启动;否则静默跳过(保持手动登记模式)。鉴权复用 provider_connect.internal_token
// (= Portal 的 SUB2API_INTERNAL_TOKEN)。失败只 warn,不影响主流程。
func startCellHeartbeat(ctx context.Context, cfg *config.Config) {
	if cfg == nil || !cfg.EdgeMode {
		return
	}
	rc := cfg.CellRegistry
	url := strings.TrimSpace(rc.URL)
	advertise := strings.TrimSpace(rc.AdvertiseAddr)
	region := strings.ToLower(strings.TrimSpace(rc.Region))
	if url == "" || advertise == "" || region == "" {
		slog.Warn("cell heartbeat 未启用:需配齐 CELL_REGISTRY_URL / CELL_ADVERTISE_ADDR / CELL_REGION")
		return
	}
	token := strings.TrimSpace(cfg.ProviderConnect.InternalToken)
	interval := time.Duration(rc.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	payload, _ := json.Marshal(map[string]string{
		"region":  region,
		"baseUrl": advertise,
		"node":    strings.TrimSpace(rc.Node),
	})
	client := &http.Client{Timeout: 10 * time.Second}

	post := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("cell heartbeat 发送失败", "err", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			slog.Warn("cell heartbeat 非 2xx", "status", resp.StatusCode)
		}
	}

	go func() {
		post() // 启动即注册一次
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				post()
			}
		}
	}()
	slog.Info("cell heartbeat 已启动",
		"url", url, "advertise", advertise, "region", region, "interval", interval.String())
}
