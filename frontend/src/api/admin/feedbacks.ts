/**
 * Admin Feedback API endpoints
 */

import { apiClient } from '../client'
import type { BasePaginationResponse, Feedback } from '@/types'

export async function list(
  page: number = 1,
  pageSize: number = 20,
  filters?: {
    status?: string
    sort_by?: string
    sort_order?: 'asc' | 'desc'
  },
  options?: {
    signal?: AbortSignal
  },
): Promise<BasePaginationResponse<Feedback>> {
  const { data } = await apiClient.get<BasePaginationResponse<Feedback>>('/admin/feedbacks', {
    params: { page, page_size: pageSize, ...filters },
    signal: options?.signal,
  })
  return data
}

export async function getById(id: number): Promise<Feedback> {
  const { data } = await apiClient.get<Feedback>(`/admin/feedbacks/${id}`)
  return data
}

export async function updateStatus(id: number, status: string): Promise<Feedback> {
  const { data } = await apiClient.put<Feedback>(`/admin/feedbacks/${id}/status`, { status })
  return data
}

export async function reply(id: number, replyText: string): Promise<Feedback> {
  const { data } = await apiClient.put<Feedback>(`/admin/feedbacks/${id}/reply`, { reply: replyText })
  return data
}

const feedbacksAPI = {
  list,
  getById,
  updateStatus,
  reply,
}

export default feedbacksAPI
