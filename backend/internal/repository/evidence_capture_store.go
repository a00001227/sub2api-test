package repository

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 疑似蒸馏取证：按「重复模板」聚合的 Redis 存储（临时缓冲 + 兜底 TTL，与 riskv2:* 键完全隔离）。
// 键：
//   evcap:flags            HASH field=target_key(u:<id>/k:<id>) value=JSON(EvidenceFlag) —— 捕获名单，重启可恢复。
//   evcap:tpl:<target_key> HASH field=simhash value=JSON(EvidenceTemplate) —— 每个重复模板一份聚合，整体 EXPIRE 兜底 TTL。
const (
	evcapFlagsKey  = "evcap:flags"
	evcapTplPrefix = "evcap:tpl:"
)

type evidenceCaptureStore struct{ rdb *redis.Client }

// NewEvidenceCaptureStore 构造证据捕获存储；rdb 为 nil → nil（服务侧据此禁用）。
func NewEvidenceCaptureStore(rdb *redis.Client) service.EvidenceCaptureStore {
	if rdb == nil {
		return nil
	}
	return &evidenceCaptureStore{rdb: rdb}
}

func evcapTplKey(targetKey string) string { return evcapTplPrefix + targetKey }

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

func (s *evidenceCaptureStore) GetTemplate(ctx context.Context, targetKey, simhash string) (*service.EvidenceTemplate, error) {
	v, err := s.rdb.HGet(ctx, evcapTplKey(targetKey), simhash).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t service.EvidenceTemplate
	if err := json.Unmarshal([]byte(v), &t); err != nil {
		return nil, nil // 坏数据当不存在
	}
	return &t, nil
}

func (s *evidenceCaptureStore) PutTemplate(ctx context.Context, targetKey string, t service.EvidenceTemplate, ttl time.Duration) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	key := evcapTplKey(targetKey)
	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, key, t.Simhash, b)
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl) // 每次写刷新兜底 TTL
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *evidenceCaptureStore) TemplateCount(ctx context.Context, targetKey string) (int, error) {
	n, err := s.rdb.HLen(ctx, evcapTplKey(targetKey)).Result()
	return int(n), err
}

func (s *evidenceCaptureStore) ListTemplates(ctx context.Context, targetKey string) ([]service.EvidenceTemplate, error) {
	m, err := s.rdb.HGetAll(ctx, evcapTplKey(targetKey)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]service.EvidenceTemplate, 0, len(m))
	for _, v := range m {
		var t service.EvidenceTemplate
		if json.Unmarshal([]byte(v), &t) == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count }) // 重复次数降序
	return out, nil
}

func (s *evidenceCaptureStore) PurgeEvidence(ctx context.Context, targetKey string) error {
	return s.rdb.Del(ctx, evcapTplKey(targetKey)).Err()
}
