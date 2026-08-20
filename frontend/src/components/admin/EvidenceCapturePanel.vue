<template>
  <div class="border-t border-gray-100 pt-4 dark:border-dark-600">
    <div class="mb-2 flex items-center justify-between">
      <p class="text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.evidence.section') }}</p>
      <button @click="loadStatus" :disabled="loading" class="text-xs text-primary-600 hover:underline dark:text-primary-400">
        {{ t('admin.evidence.refresh') }}
      </button>
    </div>
    <p class="mb-3 text-[11px] text-amber-600 dark:text-amber-400">{{ t('admin.evidence.hint') }}</p>

    <!-- 目标选择：整个用户 / 指定 Key -->
    <div class="mb-3 flex flex-wrap items-end gap-2">
      <div>
        <label class="input-label">{{ t('admin.evidence.targetLabel') }}</label>
        <div class="flex gap-1">
          <button :class="targetType === 'user' ? 'btn-primary' : 'btn-secondary'" class="btn btn-xs" @click="setTargetType('user')">{{ t('admin.evidence.targetUser') }}</button>
          <button :class="targetType === 'key' ? 'btn-primary' : 'btn-secondary'" class="btn btn-xs" @click="setTargetType('key')">{{ t('admin.evidence.targetKey') }}</button>
        </div>
      </div>
      <div v-if="targetType === 'key'">
        <label class="input-label" for="ev-kid">{{ t('admin.evidence.keyIdLabel') }}</label>
        <input id="ev-kid" v-model.number="keyId" type="number" min="1" step="1" class="input w-32" :placeholder="t('admin.evidence.keyIdPlaceholder')" @change="loadStatus" />
      </div>
    </div>

    <!-- 未在捕获：开始 -->
    <div v-if="!flag && targetReady" class="flex flex-wrap items-end gap-2">
      <div>
        <label class="input-label" for="ev-th">{{ t('admin.evidence.threshold') }}</label>
        <input id="ev-th" v-model.number="storeThreshold" type="number" min="2" max="100" class="input w-28" />
      </div>
      <button data-test="ev-start" @click="start" :disabled="loading" class="btn btn-primary btn-sm">{{ t('admin.evidence.start') }}</button>
      <span class="text-[11px] text-gray-400">{{ t('admin.evidence.thresholdHint') }}</span>
    </div>

    <!-- 捕获中：状态 + 操作 -->
    <div v-else-if="flag" class="space-y-2">
      <div class="flex flex-wrap items-center gap-2 text-xs">
        <span class="rounded-full bg-red-100 px-2.5 py-0.5 font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300">{{ t('admin.evidence.capturing') }}</span>
        <span class="font-mono text-gray-500 dark:text-gray-400">{{ flag.target_key }}</span>
        <span class="text-gray-600 dark:text-gray-300">{{ t('admin.evidence.thresholdIs', { n: flag.store_threshold }) }}</span>
      </div>
      <div class="flex flex-wrap gap-2">
        <button data-test="ev-view" @click="loadTemplates" :disabled="tplLoading" class="btn btn-secondary btn-sm">
          {{ tplLoading ? t('admin.evidence.loading') : t('admin.evidence.view') }}
        </button>
        <button data-test="ev-purge" @click="purge" :disabled="loading" class="btn btn-secondary btn-sm text-red-600 dark:text-red-400">{{ t('admin.evidence.purge') }}</button>
      </div>
    </div>

    <p v-if="error" role="alert" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ error }}</p>

    <!-- 已聚合的重复模板 -->
    <div v-if="tplShown" class="mt-3">
      <p class="mb-1 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.evidence.summary', { n: templates.length, max: maxRepeat }) }}
      </p>
      <div v-if="templates.length === 0" class="text-xs text-gray-400">{{ t('admin.evidence.empty') }}</div>
      <div v-else class="max-h-[32rem] space-y-2 overflow-y-auto pr-1">
        <div v-for="(tp, i) in templates" :key="i" class="rounded border p-2 text-xs" :class="badgeBorder(tp.count)">
          <div class="mb-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[11px] text-gray-500 dark:text-gray-400">
            <span class="rounded px-1.5 py-0.5 font-medium text-white" :class="badgeBg(tp.count)">{{ t('admin.evidence.repeatBadge', { n: tp.count }) }}</span>
            <span>key: <b class="font-mono">{{ tp.api_key_ids.map((k) => '#' + k).join(', ') }}</b></span>
            <span>model: <b class="font-mono">{{ tp.model || '—' }}</b></span>
            <span class="font-mono">{{ tp.endpoint }}</span>
            <span>{{ t('admin.evidence.timeSpan') }}: {{ fmtTime(tp.first_seen) }} → {{ fmtTime(tp.last_seen) }}</span>
            <span v-if="tp.truncated" class="text-amber-600 dark:text-amber-400">{{ t('admin.evidence.truncated') }}</span>
          </div>
          <div v-if="tp.request_ids.length" class="mb-1 truncate font-mono text-[10px] text-gray-400 dark:text-gray-500">
            req: {{ tp.request_ids.join('  ') }}
          </div>
          <pre v-if="tp.has_body" class="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded bg-gray-50 p-2 font-mono text-[11px] text-gray-800 dark:bg-dark-700 dark:text-gray-200">{{ tp.body }}</pre>
          <p v-else class="text-[11px] text-gray-400">{{ t('admin.evidence.noBodyYet', { n: flag ? flag.store_threshold : storeThreshold }) }}</p>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { evidenceAPI, type EvidenceFlag, type EvidenceTemplate, type EvidenceTargetType } from '@/api/admin/evidence'

const props = defineProps<{ userId: number }>()
const { t } = useI18n()
const appStore = useAppStore()

const targetType = ref<EvidenceTargetType>('user')
const keyId = ref<number | ''>('')
const loading = ref(false)
const flag = ref<EvidenceFlag | null>(null)
const storeThreshold = ref(2)
const templates = ref<EvidenceTemplate[]>([])
const tplLoading = ref(false)
const tplShown = ref(false)
const error = ref('')

const target = computed<string>(() => {
  if (targetType.value === 'user') return 'u:' + props.userId
  const n = Number(keyId.value)
  return Number.isInteger(n) && n > 0 ? 'k:' + n : ''
})
const targetReady = computed(() => target.value !== '')
const maxRepeat = computed(() => templates.value.reduce((m, tp) => Math.max(m, tp.count), 0))

function badgeBg(count: number): string {
  if (count >= 10) return 'bg-red-600'
  if (count >= 3) return 'bg-amber-500'
  return 'bg-gray-400'
}
function badgeBorder(count: number): string {
  if (count >= 10) return 'border-red-300 bg-red-50 dark:border-red-800 dark:bg-red-900/20'
  if (count >= 3) return 'border-amber-300 bg-amber-50 dark:border-amber-800 dark:bg-amber-900/10'
  return 'border-gray-200 dark:border-dark-600'
}
function fmtTime(unix: number): string {
  if (!unix || unix <= 0) return '—'
  return new Date(unix * 1000).toLocaleString()
}

function resetView() {
  flag.value = null
  templates.value = []
  tplShown.value = false
  error.value = ''
}

function setTargetType(tt: EvidenceTargetType) {
  if (targetType.value === tt) return
  targetType.value = tt
  resetView()
  loadStatus()
}

async function loadStatus() {
  resetView()
  if (!targetReady.value) return
  loading.value = true
  try {
    const list = await evidenceAPI.listCaptures()
    flag.value = list.find((f) => f.target_key === target.value) ?? null
  } catch (e: any) {
    error.value = e?.message || t('admin.evidence.loadFail')
  } finally {
    loading.value = false
  }
}

async function start() {
  if (!targetReady.value) {
    error.value = t('admin.evidence.badKeyId')
    return
  }
  const th = Number(storeThreshold.value)
  if (!Number.isInteger(th) || th < 2) {
    error.value = t('admin.evidence.badThreshold')
    return
  }
  loading.value = true
  error.value = ''
  try {
    const id = targetType.value === 'user' ? props.userId : Number(keyId.value)
    flag.value = await evidenceAPI.startCapture(targetType.value, id, th)
    appStore.showSuccess(t('admin.evidence.started'))
  } catch (e: any) {
    error.value = e?.message || t('admin.evidence.startFail')
  } finally {
    loading.value = false
  }
}

async function loadTemplates() {
  tplLoading.value = true
  error.value = ''
  try {
    templates.value = await evidenceAPI.listTemplates(target.value)
    tplShown.value = true
  } catch (e: any) {
    error.value = e?.message || t('admin.evidence.loadFail')
  } finally {
    tplLoading.value = false
  }
}

async function purge() {
  if (!window.confirm(t('admin.evidence.purgeConfirm'))) return
  loading.value = true
  error.value = ''
  try {
    await evidenceAPI.purge(target.value)
    resetView()
    appStore.showSuccess(t('admin.evidence.purged'))
  } catch (e: any) {
    error.value = e?.message || t('admin.evidence.purgeFail')
  } finally {
    loading.value = false
  }
}

watch(
  () => props.userId,
  () => {
    targetType.value = 'user'
    keyId.value = ''
    resetView()
    loadStatus()
  },
)

onMounted(loadStatus)
</script>
