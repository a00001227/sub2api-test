/**
 * Admin Enforcement API — 蒸馏执行层：HIGH 自动限速 + 人工封禁。
 *
 * 只读 risk_v2 tier，绝不改 scoring。自动限速在请求路径中间件里做（默认关）；
 * 本 API 用于查看开关/名单、增删豁免、人工封禁/解封 user/key。
 * 后端返回裸 DTO（非信封），apiClient 拦截器原样透传。
 */

import { apiClient } from '../client'

export interface EnforcementStatus {
  enabled: boolean
  throttle_rpm: number
  confidence_min: number
  high_user_count: number
  allowlist_size: number
  model_rule_count: number
  refreshed_at: number
}

export type EnforcementModelAction = 'block' | 'throttle'

export interface EnforcementModelRule {
  model: string
  action: EnforcementModelAction
}

export interface EnforcementHighUser {
  user_id: number
  risk_index: number
  confidence: number
  data_sufficient: boolean
  assessed_at: number
  throttled: boolean
  allowlisted: boolean
}

export type EnforcementTargetType = 'user' | 'key'

export const enforcementAPI = {
  /** 执行层运行态（开关/阈值/HIGH 数/豁免数/刷新时间）。 */
  async getStatus(): Promise<EnforcementStatus | null> {
    const { data } = await apiClient.get<{ status: EnforcementStatus }>('/admin/enforcement/status')
    return data.status ?? null
  },

  /** 当前 HIGH 用户名单（含限速/豁免标注）。 */
  async listHighUsers(): Promise<EnforcementHighUser[]> {
    const { data } = await apiClient.get<{ users: EnforcementHighUser[] }>('/admin/enforcement/high-users')
    return data.users ?? []
  },

  /** 将某用户加入豁免名单（限速一票否决）。 */
  async addAllowlist(userId: number): Promise<void> {
    await apiClient.post('/admin/enforcement/allowlist', { user_id: userId })
  },

  /** 将某用户移出豁免名单。 */
  async removeAllowlist(userId: number): Promise<void> {
    await apiClient.delete(`/admin/enforcement/allowlist/${userId}`)
  },

  /** 人工封禁某 user/key（status→disabled，可逆）。 */
  async ban(targetType: EnforcementTargetType, targetId: number): Promise<void> {
    await apiClient.post('/admin/enforcement/ban', { target_type: targetType, target_id: targetId })
  },

  /** 解封某 user/key（status→active）。 */
  async unban(targetType: EnforcementTargetType, targetId: number): Promise<void> {
    await apiClient.post('/admin/enforcement/unban', { target_type: targetType, target_id: targetId })
  },

  /** 当前受限模型规则（仅对 HIGH 用户生效）。 */
  async listModelRules(): Promise<EnforcementModelRule[]> {
    const { data } = await apiClient.get<{ rules: EnforcementModelRule[] }>('/admin/enforcement/model-rules')
    return data.rules ?? []
  },

  /** 设置某受限模型的处置动作（block|throttle）。 */
  async setModelRule(model: string, action: EnforcementModelAction): Promise<void> {
    await apiClient.post('/admin/enforcement/model-rules', { model, action })
  },

  /** 移除某受限模型规则。 */
  async removeModelRule(model: string): Promise<void> {
    await apiClient.delete(`/admin/enforcement/model-rules/${encodeURIComponent(model)}`)
  },
}

export default enforcementAPI
