package service

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 代理出口持久性故障 → 硬暂停账号(schedulable=false + 原因写 error_message + 回流 Portal
// "paused"),直到管理员/渠道商在 Portal 点「恢复」。与"临时下线 N 分钟自动放出"不同:
// 代理凭据失效、代理端点死掉这类故障不会自愈,自动放出只会让用户反复撞 502。
//
// 判定(两条平台路径共用,含 /v1/responses、/v1/chat/completions、/v1/messages):
//   - 一击即停:代理认证失败(SOCKS5 username/password authentication failed / HTTP 407)。
//   - 累计即停:连接被拒 / 无路由 / 网络不可达 / DNS 无此主机,10 分钟内同一账号 2 次。
//     单次可能是代理抖动,不停。
//   - 其余传输错误(超时/EOF/reset)不在此列,走各自既有的转移/临时下线逻辑。
const (
	egressFailureWindow     = 10 * time.Minute
	egressFailureStrikes    = 2
	egressPauseReasonPrefix = "egress proxy failure (paused; resume in Portal): "
	egressPauseWriteTimeout = 5 * time.Second
)

var egressFailureImmediateMarkers = []string{
	"authentication failed",         // SOCKS5 RFC1929 凭据被拒
	"proxy authentication required", // HTTP 代理 407
}

var egressFailurePersistentMarkers = []string{
	"connection refused",
	"no route to host",
	"network is unreachable",
	"no such host",
}

// egressPauseRepo 是 AccountRepository 的可选能力(type assertion 取得,不改接口以免动全部 mock)。
type egressPauseRepo interface {
	PauseForEgressFailure(ctx context.Context, id int64, reason string) error
	ResumeFromEgressPause(ctx context.Context, id int64) error
}

// classifyEgressFailure 判断传输错误是否为持久性代理出口故障;immediate=true 表示一击即停。
func classifyEgressFailure(err error) (persistent, immediate bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return false, false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range egressFailureImmediateMarkers {
		if strings.Contains(msg, m) {
			return true, true
		}
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true, false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true, false
	}
	for _, m := range egressFailurePersistentMarkers {
		if strings.Contains(msg, m) {
			return true, false
		}
	}
	return false, false
}

// egressFailureTracker 按账号累计窗口内的持久性故障次数(进程内,重启清零即可)。
type egressFailureTracker struct {
	mu      sync.Mutex
	strikes map[int64][]time.Time
}

var defaultEgressFailureTracker = &egressFailureTracker{strikes: map[int64][]time.Time{}}

// record 记一次并返回窗口内累计次数。
func (t *egressFailureTracker) record(id int64, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.strikes[id][:0]
	for _, ts := range t.strikes[id] {
		if now.Sub(ts) < egressFailureWindow {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	t.strikes[id] = kept
	return len(kept)
}

func (t *egressFailureTracker) reset(id int64) {
	t.mu.Lock()
	delete(t.strikes, id)
	t.mu.Unlock()
}

// pauseAccountForEgressFailure 按判定硬暂停账号;返回 true 表示已暂停(调用方可跳过临时下线)。
// repo 为 nil 或不支持该能力时只做判定不落库(测试/未接线),返回 false。
func pauseAccountForEgressFailure(ctx context.Context, repo AccountRepository, account *Account, rawErr error, safeErr string) bool {
	return pauseAccountForEgressFailureWith(ctx, repo, account, rawErr, safeErr, defaultEgressFailureTracker, time.Now())
}

func pauseAccountForEgressFailureWith(ctx context.Context, repo AccountRepository, account *Account, rawErr error, safeErr string, tracker *egressFailureTracker, now time.Time) bool {
	if account == nil || repo == nil {
		return false
	}
	persistent, immediate := classifyEgressFailure(rawErr)
	if !persistent {
		return false
	}
	if !immediate && tracker.record(account.ID, now) < egressFailureStrikes {
		return false
	}
	pr, ok := repo.(egressPauseRepo)
	if !ok {
		return false
	}
	reason := egressPauseReasonPrefix + truncateString(strings.TrimSpace(safeErr), 300)
	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), egressPauseWriteTimeout)
	defer cancel()
	log := logger.L().With(zap.String("component", "service.egress_pause"),
		zap.Int64("account_id", account.ID), zap.String("account_name", account.Name),
		zap.String("platform", account.Platform), zap.Bool("immediate", immediate), zap.String("cause", safeErr))
	if err := pr.PauseForEgressFailure(bgCtx, account.ID, reason); err != nil {
		log.Warn("egress_pause.account_pause_failed", zap.Error(err))
		return false
	}
	tracker.reset(account.ID)
	account.Schedulable = false
	account.ErrorMessage = reason
	log.Warn("egress_pause.account_paused")
	return true
}

// IsEgressPauseReason 报告 error_message 是否为本机制写入(恢复时据此清理)。
func IsEgressPauseReason(msg string) bool {
	return strings.HasPrefix(strings.TrimSpace(msg), egressPauseReasonPrefix)
}
