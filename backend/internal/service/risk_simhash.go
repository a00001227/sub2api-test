package service

import (
	"hash/fnv"
	"strings"
)

// Risk Phase 0（仅观测/影子模式）：请求消息的 64 位 simhash。
//
// 隐私：绝不存储 prompt/message 原文，只保留归一化后的 64 位指纹。
// 目标：相同模板、仅槽位（数字/空白/大小写）不同的请求 → 相同/相近的 hash，
// 从而在评分 worker 中通过“distinct/total 比值坍缩”识别模板化批量蒸馏。
//
// 全部计算在响应后的用量记录路径异步执行，绝不进入请求热路径。

// riskSimhashMaxInput 限制归一化输入的字节上限（首 8KB），避免大请求体拖慢异步路径。
const riskSimhashMaxInput = 8 * 1024

// normalizeForSimhash 归一化文本：小写、折叠空白、数字连续段替换为 '#'，截断到 8KB。
// 使得“同模板不同数字/空白”坍缩到同一指纹。
func normalizeForSimhash(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if len(raw) > riskSimhashMaxInput {
		raw = raw[:riskSimhashMaxInput]
	}

	var b strings.Builder
	b.Grow(len(raw))
	lastSpace := false
	inDigits := false
	for _, r := range string(raw) {
		switch {
		case r >= '0' && r <= '9':
			if !inDigits {
				b.WriteByte('#')
				inDigits = true
			}
			lastSpace = false
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v':
			inDigits = false
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
		default:
			inDigits = false
			lastSpace = false
			// ASCII 大写转小写；其它字符原样保留。
			if r >= 'A' && r <= 'Z' {
				b.WriteRune(r + ('a' - 'A'))
			} else {
				b.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// tokenizeForSimhash 将归一化文本切成 token（按空格）。空串返回 nil。
func tokenizeForSimhash(normalized string) []string {
	if normalized == "" {
		return nil
	}
	return strings.Fields(normalized)
}

// ComputeMessagesSimhash 计算消息 raw JSON 的 64 位 simhash（归一化 + token simhash）。
// 输入为 messages/contents/input 的 raw JSON（含结构噪声也无妨——同模板结构一致）。
// 返回值可能为 0（空输入），调用方据此决定是否落库。
func ComputeMessagesSimhash(raw []byte) uint64 {
	normalized := normalizeForSimhash(raw)
	tokens := tokenizeForSimhash(normalized)
	if len(tokens) == 0 {
		return 0
	}

	var v [64]int
	for _, tok := range tokens {
		h := fnv64a(tok)
		for i := 0; i < 64; i++ {
			if (h>>uint(i))&1 == 1 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}

	var fingerprint uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			fingerprint |= 1 << uint(i)
		}
	}
	return fingerprint
}

func fnv64a(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// hammingDistance64 返回两个 64 位指纹的汉明距离（用于测试/相似度评估）。
func hammingDistance64(a, b uint64) int {
	x := a ^ b
	count := 0
	for x != 0 {
		x &= x - 1
		count++
	}
	return count
}
