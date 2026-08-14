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

export type CatalogPlatform = 'anthropic' | 'openai' | 'gemini' | 'other'

/** One official-catalog entry with official prices as USD floats. */
export interface CatalogEntry {
  model: string
  model_type: ModelType
  platform: CatalogPlatform
  input_price: number
  output_price: number
  cache_read_price: number
  cache_write_price: number
  image_price: number
  added: boolean
}

export interface CatalogListResponse {
  models: CatalogEntry[]
  count: number
}

export interface CatalogSyncResult {
  total: number
}

export interface CreateFromCatalogResult {
  created: number
  skipped: number
  errors?: string[]
}

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

  // ---- Official catalog flow ----

  async listCatalog(platform?: string): Promise<CatalogListResponse> {
    const { data } = await apiClient.get<CatalogListResponse>('/admin/pricing/models/catalog', {
      params: platform ? { platform } : undefined,
    })
    return data
  },

  async syncCatalog(): Promise<CatalogSyncResult> {
    const { data } = await apiClient.post<CatalogSyncResult>('/admin/pricing/models/catalog/sync')
    return data
  },

  async createFromCatalog(models: string[]): Promise<CreateFromCatalogResult> {
    const { data } = await apiClient.post<CreateFromCatalogResult>(
      '/admin/pricing/models/from-catalog',
      { models },
    )
    return data
  },
}
