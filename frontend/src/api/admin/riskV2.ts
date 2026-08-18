/**
 * Admin Risk V2 Shadow (model-extraction risk) API — READ ONLY.
 *
 * Shadow only: these endpoints surface runtime status, latest assessments and
 * live window summaries. Nothing is ever enforced (EffectiveAction === 'NONE').
 * The `apiClient` response interceptor passes non-enveloped payloads through
 * as-is, so each call returns the raw response DTO; errors carry a stable
 * string `code` (e.g. RISK_V2_SCHEMA_NOT_READY) on the rejected value.
 */

import { apiClient } from '../client'

export type RiskV2Tier = 'INSUFFICIENT_DATA' | 'WATCH' | 'MEDIUM' | 'HIGH'

/** Meta flags echoed on every response — risk_index is NOT a probability. */
export interface RiskV2Meta {
  shadow: boolean
  enforcement: boolean
  risk_index_is_probability: boolean
}

export interface RiskV2RuntimeStatus {
  available: boolean
  shadow: boolean
  enabled: boolean
  aggregation_enabled: boolean
  scoring_enabled: boolean
  persist_enabled: boolean
  dispatcher_ready: boolean
  reporter_ready: boolean
  worker_ready: boolean
  worker_degraded: boolean
  schema_ready: boolean
  leader: boolean
  current_cycle: number
  last_completed_cycle: number
  last_cycle_status: string
  queue_depth: number
  observation_drop_ratio: number
  health_available: boolean
  health_coverage_ratio: number
  last_error_code: string
  last_updated_at: number
  meta: RiskV2Meta
}

export interface RiskV2UserSummary {
  user_id: number
  email: string
  username: string
  status: string
  known: boolean
}

export interface RiskV2ReasonCode {
  code: string
  window: string
  observed_value: number
  threshold: number
  evidence_family: string
  evidence_group: string
  confidence_contribution: number
}

/** Plane score. `score` is null when unavailable — never render as 0. */
export interface RiskV2Plane {
  score: number | null
  available: boolean
}

export interface RiskV2AssessmentListItem {
  user: RiskV2UserSummary
  user_id: number
  risk_index: number
  risk_tier: RiskV2Tier
  confidence: number
  data_sufficient: boolean
  automation: RiskV2Plane
  harvest: RiskV2Plane
  campaign: RiskV2Plane
  exposure: RiskV2Plane
  degraded: boolean
  incomplete: boolean
  health_available: boolean
  top_reason_codes: RiskV2ReasonCode[]
  assessed_at: number
  updated_at: number
  fresh: boolean
  age_seconds: number
  time_anomaly: boolean
  feature_version: string
  policy_version: string
  fingerprint_key_version: string
  effective_action: string
}

export interface RiskV2AssessmentListResponse {
  items: RiskV2AssessmentListItem[]
  limit: number
  offset: number
  has_more: boolean
  next_offset: number
  stale_after_seconds: number
  meta: RiskV2Meta
}

export interface RiskV2EvidenceFamily {
  family: string
  group: string
  available: boolean
  strength: number
  meets_high: boolean
  window: string
}

export interface RiskV2EvidenceGroup {
  group: string
  met_high: boolean
  strength: number
}

export interface RiskV2FeatureAvailability {
  requests: boolean
  fingerprint: boolean
  cache: boolean
  active_minutes: boolean
  near_duplicate: boolean
  tool_use: boolean
  structured_output: boolean
  model_concentration: boolean
}

export interface RiskV2AssessmentDetail {
  user: RiskV2UserSummary
  user_id: number
  risk_index: number
  risk_tier: RiskV2Tier
  confidence: number
  data_sufficient: boolean
  automation: RiskV2Plane
  harvest: RiskV2Plane
  campaign: RiskV2Plane
  exposure: RiskV2Plane
  feature_availability: RiskV2FeatureAvailability
  evidence_families: RiskV2EvidenceFamily[]
  evidence_groups: RiskV2EvidenceGroup[]
  reason_codes: RiskV2ReasonCode[]
  incomplete_reasons: string[]
  degraded: boolean
  incomplete: boolean
  health_available: boolean
  assessed_at: number
  fresh: boolean
  age_seconds: number
  time_anomaly: boolean
  stale_after_seconds: number
  feature_version: string
  policy_version: string
  fingerprint_key_version: string
  effective_action: string
  meta: RiskV2Meta
}

export interface RiskV2Window {
  window: string
  available: boolean
  request_count: number
  success_count: number
  active_minutes: number
  peak_rpm: number
  input_tokens: number
  output_tokens: number
  distinct_exact_estimate: number
  distinct_full_scaffold_estimate: number
  unique_api_key_count: number
}

export interface RiskV2MultiKey {
  multi_key_available: boolean
  active_api_key_count_24h: number
  synchronized_multi_key_minutes_1h: number
  cross_key_full_scaffold_overlap_estimate_1h: number
  cross_key_full_scaffold_overlap_available_1h: boolean
  key_handoff_count_1h: number
  multi_key_incomplete: boolean
}

export interface RiskV2WindowSummary {
  user_id: number
  data_available: boolean
  w_5m: RiskV2Window
  w_1h: RiskV2Window
  w_24h: RiskV2Window
  multi_key: RiskV2MultiKey
  degraded: boolean
  incomplete: boolean
  incomplete_reasons: string[]
  assessed_at: number
  fingerprint_key_version: string
  meta: RiskV2Meta
}

/** Only send fields that have a value; never send NaN/Infinity/empty. */
export interface ListRiskV2UsersParams {
  tier?: RiskV2Tier
  min_risk_index?: number
  data_sufficient?: boolean
  degraded?: boolean
  user_id?: number
  assessed_from?: number
  assessed_to?: number
  freshness?: 'fresh' | 'stale'
  limit?: number
  offset?: number
}

/**
 * Per-call options. Only `signal` is supported — it maps straight to the axios
 * request `signal` so callers can cancel in-flight requests (race protection).
 * The shared response interceptor already re-rejects axios cancellations with
 * their original `ERR_CANCELED` error, so callers can detect them.
 */
export interface RiskV2RequestOptions {
  signal?: AbortSignal
}

export const riskV2API = {
  async getStatus(opts: RiskV2RequestOptions = {}): Promise<RiskV2RuntimeStatus> {
    const { data } = await apiClient.get<RiskV2RuntimeStatus>('/admin/risk/v2/status', {
      signal: opts.signal,
    })
    return data
  },

  async listUsers(
    params: ListRiskV2UsersParams = {},
    opts: RiskV2RequestOptions = {},
  ): Promise<RiskV2AssessmentListResponse> {
    const { data } = await apiClient.get<RiskV2AssessmentListResponse>('/admin/risk/v2/users', {
      params,
      signal: opts.signal,
    })
    return data
  },

  async getUser(id: number, opts: RiskV2RequestOptions = {}): Promise<RiskV2AssessmentDetail> {
    const { data } = await apiClient.get<RiskV2AssessmentDetail>(`/admin/risk/v2/users/${id}`, {
      signal: opts.signal,
    })
    return data
  },

  async getWindows(id: number, opts: RiskV2RequestOptions = {}): Promise<RiskV2WindowSummary> {
    const { data } = await apiClient.get<RiskV2WindowSummary>(`/admin/risk/v2/users/${id}/windows`, {
      signal: opts.signal,
    })
    return data
  },
}

export default riskV2API
