package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"golang.org/x/text/unicode/norm"
)

// Model Extraction Risk V2 Shadow — P1A/P1A.1/P1A.2：请求分段 + 有界指纹 + 有界异步采集。
//
// 受 risk.v2.enabled 门控（默认关闭）。启用时仅额外做「Prompt 分段 + 指纹 + 有界采集」；
// 绝不据此执行任何限速/封禁/账号切换/代理切换/上游规避。
//
// 双轨说明：legacy f1–f7 / user_risk / risk sketch 保持不变；V2 自身用有界 dispatcher。
// 启用 V2 时 legacy 与 V2 暂时双轨运行（V2 未取代 legacy 的任何东西）。
//
// 隐私边界：只对「本次新增 user turn」的派生文本计算指纹后即弃；绝不持久保存原文；
// 图片/文件/base64/tool_result 一律不进入指纹；envelope 只含标量/摘要/哈希，不含任何原文。
//
// 术语（措辞）：
//   - Exact/Scaffold 指纹是 keyed HMAC-SHA256「带密钥摘要」（keyed digest，含用途域分隔），
//     不是可解密加密，也不宣称「密码学不可逆」或「匿名化」。
//   - NearDup 是 64-bit SimHash「相似性 sketch」（未加密、未 keyed），用于近重复度量。

const (
	// RiskV2FingerprintVersion 指纹族版本（随记录落存以便回放）。
	RiskV2FingerprintVersion = "fp-v2"
	// RiskV2NormalizationVersion 文本归一化版本。
	RiskV2NormalizationVersion = "norm-v2"

	// HMAC 用途域分隔前缀（purpose separation）：即便共用基础密钥，也保证不同类型指纹不可跨字段比较。
	riskPurposeExact    = "risk-exact:v1\x00"
	riskPurposeScaffold = "risk-scaffold:v1\x00"
)

// 占位符正则（一次编译，仅作用于 bounded 样本）。
var (
	reRiskURL       = regexp.MustCompile(`https?://[^\s]+`)
	reRiskEmail     = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]+`)
	reRiskUUID      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reRiskTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}([T ]\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?)?`)
	reRiskPath      = regexp.MustCompile(`(/[\w.\-]+){2,}/?`)
	reRiskLongID    = regexp.MustCompile(`\b\d{6,}\b`)
)

// b2s 零拷贝把只读 []byte 视作 string（仅用于 gjson 解析，绝不修改底层，也不逃逸出请求处理）。
// 这样对超大 body 也不产生 O(body) 的额外拷贝（gjson.ParseBytes 会 string(copy)）。
func b2s(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

// ─────────────────────────── 有界原文样本提取（不先拼接全文）───────────────────────────

// stripJSONQuotes 去掉 JSON 字符串值 raw 两端的引号，返回内部（仍是 escaped）子串（零拷贝子串）。
func stripJSONQuotes(raw string) string {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// collectTurnRawSpans 收集「本次 user turn」各 text 块的 raw(escaped) 内部子串（零拷贝，引用底层 body）。
// 跳过 image/document/tool_use/base64；标记 tool_result。绝不 materialize 整块 .String()。
func collectTurnRawSpans(msg gjson.Result) (spans []string, toolResult bool) {
	content := msg.Get("content")
	if !content.Exists() {
		if parts := msg.Get("parts"); parts.IsArray() { // Gemini
			parts.ForEach(func(_, p gjson.Result) bool {
				if t := p.Get("text"); t.Exists() && t.Type == gjson.String {
					spans = append(spans, stripJSONQuotes(t.Raw))
				}
				return true
			})
		}
		return spans, toolResult
	}
	if content.Type == gjson.String {
		return []string{stripJSONQuotes(content.Raw)}, false
	}
	if content.IsArray() {
		content.ForEach(func(_, blk gjson.Result) bool {
			switch blk.Get("type").String() {
			case "text", "input_text", "output_text":
				if t := blk.Get("text"); t.Type == gjson.String {
					spans = append(spans, stripJSONQuotes(t.Raw))
				}
			case "tool_result":
				toolResult = true
			default:
				// image / document / tool_use / base64：不进入长度拼接，不进入指纹。
			}
			return true
		})
	}
	return spans, toolResult
}

// appendWindow 把 spans 拼接视图上的字节窗口 [lo,hi) 追加到 buf（只复制窗口所需区间）。
func appendWindow(buf []byte, spans []string, lo, hi int) []byte {
	if lo < 0 {
		lo = 0
	}
	off := 0
	for _, sp := range spans {
		sl := len(sp)
		if off >= hi {
			break
		}
		a := lo - off
		if a < 0 {
			a = 0
		}
		b := hi - off
		if b > sl {
			b = sl
		}
		if a < b {
			buf = append(buf, sp[a:b]...)
		}
		off += sl
	}
	return buf
}

// sampleTurnEscaped 基于 spans 的逻辑长度做确定性 head/middle/tail 采样（不先拼接全文）。
// total<=maxBytes → 完整拼接（未截断）；否则只复制三段窗口（≈maxBytes）。返回 escaped 字节 + 是否截断。
func sampleTurnEscaped(spans []string, maxBytes int) (escaped []byte, originalBytes int, truncated bool) {
	total := 0
	for _, sp := range spans {
		total += len(sp)
	}
	originalBytes = total
	if maxBytes <= 0 {
		maxBytes = 1
	}
	if total <= maxBytes {
		buf := make([]byte, 0, total)
		for _, sp := range spans {
			buf = append(buf, sp...)
		}
		return buf, total, false
	}
	part := maxBytes / 3
	if part <= 0 {
		part = maxBytes
	}
	midStart := (total - part) / 2
	if midStart < 0 {
		midStart = 0
	}
	windows := [3][2]int{{0, part}, {midStart, midStart + part}, {total - part, total}}
	buf := make([]byte, 0, 3*part+2)
	for wi, w := range windows {
		if wi > 0 {
			buf = append(buf, 0x1e) // 记录分隔符，避免三段拼成伪 token
		}
		buf = appendWindow(buf, spans, w[0], w[1])
	}
	return buf, total, true
}

// jsonUnescape 容错地把 JSON-escaped 字节解码为文本（样本可能在 escape/rune 边界被切断 → 尽力而为）。
// 只在 ≤maxBytes 的样本上调用，分配受 maxBytes 约束。
func jsonUnescape(b []byte) string {
	if strings.IndexByte(b2s(b), '\\') < 0 {
		return strings.ToValidUTF8(string(b), "")
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != '\\' {
			sb.WriteByte(c)
			continue
		}
		if i+1 >= len(b) {
			break // 悬空反斜杠（被切断）→ 丢弃
		}
		i++
		switch b[i] {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case '"':
			sb.WriteByte('"')
		case '\\':
			sb.WriteByte('\\')
		case '/':
			sb.WriteByte('/')
		case 'u':
			if i+4 < len(b) {
				if r, ok := parseHex4(b[i+1 : i+5]); ok {
					sb.WriteRune(rune(r))
					i += 4
				} else {
					sb.WriteByte('\\')
					sb.WriteByte('u')
				}
			}
		default:
			sb.WriteByte(b[i])
		}
	}
	return strings.ToValidUTF8(sb.String(), "")
}

func parseHex4(b []byte) (int, bool) {
	if len(b) < 4 {
		return 0, false
	}
	v := 0
	for i := 0; i < 4; i++ {
		c := b[i]
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= int(c - '0')
		case c >= 'a' && c <= 'f':
			v |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// ─────────────────────────── 三种文本表示 ───────────────────────────
//
//   - canonical_exact_text：内容类型感知（content-aware）保义 canonicalization。
//     JSON → 键排序、值保留、数组序保留、字符串内空白保留、UseNumber；
//     普通文本/代码 → NFC + CRLF→LF + 去首尾空白，**不折叠任何内部空白**（缩进/Tab/串内空白保留）。
//     仅在「完整」（未采样/未截断）文本上计算 → ExactHMAC 才可用。
//   - near_duplicate_text：宽松折叠所有空白 + lowercase（相似性 SimHash 输入，可用于采样样本）。
//   - scaffold_text：宽松折叠基础上做变量占位化 + 折叠残余数字段（模板枚举，可用于采样样本）。

// canonicalizeExactJSON 若输入是合法 JSON，返回键排序、去无意义空白、标量/数组序/串内空白保留
// 的规范序列化（UseNumber 保证大整数不失真）。否则 (,false)。
func canonicalizeExactJSON(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	if dec.More() { // 尾部还有内容 → 非纯 JSON
		return "", false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// canonicalizeExactText 产出 canonical_exact_text（content-aware，保留内部空白/缩进）。
func canonicalizeExactText(s string) string {
	if c, ok := canonicalizeExactJSON(s); ok {
		return c
	}
	s = norm.NFC.String(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Trim(s, " \t\n\r\f\v")
}

// laxFold 宽松折叠所有 Unicode 空白为单空格并 trim（用于 near/scaffold；NFC 归一）。
func laxFold(s string) string {
	s = norm.NFC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if b.Len() > 0 && !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimRight(b.String(), " ")
}

// nearDuplicateText 产出 near_duplicate_text：宽松折叠 + 小写（相似性输入）。
func nearDuplicateText(s string) string { return strings.ToLower(laxFold(s)) }

// scaffoldText 产出 scaffold_text：宽松折叠基础上占位化变量 + 折叠残余数字段。
func scaffoldText(s string) string {
	s = laxFold(s)
	s = reRiskURL.ReplaceAllString(s, " <URL> ")
	s = reRiskEmail.ReplaceAllString(s, " <EMAIL> ")
	s = reRiskUUID.ReplaceAllString(s, " <UUID> ")
	s = reRiskTimestamp.ReplaceAllString(s, " <TIMESTAMP> ")
	s = reRiskPath.ReplaceAllString(s, " <PATH> ")
	s = reRiskLongID.ReplaceAllString(s, " <ID> ")
	s = strings.ToLower(s)

	var b strings.Builder
	b.Grow(len(s))
	inDigits := false
	lastSpace := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if !inDigits {
				b.WriteByte('#')
				inDigits = true
			}
			lastSpace = false
		case unicode.IsSpace(r):
			inDigits = false
			if b.Len() > 0 && !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		default:
			inDigits = false
			lastSpace = false
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// riskKeyedFingerprint 计算带用途域分隔的 keyed HMAC-SHA256（hex，带 key 版本前缀）。
// key 或 text 为空 → ""（不可用）。purpose 保证不同类型指纹不可跨字段直接比较。
func riskKeyedFingerprint(purpose, text, key, keyVersion string) string {
	if key == "" || text == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte(text))
	if keyVersion == "" {
		keyVersion = "v1"
	}
	return keyVersion + ":" + hex.EncodeToString(mac.Sum(nil))
}

// ─────────────────────────── 分段 ───────────────────────────

// SegmentedRequest 是最小分段结果（仅内存；不含 system/tools/history 文本）。NewUserTurn 为 bounded 样本。
type SegmentedRequest struct {
	NewUserTurn          string // 本次 user turn 的 bounded 样本文本（即算即弃）
	NewUserTurnAvailable bool
	InputOriginalBytes   int  // 原始 turn 文本字节数（escaped raw 长度，用于判断是否采样）
	Truncated            bool // 超过 max_text_bytes → 走 head/middle/tail 采样
	ToolResultPresent    bool
	SystemPresent        bool
	ToolsPresent         bool
	HistoryCount         int
}

// SegmentRequestForRisk 将请求分段并给出 bounded 样本。maxBytes 限制样本与分配。
func SegmentRequestForRisk(parsed *ParsedRequest, maxBytes int) SegmentedRequest {
	if parsed == nil {
		return SegmentedRequest{}
	}
	return segmentRiskFromRaw(parsed.MessagesRaw(), parsed.InputRaw(), bodyRawOf(parsed),
		parsed.HasSystem || len(parsed.SystemRaw()) > 0, maxBytes)
}

func bodyRawOf(parsed *ParsedRequest) []byte {
	if parsed == nil || parsed.Body == nil {
		return nil
	}
	return parsed.Body.Bytes()
}

// segmentRiskFromRaw 是分段的纯核心（仅依赖原始字节，便于单测）。用 b2s+gjson.Parse 避免大 body 拷贝。
func segmentRiskFromRaw(messagesRaw, inputRaw, bodyRaw []byte, systemPresent bool, maxBytes int) SegmentedRequest {
	var seg SegmentedRequest
	seg.SystemPresent = systemPresent
	if len(bodyRaw) > 0 {
		bs := b2s(bodyRaw)
		if t := gjson.Get(bs, "tools"); t.IsArray() && len(t.Array()) > 0 {
			seg.ToolsPresent = true
		}
		if f := gjson.Get(bs, "functions"); f.IsArray() && len(f.Array()) > 0 {
			seg.ToolsPresent = true
		}
	}

	raw := messagesRaw
	if len(raw) == 0 {
		raw = inputRaw
	}
	if len(raw) == 0 {
		return seg
	}
	res := gjson.Parse(b2s(raw))
	if res.Type == gjson.String { // Responses API：input 为纯字符串
		spans := []string{stripJSONQuotes(res.Raw)}
		escaped, orig, truncated := sampleTurnEscaped(spans, maxBytes)
		seg.InputOriginalBytes = orig
		seg.Truncated = truncated
		if txt := strings.TrimSpace(jsonUnescape(escaped)); txt != "" {
			seg.NewUserTurn = txt
			seg.NewUserTurnAvailable = true
		}
		return seg
	}
	if !res.IsArray() {
		return seg
	}
	msgs := res.Array()
	if len(msgs) == 0 {
		return seg
	}
	for _, m := range msgs {
		if m.Get("role").String() == "system" {
			seg.SystemPresent = true
			break
		}
	}
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Get("role").String() == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		seg.HistoryCount = len(msgs)
		return seg
	}
	spans, toolResult := collectTurnRawSpans(msgs[lastUser])
	seg.ToolResultPresent = toolResult
	seg.HistoryCount = len(msgs) - 1
	escaped, orig, truncated := sampleTurnEscaped(spans, maxBytes)
	seg.InputOriginalBytes = orig
	seg.Truncated = truncated
	if txt := strings.TrimSpace(jsonUnescape(escaped)); txt != "" {
		seg.NewUserTurn = txt
		seg.NewUserTurnAvailable = true
	}
	return seg
}

// ─────────────────────────── 请求侧特征 ───────────────────────────

// RiskV2RequestFeatures 是请求阶段算出的不可逆特征。绝不含原文。
type RiskV2RequestFeatures struct {
	TurnAvailable bool

	// 输入规模与采样标记（明确区分完整 vs 采样）。
	InputOriginalBytes int
	InputSampledBytes  int
	InputTruncated     bool

	// Exact：仅当完整 canonical exact text 被处理（未截断）时可用。
	ExactFingerprintAvailable bool
	ExactHMAC                 string // 完整时才有值；采样/截断时为空，且绝不参与重复率统计

	// Scaffold / NearDup：允许基于 head/middle/tail 样本计算，明确标记 sampled。
	ScaffoldHMAC               string
	ScaffoldFingerprintSampled bool
	NearDupSimhash             uint64
	HasNearDup                 bool
	NearFingerprintSampled     bool

	KeyVersion           string
	FingerprintVersion   string
	NormalizationVersion string

	SystemPresent     bool
	ToolsPresent      bool
	ToolResultPresent bool
	HistoryCount      int
	Streaming         bool

	// 采样参数（非敏感；用于 repeated-sampling 的 parameter signature）。绝不保存完整参数 JSON。
	Temperature        *float64
	TopP               *float64
	MaxTokens          int
	HasSeed            bool
	ResponseFormatType string
}

// BuildRiskV2RequestFeatures 在请求阶段计算 V2 请求侧特征。未 EffectiveEnabled（含 DEGRADED）→ nil。
func BuildRiskV2RequestFeatures(parsed *ParsedRequest, cfg config.RiskV2Config) *RiskV2RequestFeatures {
	if parsed == nil || !cfg.EffectiveEnabled() {
		return nil
	}
	maxBytes := cfg.MaxTextBytesNormalized()
	seg := segmentRiskFromRaw(parsed.MessagesRaw(), parsed.InputRaw(), bodyRawOf(parsed),
		parsed.HasSystem || len(parsed.SystemRaw()) > 0, maxBytes)

	out := &RiskV2RequestFeatures{
		FingerprintVersion:   RiskV2FingerprintVersion,
		NormalizationVersion: RiskV2NormalizationVersion,
		KeyVersion:           cfg.FingerprintKeyVersion,
		SystemPresent:        seg.SystemPresent,
		ToolsPresent:         seg.ToolsPresent,
		ToolResultPresent:    seg.ToolResultPresent,
		HistoryCount:         seg.HistoryCount,
		Streaming:            parsed.Stream,
		TurnAvailable:        seg.NewUserTurnAvailable,
		InputOriginalBytes:   seg.InputOriginalBytes,
		InputTruncated:       seg.Truncated,
		Temperature:          parsed.Temperature,
		MaxTokens:            parsed.MaxTokens,
	}
	// 采样参数（非敏感）：仅取标量,绝不保存完整参数 JSON。
	if body := bodyRawOf(parsed); len(body) > 0 {
		bs := b2s(body)
		if tp := gjson.Get(bs, "top_p"); tp.Exists() && tp.Type == gjson.Number {
			v := tp.Float()
			out.TopP = &v
		}
		out.HasSeed = gjson.Get(bs, "seed").Exists()
		if rf := gjson.Get(bs, "response_format.type"); rf.Exists() {
			out.ResponseFormatType = rf.String()
		}
	}
	if !seg.NewUserTurnAvailable {
		return out
	}
	sample := seg.NewUserTurn // 已是 bounded 样本（未截断时=完整文本）
	out.InputSampledBytes = len(sample)
	key := cfg.FingerprintHMACKey.Reveal()
	ver := cfg.FingerprintKeyVersion

	// Near / Scaffold：允许基于样本计算，随 InputTruncated 标记 sampled。
	near := nearDuplicateText(sample)
	out.NearDupSimhash = ComputeMessagesSimhash([]byte(near))
	out.HasNearDup = out.NearDupSimhash != 0
	out.NearFingerprintSampled = seg.Truncated
	out.ScaffoldHMAC = riskKeyedFingerprint(riskPurposeScaffold, scaffoldText(sample), key, ver)
	out.ScaffoldFingerprintSampled = seg.Truncated

	// Exact：只有完整文本（未截断）才计算真正的精确指纹。
	if !seg.Truncated {
		exact := canonicalizeExactText(sample)
		hmacExact := riskKeyedFingerprint(riskPurposeExact, exact, key, ver)
		out.ExactHMAC = hmacExact
		out.ExactFingerprintAvailable = hmacExact != ""
	}
	return out
}

// ─────────────────────────── 观测包 ───────────────────────────

// RiskFeatureEnvelope 是「请求侧特征 + 响应后 usage/终态」合并的不可逆观测包。
// 白名单原则：仅标量/摘要/时间戳/ID。绝不含任何原文（反射测试守卫）。
type RiskFeatureEnvelope struct {
	// —— 请求身份 ——
	// ServerRequestID 由平台在下游请求入口生成，每请求唯一，是 dispatcher 去重的唯一依据。
	// 绝不来自客户端 Header/请求体/billing/idempotency/客户端自定义 ID。
	// 隐私：绝不在此携带原始 client_request_id / billing_request_id（可能来自客户端）——
	// 它们不得进入 Redis/DB/日志/Admin。如需关联，仅以 HMAC(独立 purpose + 长度受限 ID) 形式另行处理。
	ServerRequestID string
	UserID          int64
	APIKeyID        int64 // 内部 DB ID（绝不含完整 API Key 明文）

	// —— 生命周期 ——
	RequestStartedAt   time.Time
	RequestCompletedAt time.Time
	TerminalStatus     string
	UsageAvailable     bool

	// —— 采样参数（非敏感；repeated-sampling parameter signature 用）——
	TopP               *float64
	HasSeed            bool
	ResponseFormatType string

	// —— usage（缺失字段保持 nil，不填 0）——
	Model                string
	Streaming            bool
	MaxTokens            int
	Temperature          *float64
	InputTokens          int64
	OutputTokens         int64
	CacheUsageApplicable bool   // Provider/路径是否明确支持并已正确解析缓存 usage
	CacheUsageAvailable  bool   // 是否真实获得相应 cache 字段（Applicable 前提下）
	CacheCreationTokens  *int64 // nil = 不适用/不可用；pointer(0)=显式 0；pointer(v)=v
	CacheReadTokens      *int64

	// —— 指纹（请求侧）——
	TurnAvailable              bool
	InputOriginalBytes         int
	InputSampledBytes          int
	InputTruncated             bool
	ExactFingerprintAvailable  bool
	ExactHMAC                  string
	ScaffoldHMAC               string
	ScaffoldFingerprintSampled bool
	NearDupSimhash             uint64
	HasNearDup                 bool
	NearFingerprintSampled     bool
	KeyVersion                 string
	FingerprintVersion         string
	NormalizationVersion       string
	SystemPresent              bool
	ToolsPresent               bool
	ToolResultPresent          bool
	HistoryCount               int
}

// ─────────────────────────── Sink ───────────────────────────

// RiskV2Sink 消费观测包（P1A/P1A.1/P1A.2：仅计数/日志，不落库、不进最终风险判断）。
type RiskV2Sink interface {
	Consume(env RiskFeatureEnvelope)
}

// LogRiskV2Sink 默认 sink：仅计数。
type LogRiskV2Sink struct{ consumed int64 }

func NewLogRiskV2Sink() *LogRiskV2Sink { return &LogRiskV2Sink{} }

func (s *LogRiskV2Sink) Consume(_ RiskFeatureEnvelope) {
	if s != nil {
		atomic.AddInt64(&s.consumed, 1)
	}
}
func (s *LogRiskV2Sink) Consumed() int64 {
	if s == nil {
		return 0
	}
	return atomic.LoadInt64(&s.consumed)
}

// ─────────────────────────── Dispatcher ───────────────────────────

// RiskV2Stats 是 dispatcher 运行指标快照。
type RiskV2Stats struct {
	Enqueued     int64
	Processed    int64
	Dropped      int64
	Deduped      int64
	Evicted      int64 // 去重表因容量上限淘汰的 ID 数（有界证据）
	Panics       int64
	QueueDepth   int
	DedupTracked int // 当前去重表内 ID 数（<= 2*dedupCap）
	DedupCap     int
}

// RiskV2Dispatcher 有界异步采集器：
//   - Enqueue 非阻塞、返回是否入队成功；队满/停机/重复(同 server_request_id) → false；
//   - 每个 server_request_id 至多一条观测（有界去重，双代 map 轮换淘汰，绝不无界增长）；
//   - 单 worker，sink panic 自恢复并计数、worker 继续；
//   - Start / Stop(ctx) 幂等；Stop 有限时间 graceful drain；Stop 后 Enqueue 返回 false 不 panic。
type RiskV2Dispatcher struct {
	ch   chan RiskFeatureEnvelope
	sink RiskV2Sink

	enqueued  int64
	processed int64
	dropped   int64
	deduped   int64
	evicted   int64
	panics    int64

	started   int32
	stopped   int32
	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup

	dedupCap int
	dmu      sync.Mutex
	seenCur  map[string]struct{}
	seenPrev map[string]struct{}
}

// NewRiskV2Dispatcher 构造有界采集器（queueSize<=0 → 默认；dedupCap = max(queueSize*4, 4096)）。
func NewRiskV2Dispatcher(queueSize int, sink RiskV2Sink) *RiskV2Dispatcher {
	if queueSize <= 0 {
		queueSize = config.RiskV2QueueSizeDefault
	}
	dedupCap := queueSize * 4
	if dedupCap < 4096 {
		dedupCap = 4096
	}
	return newRiskV2Dispatcher(queueSize, dedupCap, sink)
}

// newRiskV2Dispatcher 允许显式指定 dedupCap（便于测试淘汰行为）。
func newRiskV2Dispatcher(queueSize, dedupCap int, sink RiskV2Sink) *RiskV2Dispatcher {
	if dedupCap < 1 {
		dedupCap = 1
	}
	return &RiskV2Dispatcher{
		ch:       make(chan RiskFeatureEnvelope, queueSize),
		sink:     sink,
		stopCh:   make(chan struct{}),
		dedupCap: dedupCap,
		seenCur:  make(map[string]struct{}),
		seenPrev: make(map[string]struct{}),
	}
}

// markSeen 原子「检查并登记」server_request_id：首见→true(放行)；重复→false。
// 双代 map 轮换实现有界去重（容量 ~2*dedupCap），满则淘汰最旧一代并计 Evicted。空 id 不去重。
func (d *RiskV2Dispatcher) markSeen(id string) bool {
	if id == "" {
		return true
	}
	d.dmu.Lock()
	defer d.dmu.Unlock()
	if _, ok := d.seenCur[id]; ok {
		return false
	}
	if _, ok := d.seenPrev[id]; ok {
		return false
	}
	if len(d.seenCur) >= d.dedupCap {
		atomic.AddInt64(&d.evicted, int64(len(d.seenPrev)))
		d.seenPrev = d.seenCur
		d.seenCur = make(map[string]struct{})
	}
	d.seenCur[id] = struct{}{}
	return true
}

// Enqueue 非阻塞入队，返回是否入队成功。停机/重复/队满 → false（各自计数）。
func (d *RiskV2Dispatcher) Enqueue(env RiskFeatureEnvelope) bool {
	if d == nil {
		return false
	}
	if atomic.LoadInt32(&d.stopped) == 1 {
		atomic.AddInt64(&d.dropped, 1)
		return false
	}
	if !d.markSeen(env.ServerRequestID) {
		atomic.AddInt64(&d.deduped, 1)
		return false
	}
	select {
	case d.ch <- env:
		atomic.AddInt64(&d.enqueued, 1)
		return true
	default:
		atomic.AddInt64(&d.dropped, 1)
		return false
	}
}

// Start 启动单 worker（幂等）。
func (d *RiskV2Dispatcher) Start() {
	if d == nil {
		return
	}
	d.startOnce.Do(func() {
		atomic.StoreInt32(&d.started, 1)
		d.wg.Add(1)
		go d.run()
	})
}

func (d *RiskV2Dispatcher) run() {
	defer d.wg.Done()
	for {
		select {
		case env := <-d.ch:
			d.consumeSafe(env)
		case <-d.stopCh:
			for {
				select {
				case env := <-d.ch:
					d.consumeSafe(env)
				default:
					return
				}
			}
		}
	}
}

func (d *RiskV2Dispatcher) consumeSafe(env RiskFeatureEnvelope) {
	defer func() {
		if r := recover(); r != nil {
			atomic.AddInt64(&d.panics, 1)
			logger.LegacyPrintf("service.risk_v2", "[risk_v2] sink panic recovered: %v", r)
		}
	}()
	if d.sink != nil {
		d.sink.Consume(env)
	}
	atomic.AddInt64(&d.processed, 1)
}

// Stop 幂等停机：置停机位、关 stopCh 触发 drain，并在 ctx 截止前等待 worker 收尾。
func (d *RiskV2Dispatcher) Stop(ctx context.Context) error {
	if d == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	d.stopOnce.Do(func() {
		atomic.StoreInt32(&d.stopped, 1)
		close(d.stopCh)
	})
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats 返回运行指标快照。
func (d *RiskV2Dispatcher) Stats() RiskV2Stats {
	if d == nil {
		return RiskV2Stats{}
	}
	d.dmu.Lock()
	tracked := len(d.seenCur) + len(d.seenPrev)
	d.dmu.Unlock()
	return RiskV2Stats{
		Enqueued:     atomic.LoadInt64(&d.enqueued),
		Processed:    atomic.LoadInt64(&d.processed),
		Dropped:      atomic.LoadInt64(&d.dropped),
		Deduped:      atomic.LoadInt64(&d.deduped),
		Evicted:      atomic.LoadInt64(&d.evicted),
		Panics:       atomic.LoadInt64(&d.panics),
		QueueDepth:   len(d.ch),
		DedupTracked: tracked,
		DedupCap:     d.dedupCap,
	}
}

// ─────────────────────────── Provider ───────────────────────────

// ProvideRiskV2Dispatcher 是 wire 源引用的 provider：
//   - disabled → nil（不创建 worker）；
//   - enabled 但配置无效 → 输出一次结构化配置错误（仅键名，无密钥值），返回 nil（DEGRADED，不启动）；
//   - enabled 且有效 → 用给定 sink 构造并 Start 一个有界 dispatcher 返回。
//
// sink 由装配处决定（aggregation_enabled 且 Redis 可用 → Redis 聚合 sink；否则计数 sink）；
// nil → 回退到计数 sink（fail-open）。
func ProvideRiskV2Dispatcher(cfg *config.Config, sink RiskV2Sink) *RiskV2Dispatcher {
	if cfg == nil || !cfg.Risk.V2.Enabled {
		return nil
	}
	if err := cfg.Risk.V2.Validate(); err != nil {
		logger.LegacyPrintf("service.risk_v2",
			"[risk_v2] enabled but configuration invalid → V2 DEGRADED (not started): %v", err)
		return nil
	}
	if sink == nil {
		sink = NewLogRiskV2Sink()
	}
	d := NewRiskV2Dispatcher(cfg.Risk.V2.QueueSizeNormalized(), sink)
	d.Start()
	logger.LegacyPrintf("service.risk_v2",
		"[risk_v2] shadow enabled (queue=%d, max_text_bytes=%d, key_version=%s, aggregation=%v, scoring=%v)",
		cfg.Risk.V2.QueueSizeNormalized(), cfg.Risk.V2.MaxTextBytesNormalized(), cfg.Risk.V2.FingerprintKeyVersion,
		cfg.Risk.V2.AggregationEnabled, cfg.Risk.V2.ScoringEnabled)
	return d
}
