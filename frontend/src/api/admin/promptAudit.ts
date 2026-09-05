/**
 * Admin Prompt Audit API — 提示词审计：留存用户提示词原文，供事后审计/取证。
 *
 * 与风控中心（内容审核 risk-control）完全解耦：内容审核只存 240 字摘要用于风控判定，
 * 本模块按需留存提示词全文。独立开关 + 独立保留期。默认关闭。
 * 后端 config/status 返回裸业务负载；events 走标准分页信封（apiClient 拦截器已拆封 data）。
 */

import { apiClient } from '../client'

export interface PromptAuditConfig {
  enabled: boolean
  retention_days: number
}

export interface PromptAuditStatus {
  enabled: boolean
  retention_days: number
  queue_length: number
  queue_capacity: number
  stored: number
  dropped: number
}

export interface PromptAuditEvent {
  id: number
  request_id: string
  user_id: number | null
  user_email: string
  api_key_id: number | null
  api_key_name: string
  group_id: number | null
  group_name: string
  provider: string
  endpoint: string
  protocol: string
  model: string
  prompt_hash: string
  prompt_length: number
  message_count: number
  full_prompt: string // 仅详情接口返回；列表为空串
  user_status: string
  created_at: string
}

export interface ListPromptAuditEventsParams {
  page?: number
  page_size?: number
  search?: string
  group_id?: number
  api_key_id?: number
  user_id?: number
  from?: string
  to?: string
}

export interface PromptAuditEventsResponse {
  items: PromptAuditEvent[]
  total: number
  page: number
  page_size: number
  pages: number
}

export async function getConfig(): Promise<PromptAuditConfig> {
  const { data } = await apiClient.get<PromptAuditConfig>('/admin/prompt-audit/config')
  return data
}

export async function updateConfig(payload: Partial<PromptAuditConfig>): Promise<PromptAuditConfig> {
  const { data } = await apiClient.put<PromptAuditConfig>('/admin/prompt-audit/config', payload)
  return data
}

export async function getStatus(): Promise<PromptAuditStatus> {
  const { data } = await apiClient.get<PromptAuditStatus>('/admin/prompt-audit/status')
  return data
}

export async function listEvents(
  params: ListPromptAuditEventsParams = {}
): Promise<PromptAuditEventsResponse> {
  const { data } = await apiClient.get<PromptAuditEventsResponse>('/admin/prompt-audit/events', {
    params,
  })
  return data
}

export async function getEvent(id: number): Promise<PromptAuditEvent> {
  const { data } = await apiClient.get<PromptAuditEvent>(`/admin/prompt-audit/events/${id}`)
  return data
}

export async function deleteEvent(id: number): Promise<{ deleted: boolean }> {
  const { data } = await apiClient.delete<{ deleted: boolean }>(`/admin/prompt-audit/events/${id}`)
  return data
}

export async function deleteAll(): Promise<{ deleted: number }> {
  const { data } = await apiClient.delete<{ deleted: number }>('/admin/prompt-audit/events')
  return data
}

export const promptAuditAPI = {
  getConfig,
  updateConfig,
  getStatus,
  listEvents,
  getEvent,
  deleteEvent,
  deleteAll,
}

export default promptAuditAPI
