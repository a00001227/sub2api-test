package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 疑似蒸馏取证：请求原文捕获的 Redis 存储（临时缓冲 + 兜底 TTL，与 riskv2:* 键完全隔离）。
// 键：
//   evcap:flags           HASH field=target_key(u:<id>/k:<id>) value=JSON(EvidenceFlag)  —— 捕获名单，重启可恢复。
//   evcap:buf:<target_key> LIST 每条 JSON(EvidenceEntry)，LPUSH+LTRIM 封顶 N、EXPIRE 兜底 TTL。
const (
	evcapFlagsKey  = "evcap:flags"
	evcapBufPrefix = "evcap:buf:"
)

type evidenceCaptureStore struct{ rdb *redis.Client }

// NewEvidenceCaptureStore 构造证据捕获存储；rdb 为 nil → nil（服务侧据此禁用）。
func NewEvidenceCaptureStore(rdb *redis.Client) service.EvidenceCaptureStore {
	if rdb == nil {
		return nil
	}
	return &evidenceCaptureStore{rdb: rdb}
}

func evcapBufKey(targetKey string) string { return evcapBufPrefix + targetKey }

func (s *evidenceCaptureStore) LoadFlags(ctx context.Context) ([]service.EvidenceFlag, error) {
	m, err := s.rdb.HGetAll(ctx, evcapFlagsKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]service.EvidenceFlag, 0, len(m))
	for _, v := range m {
		var f service.EvidenceFlag
		if json.Unmarshal([]byte(v), &f) == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *evidenceCaptureStore) SaveFlag(ctx context.Context, f service.EvidenceFlag) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return s.rdb.HSet(ctx, evcapFlagsKey, f.TargetKey, b).Err()
}

func (s *evidenceCaptureStore) DeleteFlag(ctx context.Context, targetKey string) error {
	return s.rdb.HDel(ctx, evcapFlagsKey, targetKey).Err()
}

func (s *evidenceCaptureStore) AppendEvidence(ctx context.Context, targetKey string, e service.EvidenceEntry, capN int, ttl time.Duration) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	key := evcapBufKey(targetKey)
	pipe := s.rdb.TxPipeline()
	pipe.LPush(ctx, key, b)
	if capN > 0 {
		pipe.LTrim(ctx, key, 0, int64(capN-1)) // 封顶 N 条，超出丢弃最旧
	}
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *evidenceCaptureStore) ListEvidence(ctx context.Context, targetKey string, limit int) ([]service.EvidenceEntry, error) {
	if limit <= 0 {
		limit = 1
	}
	vals, err := s.rdb.LRange(ctx, evcapBufKey(targetKey), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]service.EvidenceEntry, 0, len(vals))
	for _, v := range vals {
		var e service.EvidenceEntry
		if json.Unmarshal([]byte(v), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *evidenceCaptureStore) PurgeEvidence(ctx context.Context, targetKey string) error {
	return s.rdb.Del(ctx, evcapBufKey(targetKey)).Err()
}
