<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-8">
      <!-- Header -->
      <div class="mb-6 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.risk.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.risk.description') }}</p>
        </div>
        <div class="flex items-center gap-3">
          <button @click="loadUsers" :disabled="loading" class="btn btn-secondary" :title="t('admin.risk.refresh')">
            <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
          </button>
        </div>
      </div>

      <!-- Shadow-mode banner (unmistakable) -->
      <div
        v-if="config"
        :class="isEnforce
          ? 'border-red-300 bg-red-50 dark:border-red-800 dark:bg-red-900/20'
          : 'border-amber-300 bg-amber-50 dark:border-amber-700 dark:bg-amber-900/20'"
        class="mb-6 flex items-start gap-3 rounded-lg border-2 px-4 py-3"
      >
        <Icon
          :name="isEnforce ? 'exclamationTriangle' : 'eye'"
          size="lg"
          :class="isEnforce ? 'mt-0.5 text-red-600 dark:text-red-400' : 'mt-0.5 text-amber-600 dark:text-amber-400'"
        />
        <div class="flex-1">
          <p
            :class="isEnforce ? 'text-red-800 dark:text-red-200' : 'text-amber-800 dark:text-amber-200'"
            class="text-sm font-bold"
          >
            {{ isEnforce ? t('admin.risk.enforceBannerTitle') : t('admin.risk.shadowBannerTitle') }}
          </p>
          <p
            :class="isEnforce ? 'text-red-700 dark:text-red-300' : 'text-amber-700 dark:text-amber-300'"
            class="mt-0.5 text-xs"
          >
            {{ isEnforce ? t('admin.risk.enforceBannerDesc') : t('admin.risk.shadowBannerDesc') }}
          </p>
        </div>
        <span
          :class="isEnforce
            ? 'bg-red-200 text-red-800 dark:bg-red-800 dark:text-red-100'
            : 'bg-amber-200 text-amber-800 dark:bg-amber-800 dark:text-amber-100'"
          class="whitespace-nowrap rounded-full px-3 py-1 text-xs font-semibold"
        >
          {{ t('admin.risk.currentMode') }}: {{ isEnforce ? t('admin.risk.modeEnforce') : t('admin.risk.modeObserve') }}
        </span>
      </div>

      <!-- Filter bar -->
      <div class="mb-4 flex flex-wrap items-center gap-3">
        <label class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.risk.tierFilter') }}</label>
        <select v-model="tierFilter" @change="loadUsers" class="input w-40">
          <option value="">{{ t('admin.risk.allTiers') }}</option>
          <option value="high">{{ t('admin.risk.tierHigh') }}</option>
          <option value="medium">{{ t('admin.risk.tierMedium') }}</option>
          <option value="watch">{{ t('admin.risk.tierWatch') }}</option>
        </select>
      </div>

      <!-- Users table -->
      <div class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-500">
        <div v-if="loading" class="flex items-center justify-center py-16">
          <Icon name="refresh" size="xl" class="animate-spin text-gray-400" />
        </div>
        <div v-else-if="sortedUsers.length === 0" class="py-16 text-center">
          <p class="text-sm font-medium text-gray-600 dark:text-gray-300">{{ t('admin.risk.emptyTitle') }}</p>
          <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.risk.emptyDesc') }}</p>
        </div>
        <table v-else class="min-w-full divide-y divide-gray-200 dark:divide-dark-500">
          <thead class="bg-gray-50 dark:bg-dark-700">
            <tr>
              <th class="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.risk.colUser') }}</th>
              <th class="px-4 py-3 text-right text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.risk.colScore') }}</th>
              <th class="px-4 py-3 text-center text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.risk.colTier') }}</th>
              <th class="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.risk.colFeatures') }}</th>
              <th class="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.risk.colWouldDo') }}</th>
              <th class="px-4 py-3 text-center text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.risk.colAllowlist') }}</th>
              <th class="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.risk.colManualTier') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-600 dark:bg-dark-800">
            <tr
              v-for="u in sortedUsers"
              :key="u.user_id"
              class="cursor-pointer hover:bg-gray-50 dark:hover:bg-dark-700"
              @click="openDetail(u)"
            >
              <td class="px-4 py-3">
                <div class="text-sm font-medium text-gray-900 dark:text-white">{{ u.email }}</div>
                <div class="font-mono text-xs text-gray-400 dark:text-gray-500">#{{ u.user_id }}</div>
              </td>
              <td class="px-4 py-3 text-right font-mono text-sm font-semibold text-gray-900 dark:text-white">
                {{ u.score.toFixed(1) }}
              </td>
              <td class="px-4 py-3 text-center">
                <span :class="tierBadgeClass(u.tier)" class="inline-block rounded-full px-2.5 py-0.5 text-xs font-medium">
                  {{ tierLabel(u.tier) }}
                </span>
              </td>
              <td class="px-4 py-3">
                <div class="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-gray-600 dark:text-gray-300">
                  <span>{{ t('admin.risk.featDistinctRatio') }}: <b class="font-mono">{{ fmtRatio(u.window.distinct_ratio) }}</b></span>
                  <span>{{ t('admin.risk.featSingleTurn') }}: <b class="font-mono">{{ fmtRatio(u.window.single_turn_ratio) }}</b></span>
                  <span>{{ t('admin.risk.featOutIn') }}: <b class="font-mono">{{ fmtNum(u.features.out_in_ratio) }}</b></span>
                  <span>{{ t('admin.risk.featTopModel') }}: <b class="font-mono">{{ u.window.top_model || '—' }}</b></span>
                  <span>{{ t('admin.risk.featRpmPeak') }}: <b class="font-mono">{{ fmtNum(u.window.rpm_peak) }}</b></span>
                  <span>{{ t('admin.risk.featBudgetPct') }}: <b class="font-mono">{{ fmtPct(u.window.budget_pct) }}</b></span>
                </div>
              </td>
              <td class="px-4 py-3">
                <span class="text-xs italic text-gray-400 dark:text-gray-500">{{ wouldDoLabel(u.would_do_action) }}</span>
              </td>
              <td class="px-4 py-3 text-center" @click.stop>
                <button
                  @click="toggleAllowlist(u)"
                  :disabled="allowlistBusy.has(u.user_id)"
                  :class="u.allowlisted ? 'bg-green-500 hover:bg-green-600' : 'bg-gray-300 hover:bg-gray-400 dark:bg-dark-500'"
                  class="relative inline-flex h-5 w-9 items-center rounded-full transition-colors focus:outline-none disabled:opacity-50"
                >
                  <span
                    :class="u.allowlisted ? 'translate-x-4' : 'translate-x-0.5'"
                    class="inline-block h-4 w-4 transform rounded-full bg-white transition-transform"
                  />
                </button>
              </td>
              <td class="px-4 py-3" @click.stop>
                <select
                  :value="u.manual_tier ?? ''"
                  @change="onManualTierChange(u, ($event.target as HTMLSelectElement).value)"
                  :disabled="manualTierBusy.has(u.user_id)"
                  class="input w-28 text-xs"
                >
                  <option value="">{{ t('admin.risk.manualAuto') }}</option>
                  <option value="watch">{{ t('admin.risk.tierWatch') }}</option>
                  <option value="medium">{{ t('admin.risk.tierMedium') }}</option>
                  <option value="high">{{ t('admin.risk.tierHigh') }}</option>
                </select>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Config panel (collapsible) -->
      <div class="mt-6 overflow-hidden rounded-lg border border-gray-200 dark:border-dark-500">
        <button
          @click="configOpen = !configOpen"
          class="flex w-full items-center justify-between bg-gray-50 px-4 py-3 text-left dark:bg-dark-700"
        >
          <span class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.risk.configTitle') }}</span>
          <span class="text-xs text-gray-500 dark:text-gray-400">
            {{ configOpen ? t('admin.risk.collapse') : t('admin.risk.expand') }}
          </span>
        </button>
        <div v-if="configOpen" class="bg-white p-4 dark:bg-dark-800">
          <p class="mb-4 flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
            <Icon name="infoCircle" size="sm" />
            {{ t('admin.risk.configHint') }}
          </p>

          <div v-if="!draft" class="py-8 text-center">
            <Icon name="refresh" size="lg" class="animate-spin text-gray-400" />
          </div>

          <div v-else class="space-y-5">
            <!-- Mode -->
            <div>
              <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.configMode') }}</label>
              <select v-model="draft.mode" class="input w-48">
                <option value="observe">{{ t('admin.risk.configModeObserveOpt') }}</option>
                <option value="enforce">{{ t('admin.risk.configModeEnforceOpt') }}</option>
              </select>
              <p v-if="draft.mode === 'enforce'" class="mt-2 rounded-md bg-red-50 px-3 py-2 text-xs font-medium text-red-700 dark:bg-red-900/20 dark:text-red-300">
                {{ t('admin.risk.enforceWarning') }}
              </p>
            </div>

            <!-- Scalar thresholds -->
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
              <div>
                <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.mediumScore') }}</label>
                <input v-model.number="draft.medium_score" type="number" step="any" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.highScore') }}</label>
                <input v-model.number="draft.high_score" type="number" step="any" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.volumeFloor') }}</label>
                <input v-model.number="draft.volume_floor" type="number" step="any" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.andGateK') }}</label>
                <input v-model.number="draft.and_gate_k" type="number" step="any" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.dailyBudget') }}</label>
                <input v-model.number="dailyBudgetUsdc" type="number" step="any" class="input w-full" />
              </div>
              <div>
                <label class="mb-1 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.weeklyBudget') }}</label>
                <input v-model.number="weeklyBudgetUsdc" type="number" step="any" class="input w-full" />
              </div>
            </div>

            <!-- Weights -->
            <div>
              <p class="mb-2 text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.weightsSection') }}</p>
              <div class="grid grid-cols-2 gap-3 sm:grid-cols-4 lg:grid-cols-7">
                <div v-for="k in featureKeys" :key="'w-' + k">
                  <label class="mb-1 block text-center text-xs font-medium uppercase text-gray-500 dark:text-gray-400">{{ k }}</label>
                  <input v-model.number="draft.weights[k]" type="number" step="any" class="input w-full text-center" />
                </div>
              </div>
            </div>

            <!-- Thresholds -->
            <div>
              <p class="mb-2 text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.risk.thresholdsSection') }}</p>
              <div class="grid grid-cols-2 gap-3 sm:grid-cols-4 lg:grid-cols-7">
                <div v-for="k in featureKeys" :key="'t-' + k">
                  <label class="mb-1 block text-center text-xs font-medium uppercase text-gray-500 dark:text-gray-400">{{ k }}</label>
                  <input v-model.number="draft.thresholds[k]" type="number" step="any" class="input w-full text-center" />
                </div>
              </div>
            </div>

            <div class="flex justify-end">
              <button @click="saveConfig" :disabled="savingConfig" class="btn btn-primary">
                {{ savingConfig ? t('admin.risk.saving') : t('admin.risk.save') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Row detail drawer -->
    <div
      v-if="detailUser"
      class="fixed inset-0 z-50 flex justify-end bg-black/40"
      @click.self="detailUser = null"
    >
      <div class="flex h-full w-full max-w-md flex-col overflow-y-auto bg-white shadow-xl dark:bg-dark-800">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-dark-500">
          <div>
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.risk.detailTitle') }}</h3>
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ detailUser.email }}</p>
          </div>
          <button @click="detailUser = null" class="btn btn-secondary btn-xs">{{ t('admin.risk.close') }}</button>
        </div>

        <div class="flex-1 space-y-6 p-5">
          <!-- Score + tier -->
          <div class="flex items-center gap-4">
            <div>
              <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.colScore') }}</div>
              <div class="font-mono text-2xl font-bold text-gray-900 dark:text-white">{{ detailUser.score.toFixed(1) }}</div>
            </div>
            <span :class="tierBadgeClass(detailUser.tier)" class="rounded-full px-2.5 py-0.5 text-xs font-medium">
              {{ tierLabel(detailUser.tier) }}
            </span>
          </div>

          <!-- Feature vector meters -->
          <div>
            <p class="mb-3 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.risk.featuresSection') }}</p>
            <div class="space-y-3">
              <div v-for="f in featureMeters" :key="f.key">
                <div class="mb-1 flex items-center justify-between text-xs">
                  <span class="text-gray-600 dark:text-gray-300">{{ f.label }}</span>
                  <span class="font-mono text-gray-500 dark:text-gray-400">{{ f.value.toFixed(2) }}</span>
                </div>
                <div class="h-2 w-full overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600">
                  <div class="h-full rounded-full bg-blue-500" :style="{ width: meterWidth(f.value) }"></div>
                </div>
              </div>
            </div>
          </div>

          <!-- 24h window -->
          <div>
            <p class="mb-3 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.risk.windowSection') }}</p>
            <dl class="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.winRequests') }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ detailUser.window.requests_24h }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.winOutputTokens') }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ detailUser.window.output_tokens_24h }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.winDistinctRatio') }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ fmtRatio(detailUser.window.distinct_ratio) }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.winSingleTurn') }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ fmtRatio(detailUser.window.single_turn_ratio) }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.winTopModel') }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ detailUser.window.top_model || '—' }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.winRpmPeak') }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ fmtNum(detailUser.window.rpm_peak) }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.winBudgetPct') }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ fmtPct(detailUser.window.budget_pct) }}</dd></div>
              <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.risk.colWouldDo') }}</dt><dd class="text-gray-900 dark:text-white">{{ wouldDoLabel(detailUser.would_do_action) }}</dd></div>
            </dl>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores/app'
import {
  riskAPI,
  type RiskUserView,
  type RiskConfig,
  type RiskTier,
  type RiskWouldDoAction,
  type UpdateRiskConfigPayload,
} from '@/api/admin/risk'

const { t } = useI18n()
const appStore = useAppStore()

type FeatureKey = 'f1' | 'f2' | 'f3' | 'f4' | 'f5' | 'f6' | 'f7'
const featureKeys: FeatureKey[] = ['f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7']

const MICROS_PER_USDC = 1_000_000

const loading = ref(false)
const users = ref<RiskUserView[]>([])
const tierFilter = ref<'' | RiskTier>('')

const config = ref<RiskConfig | null>(null)
const draft = ref<RiskConfig | null>(null)
const configOpen = ref(false)
const savingConfig = ref(false)

const detailUser = ref<RiskUserView | null>(null)

const allowlistBusy = ref(new Set<number>())
const manualTierBusy = ref(new Set<number>())

const isEnforce = computed(() => config.value?.mode === 'enforce')

const sortedUsers = computed(() => [...users.value].sort((a, b) => b.score - a.score))

// Budget fields are stored as micros but edited in USDC.
const dailyBudgetUsdc = computed({
  get: () => (draft.value ? draft.value.daily_budget_micros / MICROS_PER_USDC : 0),
  set: (v: number) => {
    if (draft.value) draft.value.daily_budget_micros = Math.round((v || 0) * MICROS_PER_USDC)
  },
})
const weeklyBudgetUsdc = computed({
  get: () => (draft.value ? draft.value.weekly_budget_micros / MICROS_PER_USDC : 0),
  set: (v: number) => {
    if (draft.value) draft.value.weekly_budget_micros = Math.round((v || 0) * MICROS_PER_USDC)
  },
})

const featureMeters = computed(() => {
  if (!detailUser.value) return []
  const f = detailUser.value.features
  return [
    { key: 'f1', label: t('admin.risk.f1'), value: f.f1_diversity_collapse },
    { key: 'f2', label: t('admin.risk.f2'), value: f.f2_single_turn_ratio },
    { key: 'f3', label: t('admin.risk.f3'), value: f.f3_output_harvest },
    { key: 'f4', label: t('admin.risk.f4'), value: f.f4_machine_cadence },
    { key: 'f5', label: t('admin.risk.f5'), value: f.f5_teacher_concentration },
    { key: 'f6', label: t('admin.risk.f6'), value: f.f6_determinism },
    { key: 'f7', label: t('admin.risk.f7'), value: f.f7_fanout_novelty },
  ]
})

function tierBadgeClass(tier: RiskTier): string {
  switch (tier) {
    case 'high':
      return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'medium':
      return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-300'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-dark-600 dark:text-gray-300'
  }
}

function tierLabel(tier: RiskTier): string {
  return t(`admin.risk.tier${tier.charAt(0).toUpperCase() + tier.slice(1)}`)
}

function wouldDoLabel(action: RiskWouldDoAction): string {
  switch (action) {
    case 'throttle_and_flag':
      return t('admin.risk.wouldThrottle')
    case 'monitor_closely':
      return t('admin.risk.wouldMonitor')
    default:
      return t('admin.risk.wouldNone')
  }
}

function meterWidth(v: number): string {
  const pct = Math.max(0, Math.min(1, v)) * 100
  return `${pct}%`
}

function fmtRatio(v: number): string {
  if (v == null || !isFinite(v)) return '—'
  return v.toFixed(2)
}

function fmtNum(v: number): string {
  if (v == null || !isFinite(v)) return '—'
  return Number(v.toFixed(2)).toString()
}

function fmtPct(v: number): string {
  if (v == null || !isFinite(v)) return '—'
  return `${(v * 100).toFixed(1)}%`
}

async function loadUsers() {
  loading.value = true
  try {
    users.value = await riskAPI.listUsers(tierFilter.value ? { tier: tierFilter.value } : {})
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.risk.loadFail'))
  } finally {
    loading.value = false
  }
}

async function loadConfig() {
  try {
    const cfg = await riskAPI.getConfig()
    config.value = cfg
    draft.value = JSON.parse(JSON.stringify(cfg)) as RiskConfig
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.risk.configLoadFail'))
  }
}

async function openDetail(u: RiskUserView) {
  detailUser.value = u
  // Fetch the full view (includes 24h window details) in case the list is trimmed.
  try {
    const full = await riskAPI.getUser(u.user_id)
    if (detailUser.value && detailUser.value.user_id === full.user_id) {
      detailUser.value = full
    }
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.risk.detailLoadFail'))
  }
}

async function toggleAllowlist(u: RiskUserView) {
  if (allowlistBusy.value.has(u.user_id)) return
  allowlistBusy.value.add(u.user_id)
  const next = !u.allowlisted
  try {
    const res = await riskAPI.setAllowlist(u.user_id, next)
    u.allowlisted = res.allowlisted
    appStore.showSuccess(res.allowlisted ? t('admin.risk.allowlistOn') : t('admin.risk.allowlistOff'))
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.risk.allowlistFail'))
  } finally {
    allowlistBusy.value.delete(u.user_id)
  }
}

async function onManualTierChange(u: RiskUserView, value: string) {
  if (manualTierBusy.value.has(u.user_id)) return
  manualTierBusy.value.add(u.user_id)
  const tier = (value === '' ? null : (value as RiskTier))
  try {
    const res = await riskAPI.setManualTier(u.user_id, tier)
    u.manual_tier = res.manual_tier
    appStore.showSuccess(t('admin.risk.manualTierUpdated'))
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.risk.manualTierFail'))
  } finally {
    manualTierBusy.value.delete(u.user_id)
  }
}

async function saveConfig() {
  if (!draft.value) return
  savingConfig.value = true
  try {
    const d = draft.value
    const payload: UpdateRiskConfigPayload = {
      mode: d.mode,
      medium_score: d.medium_score,
      high_score: d.high_score,
      volume_floor: d.volume_floor,
      and_gate_k: d.and_gate_k,
      daily_budget_micros: d.daily_budget_micros,
      weekly_budget_micros: d.weekly_budget_micros,
      weights: { ...d.weights },
      thresholds: { ...d.thresholds },
    }
    const cfg = await riskAPI.updateConfig(payload)
    config.value = cfg
    draft.value = JSON.parse(JSON.stringify(cfg)) as RiskConfig
    appStore.showSuccess(t('admin.risk.saveSuccess'))
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.risk.saveFail'))
  } finally {
    savingConfig.value = false
  }
}

onMounted(() => {
  loadUsers()
  loadConfig()
})
</script>
