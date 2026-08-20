import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import RiskV2ShadowView from '../RiskV2ShadowView.vue'
import { riskV2API } from '@/api/admin/riskV2'
import en from '@/i18n/locales/en'

const { getStatus, listUsers, getUser, getWindows } = vi.hoisted(() => ({
  getStatus: vi.fn(),
  listUsers: vi.fn(),
  getUser: vi.fn(),
  getWindows: vi.fn(),
}))
const i18nState = vi.hoisted(() => ({ locale: 'en' as 'en' | 'zh' }))

vi.mock('@/api/admin/riskV2', () => {
  const api = { getStatus, listUsers, getUser, getWindows }
  return { riskV2API: api, default: api }
})

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  const { makeUseI18n } = await import('@/__tests__/riskV2I18nHelper')
  return { ...actual, useI18n: makeUseI18n(i18nState) }
})

// ---- factories ------------------------------------------------------------
const meta = { shadow: true, enforcement: false, risk_index_is_probability: false }

function statusFactory(o: Record<string, unknown> = {}) {
  return {
    available: true, shadow: true, enabled: true,
    aggregation_enabled: true, scoring_enabled: true, persist_enabled: true,
    dispatcher_ready: true, reporter_ready: true, worker_ready: true, worker_degraded: false,
    schema_ready: true, leader: true, current_cycle: 0, last_completed_cycle: 0,
    last_cycle_status: 'ok', queue_depth: 0, observation_drop_ratio: 0,
    health_available: true, health_coverage_ratio: 1, last_error_code: '', last_updated_at: 0,
    meta, ...o,
  }
}
function itemFactory(o: Record<string, unknown> = {}) {
  return {
    user: { user_id: 1, email: 'user1@example.com', username: 'u1', status: 'active', known: true },
    user_id: 1, risk_index: 82.5, risk_tier: 'HIGH', confidence: 0.91, data_sufficient: true,
    automation: { score: 70, available: true }, harvest: { score: null, available: false },
    campaign: { score: 12, available: true }, exposure: { score: 5, available: true },
    degraded: false, incomplete: false, health_available: true,
    top_reason_codes: [{ code: 'EXACT_DUPLICATION_HIGH', window: '1h', observed_value: 0.9, threshold: 0.7, evidence_family: 'TEMPLATE_ENUMERATION', evidence_group: 'PROMPT_PATTERN', confidence_contribution: 0 }],
    assessed_at: 1700000000, updated_at: 1700000000, fresh: true, age_seconds: 10, time_anomaly: false,
    feature_version: 'f1', policy_version: 'p1', fingerprint_key_version: 'k1', effective_action: 'NONE',
    ...o,
  }
}
function detailFactory(o: Record<string, unknown> = {}) {
  return {
    ...itemFactory(),
    feature_availability: { requests: true, fingerprint: true, cache: false, active_minutes: true, near_duplicate: false, tool_use: true, structured_output: true, model_concentration: true },
    evidence_families: [{ family: 'TEMPLATE_ENUMERATION', group: 'PROMPT_PATTERN', available: true, strength: 8.2, meets_high: true, window: '1h' }],
    evidence_groups: [{ group: 'PROMPT_PATTERN', met_high: true, strength: 8.2 }],
    reason_codes: [{ code: 'FULL_SCAFFOLD_REUSE_HIGH', window: '24h', observed_value: 0.88, threshold: 0.6, evidence_family: 'TEMPLATE_ENUMERATION', evidence_group: 'PROMPT_PATTERN', confidence_contribution: 0.1 }],
    incomplete_reasons: ['redis_degraded', 'exact_incomplete:1h'],
    stale_after_seconds: 600, meta, ...o,
  }
}
function windowFactory(label: string, o: Record<string, unknown> = {}) {
  return {
    window: label, available: true, request_count: 5, success_count: 5, active_minutes: 3, peak_rpm: 2,
    input_tokens: 10, output_tokens: 20, distinct_exact_estimate: 1, distinct_full_scaffold_estimate: 2,
    unique_api_key_count: 1, ...o,
  }
}
function windowsFactory(o: Record<string, unknown> = {}) {
  return {
    user_id: 1, data_available: true,
    w_5m: windowFactory('5m'), w_1h: windowFactory('1h'), w_24h: windowFactory('24h'),
    multi_key: { multi_key_available: true, active_api_key_count_24h: 2, synchronized_multi_key_minutes_1h: 1, cross_key_full_scaffold_overlap_estimate_1h: 0, cross_key_full_scaffold_overlap_available_1h: true, key_handoff_count_1h: 0, multi_key_incomplete: false },
    degraded: false, incomplete: false, incomplete_reasons: [], assessed_at: 1700000000, fingerprint_key_version: 'k1', meta,
    ...o,
  }
}
function defer<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function mountView() {
  return mount(RiskV2ShadowView, {
    global: {
      stubs: { AppLayout: { template: '<div><slot /></div>' }, Icon: true, EvidenceCapturePanel: true, EnforcementPanel: true },
    },
  })
}

async function mountReady() {
  const w = mountView()
  await flushPromises()
  return w
}

describe('admin RiskV2ShadowView', () => {
  beforeEach(() => {
    getStatus.mockReset(); listUsers.mockReset(); getUser.mockReset(); getWindows.mockReset()
    getStatus.mockResolvedValue(statusFactory())
    listUsers.mockResolvedValue({ items: [itemFactory()], limit: 20, offset: 0, has_more: false, next_offset: 0, stale_after_seconds: 600, meta })
    getUser.mockResolvedValue(detailFactory())
    getWindows.mockResolvedValue(windowsFactory())
  })
  afterEach(() => { document.documentElement.classList.remove('dark'); i18nState.locale = 'en' })

  // §十一.22 — read-only surface
  it('exposes only read-only API methods (no execution/mutation surface)', () => {
    expect(Object.keys(riskV2API).sort()).toEqual(['getStatus', 'getUser', 'getWindows', 'listUsers'])
  })

  // §十一.4 — Runtime Disabled (neutral, not failure)
  it('shows a neutral disabled notice when Risk V2 is disabled', async () => {
    getStatus.mockResolvedValue(statusFactory({ enabled: false }))
    const w = await mountReady()
    expect(w.find('[data-test="status-disabled"]').exists()).toBe(true)
    expect(w.find('[data-test="mode-label"]').exists()).toBe(false)
  })

  // §十一.5/6/7 — mode labels
  it('renders Aggregation-only, Dry Run and Persist mode labels', async () => {
    getStatus.mockResolvedValue(statusFactory({ scoring_enabled: false }))
    let w = await mountReady()
    expect(w.get('[data-test="mode-label"]').text()).toBe(en.admin.riskV2.modeAggOnly)

    getStatus.mockResolvedValue(statusFactory({ persist_enabled: false }))
    w = await mountReady()
    expect(w.get('[data-test="mode-label"]').text()).toBe(en.admin.riskV2.modeDryRun)

    getStatus.mockResolvedValue(statusFactory())
    w = await mountReady()
    expect(w.get('[data-test="mode-label"]').text()).toBe(en.admin.riskV2.modePersist)
  })

  // §十一.8 — Schema Not Ready: status still visible, list area independent
  it('shows schema-not-ready in the list while runtime status stays visible', async () => {
    listUsers.mockRejectedValue({ status: 503, code: 'RISK_V2_SCHEMA_NOT_READY' })
    const w = await mountReady()
    expect(w.find('[data-test="schema-not-ready"]').exists()).toBe(true)
    expect(w.find('[data-test="mode-label"]').exists()).toBe(true) // status unaffected
  })

  // §十一.9 — List Loading
  it('shows a loading state while the list request is in flight', async () => {
    const d = defer<any>()
    listUsers.mockReturnValue(d.promise)
    const w = mountView()
    await flushPromises() // status resolves; list still pending
    expect(w.find('[data-test="list-loading"]').exists()).toBe(true)
    d.resolve({ items: [], has_more: false })
    await flushPromises()
    expect(w.find('[data-test="list-loading"]').exists()).toBe(false)
  })

  // §十一.10 — List Empty (persist mode → plain empty, not dry-run copy)
  it('shows the plain empty state in persist mode', async () => {
    listUsers.mockResolvedValue({ items: [], has_more: false })
    const w = await mountReady()
    expect(w.find('[data-test="list-empty"]').exists()).toBe(true)
    expect(w.find('[data-test="dry-run-empty"]').exists()).toBe(false)
  })

  // §六 / §十一.6 — Dry Run + empty must NOT read as "no risky users"
  it('shows dry-run empty copy (not a low-risk claim) in Dry Run + empty', async () => {
    getStatus.mockResolvedValue(statusFactory({ persist_enabled: false }))
    listUsers.mockResolvedValue({ items: [], has_more: false })
    const w = await mountReady()
    const box = w.get('[data-test="dry-run-empty"]')
    expect(box.text()).toContain(en.admin.riskV2.dryRunEmptyTitle)
    expect(w.find('[data-test="list-empty"]').exists()).toBe(false)
  })

  // §十一.11 — Pagination
  it('paginates with offset and disables prev on the first page', async () => {
    listUsers.mockResolvedValue({ items: [itemFactory()], has_more: true })
    const w = await mountReady()
    expect((w.get('[data-test="prev-btn"]').element as HTMLButtonElement).disabled).toBe(true)
    expect((w.get('[data-test="next-btn"]').element as HTMLButtonElement).disabled).toBe(false)
    await w.get('[data-test="next-btn"]').trigger('click')
    await flushPromises()
    expect(listUsers).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 20, limit: 20 }), expect.anything())
  })

  // §十一.12 — Filter validation blocks the request
  it('blocks the query and shows an error when the risk index is out of range', async () => {
    const w = await mountReady()
    listUsers.mockClear()
    await w.get('#rv2-min').setValue('999')
    await w.get('[data-test="query-btn"]').trigger('click')
    await flushPromises()
    expect(w.find('[data-test="filter-error"]').exists()).toBe(true)
    expect(listUsers).not.toHaveBeenCalled()
  })

  // §十一.13 — Filter query mapping (only valid fields sent)
  it('maps valid filters to API params', async () => {
    const w = await mountReady()
    listUsers.mockClear()
    await w.get('#rv2-tier').setValue('HIGH')
    await w.get('#rv2-min').setValue('50')
    await w.get('#rv2-uid').setValue('7')
    await w.get('[data-test="query-btn"]').trigger('click')
    await flushPromises()
    expect(listUsers).toHaveBeenCalledWith(
      expect.objectContaining({ tier: 'HIGH', min_risk_index: 50, user_id: 7, offset: 0, limit: 20 }),
      expect.anything(),
    )
  })

  // §十一.14 — Nullable plane shows N/A, never 0
  it('renders unavailable planes as N/A rather than 0', async () => {
    const w = await mountReady()
    const row = w.get('[data-test="user-row"]')
    expect(row.text()).toContain('N/A') // harvest plane unavailable
    expect(row.text()).not.toMatch(/H:\s*0/)
  })

  // §十一.15 — HIGH tier visual + explanatory, keyboard reachable
  it('marks HIGH tier with a warning badge and a keyboard-reachable explanation', async () => {
    const w = await mountReady()
    const badge = w.get('[data-test="user-row"] span[aria-label]')
    expect(badge.attributes('aria-label')).toContain(en.admin.riskV2.highTooltip)
    expect(badge.attributes('tabindex')).toBe('0')
    expect(badge.classes().some((c) => c.includes('red'))).toBe(true)
  })

  // §十一.16/17 — fresh/stale + time anomaly
  it('renders stale and time-anomaly flags', async () => {
    listUsers.mockResolvedValue({ items: [itemFactory({ fresh: false, time_anomaly: true })], has_more: false })
    const w = await mountReady()
    const row = w.get('[data-test="user-row"]')
    expect(row.text()).toContain(en.admin.riskV2.stale)
    expect(row.text()).toContain(en.admin.riskV2.flagTimeAnomaly)
  })

  // §十一.20 — reason codes rendered human-readable + raw code
  it('renders human-readable reason names alongside the raw code', async () => {
    const w = await mountReady()
    const row = w.get('[data-test="user-row"]')
    expect(row.text()).toContain(en.admin.riskV2.reason.EXACT_DUPLICATION_HIGH)
    expect(row.find('span[title="EXACT_DUPLICATION_HIGH"]').exists()).toBe(true)
  })

  it('falls back to the raw code for an unknown reason code', async () => {
    listUsers.mockResolvedValue({ items: [itemFactory({ top_reason_codes: [{ code: 'BRAND_NEW_CODE_X', window: '1h', observed_value: 1, threshold: 1, evidence_family: '', evidence_group: '', confidence_contribution: 0 }] })], has_more: false })
    const w = await mountReady()
    expect(w.get('[data-test="user-row"]').text()).toContain('BRAND_NEW_CODE_X')
  })

  // §十一.18/21 — detail drawer + EffectiveAction NONE only
  it('opens the detail drawer on demand and shows EffectiveAction NONE', async () => {
    const w = await mountReady()
    expect(getWindows).not.toHaveBeenCalled()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    expect(getUser).toHaveBeenCalledWith(1, expect.anything())
    const drawer = w.get('[data-test="drawer"]')
    expect(drawer.attributes('role')).toBe('dialog')
    expect(drawer.attributes('aria-modal')).toBe('true')
    expect(w.get('[data-test="effective-action"]').text()).toContain('NONE')
    // reason + family human names present
    expect(drawer.text()).toContain(en.admin.riskV2.reason.FULL_SCAFFOLD_REUSE_HIGH)
    expect(drawer.text()).toContain(en.admin.riskV2.family.TEMPLATE_ENUMERATION)
  })

  // §十一.19 — Escape closes; close button has aria-label
  it('closes the drawer on Escape and exposes an aria-label on the close button', async () => {
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    expect(w.get('[data-test="drawer-close"]').attributes('aria-label')).toBe(en.admin.riskV2.ariaClose)
    await w.get('[data-test="drawer"]').trigger('keydown', { key: 'Escape' })
    await flushPromises()
    expect(w.find('[data-test="drawer"]').exists()).toBe(false)
  })

  // §十一.23 — Live not requested by default (list load or drawer open)
  it('never requests live windows automatically', async () => {
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    expect(getWindows).not.toHaveBeenCalled()
    expect(w.find('[data-test="live-region"]').exists()).toBe(false)
  })

  // §十一.24 — Live loads only on click
  it('loads live windows only after clicking the load button', async () => {
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    await w.get('[data-test="live-load-btn"]').trigger('click')
    await flushPromises()
    expect(getWindows).toHaveBeenCalledWith(1, expect.anything())
    expect(w.find('[data-test="live-region"]').exists()).toBe(true)
  })

  // §十一.25 — Refresh cooldown
  it('applies an 8s cooldown after loading live windows', async () => {
    vi.useFakeTimers()
    try {
      const w = await mountReady()
      await w.get('[data-test="view-detail-btn"]').trigger('click')
      await flushPromises()
      await w.get('[data-test="live-load-btn"]').trigger('click')
      await flushPromises()
      const btn = () => w.get('[data-test="live-load-btn"]').element as HTMLButtonElement
      expect(btn().disabled).toBe(true)
      expect(w.get('[data-test="live-load-btn"]').text()).toContain('8')
      vi.advanceTimersByTime(8000)
      await flushPromises()
      expect(btn().disabled).toBe(false)
    } finally {
      vi.useRealTimers()
    }
  })

  // §十一.26/27/28 — live error mapping
  it('maps live-metrics rate-limit / busy / unavailable errors', async () => {
    for (const [code, msg] of [
      ['RISK_V2_LIVE_METRICS_RATE_LIMITED', en.admin.riskV2.liveRateLimited],
      ['RISK_V2_LIVE_METRICS_BUSY', en.admin.riskV2.liveBusy],
      ['RISK_V2_LIVE_METRICS_UNAVAILABLE', en.admin.riskV2.liveUnavailable],
    ] as const) {
      getWindows.mockReset()
      getWindows.mockRejectedValue({ status: code.includes('UNAVAILABLE') ? 503 : 429, code })
      const w = await mountReady()
      await w.get('[data-test="view-detail-btn"]').trigger('click')
      await flushPromises()
      await w.get('[data-test="live-load-btn"]').trigger('click')
      await flushPromises()
      expect(w.get('[data-test="live-error"]').text()).toBe(msg)
    }
  })

  // §十一.29 — data_available=false: no zeroed dashboard
  it('shows a no-data notice (not a zeroed dashboard) when data_available is false', async () => {
    getWindows.mockResolvedValue(windowsFactory({ data_available: false }))
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    await w.get('[data-test="live-load-btn"]').trigger('click')
    await flushPromises()
    expect(w.find('[data-test="live-no-data"]').exists()).toBe(true)
    expect(w.find('[data-test="live-region"]').exists()).toBe(false)
  })

  // §十一.30 — live failure keeps the loaded detail
  it('keeps the loaded assessment detail when a live-window request fails', async () => {
    getWindows.mockRejectedValue({ status: 503, code: 'RISK_V2_LIVE_METRICS_UNAVAILABLE' })
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    await w.get('[data-test="live-load-btn"]').trigger('click')
    await flushPromises()
    expect(w.get('[data-test="live-error"]').exists()).toBe(true)
    expect(w.get('[data-test="effective-action"]').text()).toContain('NONE') // detail intact
  })

  // §十一.31 — never render sensitive identifiers
  it('never renders HMAC / SimHash / API key material', async () => {
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    await w.get('[data-test="live-load-btn"]').trigger('click')
    await flushPromises()
    const text = w.text().toLowerCase()
    expect(text).not.toContain('hmac')
    expect(text).not.toContain('simhash')
    expect(text).not.toContain('redis member')
  })

  // §十一.32 — no browser persistent storage writes
  it('does not write to localStorage or sessionStorage during the flow', async () => {
    const w = await mountReady()
    const lsSet = vi.spyOn(Storage.prototype, 'setItem')
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    await w.get('[data-test="live-load-btn"]').trigger('click')
    await flushPromises()
    expect(lsSet).not.toHaveBeenCalled()
    lsSet.mockRestore()
  })

  // §十一.33 — request cancellation on a new list request
  it('aborts the previous list request when a new one starts', async () => {
    const abortSpy = vi.spyOn(AbortController.prototype, 'abort')
    const w = await mountReady()
    abortSpy.mockClear()
    await w.get('[data-test="query-btn"]').trigger('click')
    await flushPromises()
    expect(abortSpy).toHaveBeenCalled()
    abortSpy.mockRestore()
  })

  // §十一.34 / §五 — stale response cannot overwrite a newer one (race).
  // List controls disable while loading, so the concurrent race is driven via the
  // exposed loadUsers seam: slow A starts, fast B supersedes and returns first,
  // then stale A resolves late and must be discarded.
  it('shows the newest result when an older list response arrives late', async () => {
    const w = await mountReady()
    const a = defer<any>()
    const b = defer<any>()
    listUsers.mockReturnValueOnce(a.promise) // request A (slow)
    ;(w.vm as any).loadUsers()
    listUsers.mockReturnValueOnce(b.promise) // request B (fast) supersedes A
    ;(w.vm as any).loadUsers()
    b.resolve({ items: [itemFactory({ user_id: 2, user: { user_id: 2, email: 'B@example.com', username: 'b', status: 'active', known: true } })], has_more: false })
    await flushPromises()
    a.resolve({ items: [itemFactory({ user_id: 1, user: { user_id: 1, email: 'A@example.com', username: 'a', status: 'active', known: true } })], has_more: false })
    await flushPromises()
    expect(w.text()).toContain('B@example.com')
    expect(w.text()).not.toContain('A@example.com')
  })

  // §十一.35 — dark + light base render
  it('renders in both light and dark base themes without error', async () => {
    const light = await mountReady()
    expect(light.find('[data-test="user-row"]').exists()).toBe(true)
    document.documentElement.classList.add('dark')
    const dark = await mountReady()
    expect(dark.find('[data-test="user-row"]').exists()).toBe(true)
  })

  // §十一.36 / §八.6 — no unresolved i18n keys leak into the UI
  it('does not leak raw admin.riskV2.* i18n keys into the rendered output', async () => {
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    await w.get('[data-test="live-load-btn"]').trigger('click')
    await flushPromises()
    expect(w.text()).not.toContain('admin.riskV2.')
    expect(w.text()).not.toContain('admin.riskTabs.')
  })

  // §五 — cancelled request is not surfaced as a business error
  it('ignores an axios cancellation instead of showing an error', async () => {
    listUsers.mockRejectedValue({ code: 'ERR_CANCELED', name: 'CanceledError' })
    const w = await mountReady()
    expect(w.find('[data-test="list-error"]').exists()).toBe(false)
  })

  // §六 — list error alert (role=alert) for a 500
  it('shows a role=alert list error for a server error', async () => {
    listUsers.mockRejectedValue({ status: 500, code: 'RISK_V2_INTERNAL' })
    const w = await mountReady()
    const alert = w.get('[data-test="list-error"]')
    expect(alert.attributes('role')).toBe('alert')
  })

  // §四 — explicit shadow limitations always shown in the live section
  it('states cache does not affect tier and near-duplicate is unavailable', async () => {
    const w = await mountReady()
    await w.get('[data-test="view-detail-btn"]').trigger('click')
    await flushPromises()
    const drawer = w.get('[data-test="drawer"]')
    expect(drawer.text()).toContain(en.admin.riskV2.liveCacheNote)
    expect(drawer.text()).toContain(en.admin.riskV2.liveNearDupNote)
  })
})
