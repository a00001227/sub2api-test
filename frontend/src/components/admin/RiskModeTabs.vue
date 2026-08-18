<template>
  <!--
    Shared Legacy / Risk V2 Shadow switcher.
    Rendered identically at the top of RiskView.vue (Legacy) and
    RiskV2ShadowView.vue (V2). It is navigation-only — it owns no business
    state and never touches either page's data. Keyboard accessible: the tabs
    are real <a> links (Tab-focusable, Enter-activatable) with roving arrow-key
    navigation and tablist/tab ARIA semantics.
  -->
  <nav
    class="mb-6 flex gap-2 border-b border-gray-100 dark:border-dark-700"
    role="tablist"
    :aria-label="t('admin.riskTabs.ariaLabel')"
  >
    <router-link
      to="/admin/risk"
      role="tab"
      data-test="tab-legacy"
      :aria-selected="active === 'legacy'"
      :aria-current="active === 'legacy' ? 'page' : undefined"
      :tabindex="active === 'legacy' ? 0 : -1"
      :class="active === 'legacy' ? activeClass : idleClass"
      @keydown="onKeydown"
    >{{ t('admin.riskTabs.legacy') }}</router-link>
    <router-link
      to="/admin/risk-v2"
      role="tab"
      data-test="tab-v2"
      :aria-selected="active === 'v2'"
      :aria-current="active === 'v2' ? 'page' : undefined"
      :tabindex="active === 'v2' ? 0 : -1"
      :class="active === 'v2' ? activeClass : idleClass"
      @keydown="onKeydown"
    >{{ t('admin.riskTabs.v2') }}</router-link>
  </nav>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

const props = defineProps<{ active: 'legacy' | 'v2' }>()

const { t } = useI18n()
const router = useRouter()

const activeClass =
  'inline-flex whitespace-nowrap rounded-t-lg bg-primary-50 px-3 py-2 text-sm font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300'
const idleClass =
  'inline-flex whitespace-nowrap rounded-t-lg px-3 py-2 text-sm font-medium text-gray-500 hover:bg-gray-50 hover:text-gray-900 dark:text-gray-400 dark:hover:bg-dark-700 dark:hover:text-white'

// Roving tab navigation: Left/Right (and Home/End) move between the two tabs.
function onKeydown(e: KeyboardEvent) {
  const target = props.active === 'legacy' ? '/admin/risk-v2' : '/admin/risk'
  if (e.key === 'ArrowRight' || e.key === 'ArrowLeft' || e.key === 'Home' || e.key === 'End') {
    e.preventDefault()
    // Home → legacy, End → v2, arrows → the other tab.
    if (e.key === 'Home') router.push('/admin/risk')
    else if (e.key === 'End') router.push('/admin/risk-v2')
    else router.push(target)
  }
}
</script>
