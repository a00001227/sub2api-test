/**
 * Admin Evidence Capture API — 疑似蒸馏取证：请求原文捕获。
 *
 * 管理员标记某 user/key → 捕获其后续 N 条请求原文(脱敏) → 查看/导出 → 清除。
 * 后端返回裸 DTO（非 {code,message,data} 信封），apiClient 拦截器原样透传，
 * 故各调用返回 response.data 即为业务负载。
 */

import { apiClient } from '../client'

export type EvidenceTargetType = 'user' | 'key'

export interface EvidenceFlag {
  target_key: string // "u:<id>" | "k:<id>"
  target_type: string
  target_id: number
  remaining: number
  max: number
  started_at: number
  admin_id: number
}

export interface EvidenceEntry {
  ts: number
  user_id: number
  api_key_id: number
  request_id: string // 平台生成的 client_request_id
  model: string
  endpoint: string
  ip: string
  body: string // 已脱敏 + 限大小
  truncated: boolean
  prompt_simhash: string // 归一化 simhash(hex)；相同=模板重复
}

export const evidenceAPI = {
  /** 标记某 user/key 开始捕获后续 maxCount 条请求原文。 */
  async startCapture(targetType: EvidenceTargetType, targetId: number, maxCount: number): Promise<EvidenceFlag> {
    const { data } = await apiClient.post<{ capture: EvidenceFlag }>('/admin/evidence/captures', {
      target_type: targetType,
      target_id: targetId,
      max_count: maxCount,
    })
    return data.capture
  },

  /** 当前活跃捕获名单 + 剩余计数。 */
  async listCaptures(): Promise<EvidenceFlag[]> {
    const { data } = await apiClient.get<{ captures: EvidenceFlag[] }>('/admin/evidence/captures')
    return data.captures ?? []
  },

  /** 取某 target(u:<id>/k:<id>) 已捕获条目。 */
  async listEvidence(target: string): Promise<EvidenceEntry[]> {
    const { data } = await apiClient.get<{ entries: EvidenceEntry[] }>(
      `/admin/evidence/captures/${encodeURIComponent(target)}`,
    )
    return data.entries ?? []
  },

  /** 清除某 target 证据 + 停止捕获。 */
  async purge(target: string): Promise<void> {
    await apiClient.delete(`/admin/evidence/captures/${encodeURIComponent(target)}`)
  },
}

export default evidenceAPI
