import { describe, expect, it, beforeEach, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createRouter, createMemoryHistory, type Router } from 'vue-router'
import { defineComponent } from 'vue'

import RiskModeTabs from '../RiskModeTabs.vue'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

const i18nState = vi.hoisted(() => ({ locale: 'en' as 'en' | 'zh' }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  const { makeUseI18n } = await import('@/__tests__/riskV2I18nHelper')
  return { ...actual, useI18n: makeUseI18n(i18nState) }
})

const Stub = defineComponent({ template: '<div />' })

function makeRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/admin/risk', name: 'AdminRisk', component: Stub },
      { path: '/admin/risk-v2', name: 'AdminRiskV2Shadow', component: Stub },
    ],
  })
}

function mountTabs(active: 'legacy' | 'v2', locale: 'en' | 'zh' = 'en') {
  i18nState.locale = locale
  const router = makeRouter()
  const wrapper = mount(RiskModeTabs, {
    props: { active },
    global: { plugins: [router] },
  })
  return { wrapper, router }
}

describe('RiskModeTabs (shared Legacy / V2 switcher)', () => {
  beforeEach(() => {
    localStorage.clear()
    i18nState.locale = 'en'
  })

  it('renders both tabs identically regardless of active side (symmetry)', () => {
    const legacy = mountTabs('legacy').wrapper
    const v2 = mountTabs('v2').wrapper
    for (const w of [legacy, v2]) {
      expect(w.get('[data-test="tab-legacy"]').text()).toBe(en.admin.riskTabs.legacy)
      expect(w.get('[data-test="tab-v2"]').text()).toBe(en.admin.riskTabs.v2)
    }
  })

  it('uses tablist/tab roles and marks the active tab with aria-selected + aria-current', () => {
    const { wrapper } = mountTabs('v2')
    expect(wrapper.get('[role="tablist"]').exists()).toBe(true)
    const legacy = wrapper.get('[data-test="tab-legacy"]')
    const v2 = wrapper.get('[data-test="tab-v2"]')
    expect(legacy.attributes('role')).toBe('tab')
    expect(v2.attributes('aria-selected')).toBe('true')
    expect(v2.attributes('aria-current')).toBe('page')
    expect(legacy.attributes('aria-selected')).toBe('false')
    expect(legacy.attributes('aria-current')).toBeUndefined()
  })

  it('is keyboard reachable: active tab tabindex 0, inactive -1 (roving)', () => {
    const { wrapper } = mountTabs('legacy')
    expect(wrapper.get('[data-test="tab-legacy"]').attributes('tabindex')).toBe('0')
    expect(wrapper.get('[data-test="tab-v2"]').attributes('tabindex')).toBe('-1')
  })

  it('links to the correct routes', () => {
    const { wrapper } = mountTabs('legacy')
    expect(wrapper.get('[data-test="tab-legacy"]').attributes('href')).toContain('/admin/risk')
    expect(wrapper.get('[data-test="tab-v2"]').attributes('href')).toContain('/admin/risk-v2')
  })

  it('ArrowRight from the legacy tab navigates to the v2 route (keyboard)', async () => {
    const { wrapper, router } = mountTabs('legacy')
    await router.push('/admin/risk')
    await router.isReady()
    await wrapper.get('[data-test="tab-legacy"]').trigger('keydown', { key: 'ArrowRight' })
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/admin/risk-v2')
  })

  it('renders localized labels in Chinese', () => {
    const { wrapper } = mountTabs('v2', 'zh')
    expect(wrapper.get('[data-test="tab-legacy"]').text()).toBe(zh.admin.riskTabs.legacy)
    expect(wrapper.get('[data-test="tab-v2"]').text()).toBe(zh.admin.riskTabs.v2)
  })
})
