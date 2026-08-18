import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import RiskView from '../RiskView.vue'
import en from '@/i18n/locales/en'

// §十一.38 — Legacy Risk page regression: adding the shared RiskModeTabs shell
// must not disturb the legacy view's data, table, filters or business logic.
const { listUsers, getConfig, showError } = vi.hoisted(() => ({
  listUsers: vi.fn(),
  getConfig: vi.fn(),
  showError: vi.fn(),
}))
const i18nState = vi.hoisted(() => ({ locale: 'en' as 'en' | 'zh' }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  const { makeUseI18n } = await import('@/__tests__/riskV2I18nHelper')
  return { ...actual, useI18n: makeUseI18n(i18nState) }
})

vi.mock('@/api/admin/risk', () => {
  const riskAPI = {
    listUsers,
    getConfig,
    getUser: vi.fn(),
    updateConfig: vi.fn(),
    setAllowlist: vi.fn(),
    setManualTier: vi.fn(),
  }
  return { riskAPI, default: riskAPI }
})
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn() }),
}))

function mountLegacy() {
  return mount(RiskView, {
    global: {
      stubs: { AppLayout: { template: '<div><slot /></div>' }, Icon: true, RiskModeTabs: true },
    },
  })
}

describe('Legacy RiskView still works with the shared tab shell', () => {
  beforeEach(() => {
    listUsers.mockReset(); getConfig.mockReset(); showError.mockReset()
    listUsers.mockResolvedValue([])
    getConfig.mockResolvedValue({
      mode: 'observe', medium_score: 40, high_score: 70, volume_floor: 100, and_gate_k: 2,
      daily_budget_micros: 0, weekly_budget_micros: 0, weights: {}, thresholds: {},
    })
  })

  it('renders the legacy title and still loads its own data', async () => {
    const w = mountLegacy()
    await flushPromises()
    expect(w.text()).toContain(en.admin.risk.title)
    expect(listUsers).toHaveBeenCalled()
    expect(getConfig).toHaveBeenCalled()
    expect(showError).not.toHaveBeenCalled()
    // The shared switcher is mounted (stubbed) without disturbing the page.
    expect(w.findComponent({ name: 'RiskModeTabs' }).exists()).toBe(true)
  })
})
