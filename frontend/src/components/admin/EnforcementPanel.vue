<template>
  <div class="rounded-lg border border-gray-200 dark:border-dark-500">
    <!-- Header / toggle -->
    <button
      class="flex w-full items-center justify-between px-4 py-3 text-left"
      :aria-expanded="open"
      @click="open = !open"
    >
      <span class="flex items-center gap-2">
        <span class="text-sm font-semibold text-gray-800 dark:text-gray-200">{{ t('admin.enforcement.title') }}</span>
        <span
          v-if="status"
          class="rounded-full px-2 py-0.5 text-[11px] font-medium"
          :class="status.enabled ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300' : 'bg-gray-100 text-gray-500 dark:bg-dark-600 dark:text-gray-400'"
        >{{ status.enabled ? t('admin.enforcement.on') : t('admin.enforcement.off') }}</span>
      </span>
      <span class="text-xs text-primary-600 dark:text-primary-400">{{ open ? t('admin.enforcement.collapse') : t('admin.enforcement.expand') }}</span>
    </button>

    <div v-if="open" class="border-t border-gray-100 px-4 py-3 dark:border-dark-600">
      <p class="mb-3 text-[11px] text-amber-600 dark:text-amber-400">{{ t('admin.enforcement.hint') }}</p>

      <!-- Status row -->
      <div v-if="status" class="mb-3 grid grid-cols-2 gap-2 text-xs sm:grid-cols-3 lg:grid-cols-6">
        <div class="card px-3 py-2">
          <div class="text-gray-500 dark:text-gray-400">{{ t('admin.enforcement.master') }}</div>
          <div class="mt-0.5 font-semibold" :class="status.enabled ? 'text-red-600 dark:text-red-400' : 'text-gray-500'">
            {{ status.enabled ? t('admin.enforcement.on') : t('admin.enforcement.off') }}
          </div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-gray-500 dark:text-gray-400">{{ t('admin.enforcement.throttleRpm') }}</div>
          <div class="mt-0.5 font-semibold text-gray-800 dark:text-gray-200">{{ status.throttle_rpm }}</div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-gray-500 dark:text-gray-400">{{ t('admin.enforcement.confidenceMin') }}</div>
          <div class="mt-0.5 font-semibold text-gray-800 dark:text-gray-200">{{ status.confidence_min.toFixed(2) }}</div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-gray-500 dark:text-gray-400">{{ t('admin.enforcement.highCount') }}</div>
          <div class="mt-0.5 font-semibold text-gray-800 dark:text-gray-200">{{ status.high_user_count }}</div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-gray-500 dark:text-gray-400">{{ t('admin.enforcement.allowlistSize') }}</div>
          <div class="mt-0.5 font-semibold text-gray-800 dark:text-gray-200">{{ status.allowlist_size }}</div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-gray-500 dark:text-gray-400">{{ t('admin.enforcement.refreshedAt') }}</div>
          <div class="mt-0.5 font-mono text-gray-800 dark:text-gray-200">{{ fmtTime(status.refreshed_at) }}</div>
        </div>
      </div>
      <p v-if="status && !status.enabled" class="mb-3 rounded border border-gray-200 bg-gray-50 px-3 py-2 text-xs text-gray-600 dark:border-dark-500 dark:bg-dark-700 dark:text-gray-300">
        {{ t('admin.enforcement.disabledNote') }}
      </p>

      <div class="mb-3 flex flex-wrap items-center gap-2">
        <button @click="load" :disabled="loading" class="btn btn-secondary btn-xs">{{ t('admin.enforcement.refresh') }}</button>
      </div>

      <!-- HIGH users table -->
      <div v-if="users.length" class="overflow-x-auto">
        <table class="min-w-full text-xs">
          <thead>
            <tr class="text-left text-gray-500 dark:text-gray-400">
              <th class="py-1 pr-3">{{ t('admin.enforcement.colUser') }}</th>
              <th class="py-1 pr-3">{{ t('admin.enforcement.colRiskIndex') }}</th>
              <th class="py-1 pr-3">{{ t('admin.enforcement.colConfidence') }}</th>
              <th class="py-1 pr-3">{{ t('admin.enforcement.colDataSufficient') }}</th>
              <th class="py-1 pr-3">{{ t('admin.enforcement.colState') }}</th>
              <th class="py-1 pr-3">{{ t('admin.enforcement.colActions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 dark:divide-dark-600">
            <tr v-for="u in users" :key="u.user_id">
              <td class="py-1.5 pr-3 font-mono">#{{ u.user_id }}</td>
              <td class="py-1.5 pr-3">{{ u.risk_index.toFixed(1) }}</td>
              <td class="py-1.5 pr-3">{{ u.confidence.toFixed(2) }}</td>
              <td class="py-1.5 pr-3">{{ u.data_sufficient ? t('admin.enforcement.yes') : t('admin.enforcement.no') }}</td>
              <td class="py-1.5 pr-3">
                <span v-if="u.allowlisted" class="rounded-full bg-green-100 px-2 py-0.5 text-[11px] font-medium text-green-700 dark:bg-green-900/30 dark:text-green-300">{{ t('admin.enforcement.stateAllowlisted') }}</span>
                <span v-else-if="u.throttled" class="rounded-full bg-red-100 px-2 py-0.5 text-[11px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300">{{ t('admin.enforcement.stateThrottled') }}</span>
                <span v-else class="rounded-full bg-gray-100 px-2 py-0.5 text-[11px] font-medium text-gray-500 dark:bg-dark-600 dark:text-gray-400">{{ t('admin.enforcement.stateObserved') }}</span>
              </td>
              <td class="py-1.5 pr-3">
                <div class="flex flex-wrap gap-1">
                  <button v-if="!u.allowlisted" @click="addAllow(u.user_id)" :disabled="busy" class="btn btn-secondary btn-xs">{{ t('admin.enforcement.exempt') }}</button>
                  <button v-else @click="removeAllow(u.user_id)" :disabled="busy" class="btn btn-secondary btn-xs">{{ t('admin.enforcement.unexempt') }}</button>
                  <button @click="banUser(u.user_id)" :disabled="busy" class="btn btn-secondary btn-xs text-red-600 dark:text-red-400">{{ t('admin.enforcement.banUser') }}</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else-if="!loading" class="py-4 text-center text-xs text-gray-400">{{ t('admin.enforcement.empty') }}</div>

      <!-- Restricted models (HIGH users only) -->
      <div class="mt-4 border-t border-gray-100 pt-3 dark:border-dark-600">
        <p class="mb-1 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.enforcement.modelsTitle') }}</p>
        <p class="mb-2 text-[11px] text-gray-400">{{ t('admin.enforcement.modelsHint') }}</p>
        <div v-if="rules.length" class="mb-2 flex flex-wrap gap-2">
          <span v-for="r in rules" :key="r.model" class="inline-flex items-center gap-1 rounded border px-2 py-0.5 text-xs"
            :class="r.action === 'block' ? 'border-red-300 bg-red-50 text-red-700 dark:border-red-800 dark:bg-red-900/20 dark:text-red-300' : 'border-amber-300 bg-amber-50 text-amber-700 dark:border-amber-800 dark:bg-amber-900/10 dark:text-amber-300'">
            <b class="font-mono">{{ r.model }}</b>
            <span>· {{ r.action === 'block' ? t('admin.enforcement.actBlock') : t('admin.enforcement.actThrottle') }}</span>
            <button @click="removeRule(r.model)" :disabled="busy" class="ml-1 text-gray-400 hover:text-red-500" :aria-label="t('admin.enforcement.removeRule')">×</button>
          </span>
        </div>
        <div v-else class="mb-2 text-xs text-gray-400">{{ t('admin.enforcement.noRules') }}</div>
        <div class="flex flex-wrap items-end gap-2">
          <div>
            <label class="input-label" for="enf-model">{{ t('admin.enforcement.modelLabel') }}</label>
            <input id="enf-model" v-model.trim="modelName" type="text" class="input w-56" :placeholder="t('admin.enforcement.modelPlaceholder')" />
          </div>
          <div>
            <label class="input-label" for="enf-action">{{ t('admin.enforcement.actionLabel') }}</label>
            <select id="enf-action" v-model="modelAction" class="input w-32">
              <option value="throttle">{{ t('admin.enforcement.actThrottle') }}</option>
              <option value="block">{{ t('admin.enforcement.actBlock') }}</option>
            </select>
          </div>
          <button @click="addRule" :disabled="busy || !modelName" class="btn btn-secondary btn-sm">{{ t('admin.enforcement.addRule') }}</button>
        </div>
      </div>

      <!-- Manual key ban -->
      <div class="mt-4 flex flex-wrap items-end gap-2 border-t border-gray-100 pt-3 dark:border-dark-600">
        <div>
          <label class="input-label" for="enf-kid">{{ t('admin.enforcement.keyIdLabel') }}</label>
          <input id="enf-kid" v-model.number="keyId" type="number" min="1" step="1" class="input w-32" :placeholder="t('admin.enforcement.keyIdPlaceholder')" />
        </div>
        <button @click="banKey" :disabled="busy || !keyId" class="btn btn-secondary btn-sm text-red-600 dark:text-red-400">{{ t('admin.enforcement.banKey') }}</button>
        <button @click="unbanKey" :disabled="busy || !keyId" class="btn btn-secondary btn-sm">{{ t('admin.enforcement.unbanKey') }}</button>
      </div>

      <p v-if="error" role="alert" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ error }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { enforcementAPI, type EnforcementStatus, type EnforcementHighUser, type EnforcementModelRule, type EnforcementModelAction } from '@/api/admin/enforcement'

const { t } = useI18n()
const appStore = useAppStore()

const open = ref(false)
const loading = ref(false)
const busy = ref(false)
const status = ref<EnforcementStatus | null>(null)
const users = ref<EnforcementHighUser[]>([])
const rules = ref<EnforcementModelRule[]>([])
const modelName = ref('')
const modelAction = ref<EnforcementModelAction>('throttle')
const keyId = ref<number | ''>('')
const error = ref('')

function fmtTime(unix: number): string {
  if (!unix || unix <= 0) return '—'
  return new Date(unix * 1000).toLocaleString()
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [st, us, rs] = await Promise.all([
      enforcementAPI.getStatus(),
      enforcementAPI.listHighUsers(),
      enforcementAPI.listModelRules(),
    ])
    status.value = st
    users.value = us
    rules.value = rs
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.loadFail')
  } finally {
    loading.value = false
  }
}

async function addRule() {
  const m = modelName.value.trim()
  if (!m) return
  busy.value = true
  error.value = ''
  try {
    await enforcementAPI.setModelRule(m, modelAction.value)
    modelName.value = ''
    appStore.showSuccess(t('admin.enforcement.ruleSaved'))
    await load()
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.actionFail')
  } finally {
    busy.value = false
  }
}

async function removeRule(model: string) {
  busy.value = true
  error.value = ''
  try {
    await enforcementAPI.removeModelRule(model)
    appStore.showSuccess(t('admin.enforcement.ruleRemoved'))
    await load()
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.actionFail')
  } finally {
    busy.value = false
  }
}

async function addAllow(userId: number) {
  busy.value = true
  error.value = ''
  try {
    await enforcementAPI.addAllowlist(userId)
    appStore.showSuccess(t('admin.enforcement.exempted'))
    await load()
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.actionFail')
  } finally {
    busy.value = false
  }
}

async function removeAllow(userId: number) {
  busy.value = true
  error.value = ''
  try {
    await enforcementAPI.removeAllowlist(userId)
    appStore.showSuccess(t('admin.enforcement.unexempted'))
    await load()
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.actionFail')
  } finally {
    busy.value = false
  }
}

async function banUser(userId: number) {
  if (!window.confirm(t('admin.enforcement.banUserConfirm', { id: userId }))) return
  busy.value = true
  error.value = ''
  try {
    await enforcementAPI.ban('user', userId)
    appStore.showSuccess(t('admin.enforcement.banned'))
    await load()
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.actionFail')
  } finally {
    busy.value = false
  }
}

async function banKey() {
  const id = Number(keyId.value)
  if (!Number.isInteger(id) || id <= 0) return
  if (!window.confirm(t('admin.enforcement.banKeyConfirm', { id }))) return
  busy.value = true
  error.value = ''
  try {
    await enforcementAPI.ban('key', id)
    appStore.showSuccess(t('admin.enforcement.banned'))
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.actionFail')
  } finally {
    busy.value = false
  }
}

async function unbanKey() {
  const id = Number(keyId.value)
  if (!Number.isInteger(id) || id <= 0) return
  busy.value = true
  error.value = ''
  try {
    await enforcementAPI.unban('key', id)
    appStore.showSuccess(t('admin.enforcement.unbanned'))
  } catch (e: any) {
    error.value = e?.message || t('admin.enforcement.actionFail')
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>
