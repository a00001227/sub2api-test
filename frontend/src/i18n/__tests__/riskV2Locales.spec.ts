import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

// 切片 6：Risk V2 影子后台 —— 双语 key parity + 插值一致 + 关键 shadow 语义 + 映射表完整。
describe('risk v2 shadow locale copy', () => {
  function keys(obj: Record<string, unknown>, prefix = ''): string[] {
    const out: string[] = []
    for (const [k, v] of Object.entries(obj)) {
      const path = prefix ? `${prefix}.${k}` : k
      if (v && typeof v === 'object') out.push(...keys(v as Record<string, unknown>, path))
      else out.push(path)
    }
    return out
  }

  function flat(obj: Record<string, unknown>, prefix = ''): Record<string, string> {
    const out: Record<string, string> = {}
    for (const [k, v] of Object.entries(obj)) {
      const path = prefix ? `${prefix}.${k}` : k
      if (v && typeof v === 'object') Object.assign(out, flat(v as Record<string, unknown>, path))
      else out[path] = String(v)
    }
    return out
  }

  function tokens(s: string): string[] {
    return (s.match(/\{(\w+)\}/g) || []).sort()
  }

  it('en and zh admin.riskV2 have identical key sets (bilingual parity)', () => {
    const enKeys = keys(en.admin.riskV2 as Record<string, unknown>).sort()
    const zhKeys = keys(zh.admin.riskV2 as Record<string, unknown>).sort()
    expect(zhKeys).toEqual(enKeys)
  })

  it('interpolation variables match across locales for every riskV2 key', () => {
    const enFlat = flat(en.admin.riskV2 as Record<string, unknown>)
    const zhFlat = flat(zh.admin.riskV2 as Record<string, unknown>)
    for (const key of Object.keys(enFlat)) {
      expect(tokens(zhFlat[key] ?? ''), `interpolation mismatch on ${key}`).toEqual(tokens(enFlat[key]))
    }
  })

  it('exposes the four risk tiers in both locales', () => {
    for (const loc of [en, zh]) {
      const tier = (loc.admin.riskV2 as any).tier
      expect(Object.keys(tier).sort()).toEqual(['HIGH', 'INSUFFICIENT_DATA', 'MEDIUM', 'WATCH'])
    }
  })

  it('provides human-readable mapping tables for reasons/families/groups/incomplete/apiError', () => {
    for (const loc of [en, zh]) {
      const rv2 = loc.admin.riskV2 as any
      for (const table of ['reason', 'family', 'group', 'incomplete', 'apiError']) {
        expect(rv2[table], `missing ${table} table`).toBeTruthy()
        expect(Object.keys(rv2[table]).length, `empty ${table} table`).toBeGreaterThan(0)
      }
    }
  })

  it('maps the backend evidence families and groups exactly', () => {
    const families = [
      'TEMPLATE_ENUMERATION',
      'TEMPORAL_AUTOMATION',
      'OUTPUT_COLLECTION',
      'REPEATED_SAMPLING',
      'MULTI_KEY_COORDINATION',
      'CACHE_SESSION_CONTINUITY',
      'MODEL_CONCENTRATION',
    ]
    const groups = ['PROMPT_PATTERN', 'TEMPORAL_INTENSITY', 'RESPONSE_HARVEST', 'COORDINATION', 'MODEL']
    for (const loc of [en, zh]) {
      const rv2 = loc.admin.riskV2 as any
      for (const f of families) expect(rv2.family[f], `family ${f}`).toBeTruthy()
      for (const g of groups) expect(rv2.group[g], `group ${g}`).toBeTruthy()
    }
  })

  it('maps the live-metrics API error codes in both locales', () => {
    for (const loc of [en, zh]) {
      const api = (loc.admin.riskV2 as any).apiError
      expect(api.RISK_V2_LIVE_METRICS_RATE_LIMITED).toBeTruthy()
      expect(api.RISK_V2_LIVE_METRICS_BUSY).toBeTruthy()
      expect(api.RISK_V2_LIVE_METRICS_UNAVAILABLE).toBeTruthy()
      expect(api.RISK_V2_SCHEMA_NOT_READY).toBeTruthy()
    }
  })

  it('nav.riskV2Shadow exists in both locales', () => {
    expect((en.nav as any).riskV2Shadow).toBeTruthy()
    expect((zh.nav as any).riskV2Shadow).toBeTruthy()
  })

  it('shadow copy states risk index is not a probability', () => {
    expect((en.admin.riskV2 as any).shadowNote1.toLowerCase()).toContain('not a probability')
    expect((zh.admin.riskV2 as any).shadowNote1).toContain('不是概率')
  })

  it('dry-run empty copy never claims all users are low risk', () => {
    expect((en.admin.riskV2 as any).dryRunEmptyDesc.toLowerCase()).toContain('dry run')
    expect((zh.admin.riskV2 as any).dryRunEmptyDesc).toContain('Dry Run')
    expect((zh.admin.riskV2 as any).dryRunEmptyDesc).toContain('尚未持久化')
  })

  it('reason copy avoids "proven distillation / violation / legal" wording', () => {
    for (const loc of [en, zh]) {
      const reason = (loc.admin.riskV2 as any).reason as Record<string, string>
      for (const v of Object.values(reason)) {
        expect(v.toLowerCase()).not.toContain('proven')
        expect(v).not.toContain('已证明')
        expect(v).not.toContain('违规')
      }
    }
  })

  it('pageLabel keeps the {page} interpolation in both locales', () => {
    expect((en.admin.riskV2 as any).pageLabel).toContain('{page}')
    expect((zh.admin.riskV2 as any).pageLabel).toContain('{page}')
  })

  it('liveCooldown keeps the {sec} interpolation in both locales', () => {
    expect((en.admin.riskV2 as any).liveCooldown).toContain('{sec}')
    expect((zh.admin.riskV2 as any).liveCooldown).toContain('{sec}')
  })
})
