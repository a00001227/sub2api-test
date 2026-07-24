/**
 * Admin Regions (egress-region dictionary) API endpoints.
 */

import { apiClient } from '../client'
import type { Region, CreateRegionRequest, UpdateRegionRequest } from '@/types'

export async function list(enabledOnly = false): Promise<Region[]> {
  const { data } = await apiClient.get<{ regions: Region[] }>('/admin/regions', {
    params: enabledOnly ? { enabled_only: 1 } : undefined
  })
  return data.regions ?? []
}

export async function create(request: CreateRegionRequest): Promise<Region> {
  const { data } = await apiClient.post<Region>('/admin/regions', request)
  return data
}

export async function update(id: number, request: UpdateRegionRequest): Promise<Region> {
  const { data } = await apiClient.put<Region>(`/admin/regions/${id}`, request)
  return data
}

export async function remove(id: number): Promise<{ deleted: boolean }> {
  const { data } = await apiClient.delete<{ deleted: boolean }>(`/admin/regions/${id}`)
  return data
}

const regionsAPI = {
  list,
  create,
  update,
  remove
}

export default regionsAPI

