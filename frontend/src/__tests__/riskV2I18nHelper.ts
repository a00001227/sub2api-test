/**
 * Test-only i18n translator backed by the REAL en/zh locale objects.
 *
 * Risk V2 view specs mock `vue-i18n` (repo convention — see RiskControlView.spec)
 * rather than installing the global plugin, so `useI18n()` scope quirks can't make
 * assertions flaky. This translator resolves against the actual message files, so
 * a test asserting `en.admin.riskV2.foo` matches exactly what the component renders,
 * and unknown keys fall through to the raw key (which is how we verify the view's
 * unknown-code fallback and "no leaked i18n keys" guarantees).
 *
 * Not a spec file (no `.spec`/`.test` suffix) so Vitest never collects it as tests.
 */
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

type Msgs = Record<string, any>

function get(msgs: Msgs, key: string): unknown {
  return key.split('.').reduce<any>((o, k) => (o == null ? undefined : o[k]), msgs)
}

export function makeUseI18n(state: { locale: 'en' | 'zh' }) {
  return () => ({
    locale: { value: state.locale },
    t: (key: string, params?: Record<string, unknown>) => {
      const v = get(state.locale === 'zh' ? zh : en, key)
      if (typeof v !== 'string') return key
      return params
        ? v.replace(/\{(\w+)\}/g, (_m, name) => (params[name] != null ? String(params[name]) : `{${name}}`))
        : v
    },
    te: (key: string) => typeof get(state.locale === 'zh' ? zh : en, key) === 'string',
  })
}
