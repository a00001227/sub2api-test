package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// stickyAffinityTTL 会话→cell 绑定的存活时间,对齐 sub2api 的号级粘性会话
// (gateway_service.stickySessionTTL = 1 小时)。原先取 5 分钟(Anthropic 会话缓存
// AnthropicSessionTTL)过短:Codex /v1/responses 多轮对话用户思考间隔常超 5 分钟,
// 绑定一过期,下一轮就改选别的 cell → 落到别的号 → 上一轮合成的会话 item id 在新号
// 上校验失败(400 Invalid 'input[N].id')。拉长到 1 小时覆盖正常对话跨度;绑定到已
// 下线 cell 会被 moveToFront 自动忽略,map 由 affinitySweepThreshold 惰性清扫兜底,
// 都不受 TTL 影响。
const stickyAffinityTTL = time.Hour

// affinitySweepThreshold 触发惰性清扫过期项的 map 规模阈值(防止无界增长)。
const affinitySweepThreshold = 4096

// stickySessionKey 从请求体计算一个「跨轮稳定」的会话键,用于把同一会话固定路由回
// 同一 cell(P3-3c)。注意:中央这层的键无需和 cell 内部的 GenerateSessionHash 逐字
// 一致 —— 两级各自独立,只要中央把同一会话稳定送回同一 cell,cell 再用自己那套做
// 号级粘性即可。取值优先级:
//  1. metadata.user_id —— 客户端显式会话/设备标识,最稳(Claude Code / 自定义客户端)。
//  2. prompt_cache_key —— Codex /v1/responses 每轮都携带的会话锚点,跨轮稳定,是
//     responses 有状态对话唯一可靠的会话标识(见 openai_codex_transform 读取处)。
//  3. system/instructions + 首条内容摘要 —— 兼容 Chat Completions(system+messages)
//     与 Responses(instructions+input)的首轮内容;尽力而为(带 previous_response_id
//     的后续轮只发增量,哈希会变,故仅作最后兜底,真正靠 1/2)。
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
	if pck := gjson.GetBytes(body, "prompt_cache_key"); pck.Exists() {
		if s := strings.TrimSpace(pck.String()); s != "" {
			return "pck:" + s
		}
	}
	// system(Anthropic/Chat)→ 退回 instructions(Responses)。
	sys := gjson.GetBytes(body, "system").Raw
	if sys == "" {
		sys = gjson.GetBytes(body, "instructions").Raw
	}
	// messages.0.content(Chat)→ 退回 input(Responses,可为字符串或数组)。
	first := gjson.GetBytes(body, "messages.0.content").Raw
	if first == "" {
		first = gjson.GetBytes(body, "input").Raw
	}
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

// delete 解除会话→cell 绑定。用于该 cell 在流中途断开时驱逐陈旧绑定,让下一轮同会话
// 重新选路(换一台 cell),而不是被亲和粘回同一台死 cell(否则客户端会一直 api_error,
// 只能新开对话才能逃出)。
func (s *sessionAffinity) delete(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}
