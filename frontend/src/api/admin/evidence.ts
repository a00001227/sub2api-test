/**
 * Admin Evidence Capture API — 疑似蒸馏取证：按「重复模板」聚合捕获请求原文。
 *
 * 管理员标记某 user/key → 对其请求算提示词 simhash → 只有同一模板重复≥阈值才存一份
 * 代表性原文(脱敏) + 重复次数/涉及 Key/时间跨度。正常一次性请求只留 simhash、不存原文。
 * 后端返回裸 DTO（非信封），apiClient 拦截器原样透传，故各调用返回 response.data 即业务负载。
 */

import { apiClient } from '../client'

export type EvidenceTargetType = 'user' | 'key'

export interface EvidenceFlag {
  target_key: string // "u:<id>" | "k:<id>"
  target_type: string
  target_id: number
  store_threshold: number
  max_templates: number
  started_at: number
  admin_id: number
}

// 一个「重复模板」的聚合证据。
export interface EvidenceTemplate {
  simhash: string
  count: number
  first_seen: number
  last_seen: number
  model: string
  endpoint: string
  api_key_ids: number[]
  request_ids: string[]
  body: string // 达阈值时存的代表性原文(已脱敏 + 限大小)
  truncated: boolean
  has_body: boolean
}

export const evidenceAPI = {
  /** 标记某 user/key 开始重复模板取证。storeThreshold=同模板重复几次才存原文(<2 回落默认)。 */
  async startCapture(targetType: EvidenceTargetType, targetId: number, storeThreshold: number): Promise<EvidenceFlag> {
    const { data } = await apiClient.post<{ capture: EvidenceFlag }>('/admin/evidence/captures', {
      target_type: targetType,
      target_id: targetId,
      store_threshold: storeThreshold,
    })
    return data.capture
  },

  /** 当前活跃捕获名单。 */
  async listCaptures(): Promise<EvidenceFlag[]> {
    const { data } = await apiClient.get<{ captures: EvidenceFlag[] }>('/admin/evidence/captures')
    return data.captures ?? []
  },

  /** 取某 target(u:<id>/k:<id>) 已聚合的重复模板（按 count 降序）。 */
  async listTemplates(target: string): Promise<EvidenceTemplate[]> {
    const { data } = await apiClient.get<{ templates: EvidenceTemplate[] }>(
      `/admin/evidence/captures/${encodeURIComponent(target)}`,
    )
    return data.templates ?? []
  },

  /** 清除某 target 证据 + 停止捕获。 */
  async purge(target: string): Promise<void> {
    await apiClient.delete(`/admin/evidence/captures/${encodeURIComponent(target)}`)
  },
}

export default evidenceAPI
