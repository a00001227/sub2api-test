/**
 * Admin Pricing Models API
 * Manages the unified pricing display system.
 */

import { apiClient } from '../client'

export type ModelType = 'text' | 'image'
export type UserType = 'end_user' | 'channel_user'

export interface PricingModelRecord {
  id: number
  model: string
  model_type: ModelType
  user_type: UserType
  enabled: boolean

  // Text model fields
  input_price: number | null
  output_price: number | null
  cache_read_price: number | null
  cache_write_price: number | null
  official_input_price: number | null
  official_output_price: number | null

  // Image model field
  image_resolutions: Record<string, number> | null

  // Computed by backend
  saving_percent: number
  updated_at: string
}

export interface CreatePricingModelPayload {
  model: string
  model_type: ModelType
  user_type: UserType
  enabled?: boolean

  input_price?: number | null
  output_price?: number | null
  cache_read_price?: number | null
  cache_write_price?: number | null
  official_input_price?: number | null
  official_output_price?: number | null

  image_resolutions?: Record<string, number>
  saving_percent?: number
}

export type UpdatePricingModelPayload = Partial<CreatePricingModelPayload>

export const pricingApi = {
  async listModels(): Promise<PricingModelRecord[]> {
    const { data } = await apiClient.get<PricingModelRecord[]>('/admin/pricing/models')
    return data
  },

  async getModel(id: number): Promise<PricingModelRecord> {
    const { data } = await apiClient.get<PricingModelRecord>(`/admin/pricing/models/${id}`)
    return data
  },

  async createModel(payload: CreatePricingModelPayload): Promise<PricingModelRecord> {
    const { data } = await apiClient.post<PricingModelRecord>('/admin/pricing/models', payload)
    return data
  },

  async updateModel(id: number, payload: UpdatePricingModelPayload): Promise<PricingModelRecord> {
    const { data } = await apiClient.put<PricingModelRecord>(`/admin/pricing/models/${id}`, payload)
    return data
  },

  async deleteModel(id: number): Promise<void> {
    await apiClient.delete(`/admin/pricing/models/${id}`)
  },
}
