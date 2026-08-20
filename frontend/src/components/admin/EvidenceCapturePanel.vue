<template>
  <div class="border-t border-gray-100 pt-4 dark:border-dark-600">
    <div class="mb-2 flex items-center justify-between">
      <p class="text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.evidence.section') }}</p>
      <button @click="loadStatus" :disabled="loading" class="text-xs text-primary-600 hover:underline dark:text-primary-400">
        {{ t('admin.evidence.refresh') }}
      </button>
    </div>
    <p class="mb-3 text-[11px] text-amber-600 dark:text-amber-400">{{ t('admin.evidence.hint') }}</p>

    <!-- 未在捕获：开始 -->
    <div v-if="!flag" class="flex flex-wrap items-end gap-2">
      <div>
        <label class="input-label" for="ev-n">{{ t('admin.evidence.maxCount') }}</label>
        <input id="ev-n" v-model.number="maxCount" type="number" min="1" max="500" class="input w-28" />
      </div>
      <button data-test="ev-start" @click="start" :disabled="loading" class="btn btn-primary btn-sm">
        {{ t('admin.evidence.start') }}
      </button>
    </div>

    <!-- 捕获中：状态 + 操作 -->
    <div v-else class="space-y-2">
      <div class="flex flex-wrap items-center gap-2 text-xs">
        <span class="rounded-full bg-red-100 px-2.5 py-0.5 font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300">
          {{ t('admin.evidence.capturing') }}
        </span>
        <span class="text-gray-600 dark:text-gray-300">{{ t('admin.evidence.remaining', { remaining: flag.remaining, max: flag.max }) }}</span>
      </div>
      <div class="flex flex-wrap gap-2">
        <button data-test="ev-view" @click="loadEvidence" :disabled="entriesLoading" class="btn btn-secondary btn-sm">
          {{ entriesLoading ? t('admin.evidence.loading') : t('admin.evidence.view') }}
        </button>
        <button data-test="ev-purge" @click="purge" :disabled="loading" class="btn btn-secondary btn-sm text-red-600 dark:text-red-400">
          {{ t('admin.evidence.purge') }}
        </button>
      </div>
    </div>

    <p v-if="error" role="alert" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ error }}</p>

    <!-- 已捕获条目 -->
    <div v-if="entriesShown" class="mt-3">
      <p class="mb-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.evidence.entriesCount', { n: entries.length }) }}</p>
      <div v-if="entries.length === 0" class="text-xs text-gray-400">{{ t('admin.evidence.empty') }}</div>
      <div v-else class="max-h-72 space-y-2 overflow-y-auto pr-1">
        <div v-for="(e, i) in entries" :key="i" class="rounded border border-gray-200 p-2 text-xs dark:border-dark-600">
          <div class="mb-1 flex flex-wrap gap-x-3 text-[11px] text-gray-500 dark:text-gray-400">
            <span>{{ fmtTime(e.ts) }}</span>
            <span>model: <b class="font-mono">{{ e.model || '—' }}</b></span>
            <span class="font-mono">{{ e.endpoint }}</span>
            <span v-if="e.truncated" class="text-amber-600 dark:text-amber-400">{{ t('admin.evidence.truncated') }}</span>
          </div>
          <pre class="max-h-40 overflow-auto whitespace-pre-wrap break-all rounded bg-gray-50 p-2 font-mono text-[11px] text-gray-800 dark:bg-dark-700 dark:text-gray-200">{{ e.body }}</pre>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { evidenceAPI, type EvidenceFlag, type EvidenceEntry } from '@/api/admin/evidence'

const props = defineProps<{ userId: number }>()
const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const flag = ref<EvidenceFlag | null>(null)
const maxCount = ref(20)
const entries = ref<EvidenceEntry[]>([])
const entriesLoading = ref(false)
const entriesShown = ref(false)
const error = ref('')

function target(): string {
  return 'u:' + props.userId
}

function fmtTime(unix: number): string {
  if (!unix || unix <= 0) return '—'
  return new Date(unix * 1000).toLocaleString()
}

async function loadStatus() {
  if (!props.userId) return
  loading.value = true
  error.value = ''
  try {
    const list = await evidenceAPI.listCaptures()
    flag.value = list.find((f) => f.target_key === target()) ?? null
  } catch (e: any) {
    error.value = e?.message || t('admin.evidence.loadFail')
  } finally {
    loading.value = false
  }
}

async function start() {
  const n = Number(maxCount.value)
  if (!Number.isInteger(n) || n <= 0) {
    error.value = t('admin.evidence.badCount')
    return
  }
  loading.value = true
  error.value = ''
  try {
    flag.value = await evidenceAPI.startCapture('user', props.userId, n)
    appStore.showSuccess(t('admin.evidence.started'))
  } catch (e: any) {
    error.value = e?.message || t('admin.evidence.startFail')
  } finally {
    loading.value = false
  }
}

async function loadEvidence() {
  entriesLoading.value = true
  error.value = ''
  try {
    entries.value = await evidenceAPI.listEvidence(target())
    entriesShown.value = true
  } catch (e: any) {
    error.value = e?.message || t('admin.evidence.loadFail')
  } finally {
    entriesLoading.value = false
  }
}

async function purge() {
  if (!window.confirm(t('admin.evidence.purgeConfirm'))) return
  loading.value = true
  error.value = ''
  try {
    await evidenceAPI.purge(target())
    flag.value = null
    entries.value = []
    entriesShown.value = false
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
    // 切换用户：重置面板并重新拉状态。
    flag.value = null
    entries.value = []
    entriesShown.value = false
    error.value = ''
    loadStatus()
  },
)

onMounted(loadStatus)
</script>
