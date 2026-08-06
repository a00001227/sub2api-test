package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// stickyAffinityTTL 会话→cell 绑定的存活时间,对齐 sub2api 的 Anthropic 会话缓存
// (AnthropicSessionTTL = 5 分钟)。
const stickyAffinityTTL = 5 * time.Minute

// affinitySweepThreshold 触发惰性清扫过期项的 map 规模阈值(防止无界增长)。
const affinitySweepThreshold = 4096

// stickySessionKey 从请求体计算一个「跨轮稳定」的会话键,用于把同一会话固定路由回
// 同一 cell(P3-3c)。注意:中央这层的键无需和 cell 内部的 GenerateSessionHash 逐字
// 一致 —— 两级各自独立,只要中央把同一会话稳定送回同一 cell,cell 再用自己那套做
// 号级粘性即可。取值优先级:
//  1. metadata.user_id —— 客户端显式会话/设备标识,最稳。
//  2. system + 首条消息内容的摘要 —— 同一会话每轮都带相同的开头,跨轮稳定(尽力而为)。
//
// 取不到任何稳定信号 → 返回 ""(不做粘性,按加权随机首次落点)。
func stickySessionKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if uid := gjson.GetBytes(body, "metadata.user_id"); uid.Exists() {
		if s := uid.String(); s != "" {
			return "uid:" + s
		}
	}
	sys := gjson.GetBytes(body, "system").Raw
	first := gjson.GetBytes(body, "messages.0.content").Raw
	if sys == "" && first == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sys + "\x00" + first))
	return "conv:" + hex.EncodeToString(sum[:16])
}

type affinityEntry struct {
	u   *url.URL
	exp time.Time
}

// sessionAffinity 是进程内的「会话→cell」TTL 缓存(单中央足够;多中央实例共享留 Redis,
// 同 dynamicResolver 的延后策略)。now 可注入以便确定性测试过期。
type sessionAffinity struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]affinityEntry
}

func newSessionAffinity(ttl time.Duration) *sessionAffinity {
	return &sessionAffinity{ttl: ttl, now: time.Now, m: make(map[string]affinityEntry)}
}

// get 返回该会话上次落定的 cell(未绑定/已过期 → nil,false)。
func (s *sessionAffinity) get(key string) (*url.URL, bool) {
	if key == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return nil, false
	}
	if !s.now().Before(e.exp) {
		delete(s.m, key)
		return nil, false
	}
	return e.u, true
}

// put 绑定会话→cell,刷新 TTL。map 过大时惰性清扫过期项。
func (s *sessionAffinity) put(key string, u *url.URL) {
	if key == "" || u == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= affinitySweepThreshold {
		now := s.now()
		for k, e := range s.m {
			if !now.Before(e.exp) {
				delete(s.m, k)
			}
		}
	}
	s.m[key] = affinityEntry{u: u, exp: s.now().Add(s.ttl)}
}
