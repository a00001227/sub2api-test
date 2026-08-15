/**
 * Admin Risk Monitor (anti-distillation shadow) API
 *
 * Phase 0 is observe-only shadow mode: these endpoints surface scoring and
 * config but nothing is enforced against live traffic. The `apiClient` response
 * interceptor already unwraps the `{ code, message, data }` envelope, so every
 * call here returns the `data` payload directly.
 */

import { apiClient } from '../client'

export type RiskTier = 'watch' | 'medium' | 'high'
export type RiskMode = 'observe' | 'enforce'
export type RiskWouldDoAction = 'throttle_and_flag' | 'monitor_closely' | 'none'

/** Per-user derived feature vector. All values are normalized 0–1 unless noted. */
export interface RiskUserFeatures {
  f1_diversity_collapse: number
  f2_single_turn_ratio: number
  f3_output_harvest: number
  f4_machine_cadence: number
  f5_teacher_concentration: number
  f6_determinism: number
  f7_fanout_novelty: number
  distinct_ratio: number
  out_in_ratio: number
  top_model_share: number
  budget_daily_pct: number
  budget_weekly_pct: number
}

/** 24h rolling window aggregate for a user. */
export interface RiskUserWindow {
  requests_24h: number
  output_tokens_24h: number
  distinct_ratio: number
  single_turn_ratio: number
  top_model: string
  rpm_peak: number
  budget_pct: number
}

export interface RiskUserView {
  user_id: number
  email: string
  score: number
  tier: RiskTier
  allowlisted: boolean
  manual_tier: string | null
  features: RiskUserFeatures
  window: RiskUserWindow
  would_do_action: RiskWouldDoAction
  updated_at: string
}

export interface RiskFeatureWeights {
  f1: number
  f2: number
  f3: number
  f4: number
  f5: number
  f6: number
  f7: number
}

export type RiskFeatureThresholds = RiskFeatureWeights

export interface RiskConfig {
  mode: RiskMode
  medium_score: number
  high_score: number
  volume_floor: number
  and_gate_k: number
  daily_budget_micros: number
  weekly_budget_micros: number
  weights: RiskFeatureWeights
  thresholds: RiskFeatureThresholds
}

export type UpdateRiskConfigPayload = Partial<{
  mode: RiskMode
  medium_score: number
  high_score: number
  volume_floor: number
  and_gate_k: number
  daily_budget_micros: number
  weekly_budget_micros: number
  weights: RiskFeatureWeights
  thresholds: RiskFeatureThresholds
}>

export interface ListRiskUsersParams {
  tier?: RiskTier
  limit?: number
}

export interface AllowlistResponse {
  user_id: number
  allowlisted: boolean
}

export interface ManualTierResponse {
  user_id: number
  manual_tier: string | null
}

export const riskAPI = {
  async listUsers(params: ListRiskUsersParams = {}): Promise<RiskUserView[]> {
    const { data } = await apiClient.get<{ items: RiskUserView[] }>('/admin/risk/users', {
      params,
    })
    return data.items ?? []
  },

  async getUser(id: number): Promise<RiskUserView> {
    const { data } = await apiClient.get<RiskUserView>(`/admin/risk/users/${id}`)
    return data
  },

  async setAllowlist(id: number, on: boolean): Promise<AllowlistResponse> {
    const { data } = await apiClient.post<AllowlistResponse>(`/admin/risk/users/${id}/allowlist`, {
      on,
    })
    return data
  },

  async setManualTier(id: number, tier: RiskTier | null): Promise<ManualTierResponse> {
    const { data } = await apiClient.post<ManualTierResponse>(
      `/admin/risk/users/${id}/manual-tier`,
      { tier },
    )
    return data
  },

  async getConfig(): Promise<RiskConfig> {
    const { data } = await apiClient.get<RiskConfig>('/admin/risk/config')
    return data
  },

  async updateConfig(payload: UpdateRiskConfigPayload): Promise<RiskConfig> {
    const { data } = await apiClient.patch<RiskConfig>('/admin/risk/config', payload)
    return data
  },
}

export default riskAPI
