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
          <button
            :class="targetType === 'user' ? 'btn-primary' : 'btn-secondary'"
            class="btn btn-xs" @click="setTargetType('user')"
          >{{ t('admin.evidence.targetUser') }}</button>
          <button
            :class="targetType === 'key' ? 'btn-primary' : 'btn-secondary'"
            class="btn btn-xs" @click="setTargetType('key')"
          >{{ t('admin.evidence.targetKey') }}</button>
        </div>
      </div>
      <div v-if="targetType === 'key'">
        <label class="input-label" for="ev-kid">{{ t('admin.evidence.keyIdLabel') }}</label>
        <input id="ev-kid" v-model.number="keyId" type="number" min="1" step="1" class="input w-32"
          :placeholder="t('admin.evidence.keyIdPlaceholder')" @change="loadStatus" />
      </div>
    </div>

    <!-- 未在捕获：开始 -->
    <div v-if="!flag && targetReady" class="flex flex-wrap items-end gap-2">
      <div>
        <label class="input-label" for="ev-n">{{ t('admin.evidence.maxCount') }}</label>
        <input id="ev-n" v-model.number="maxCount" type="number" min="1" max="500" class="input w-28" />
      </div>
      <button data-test="ev-start" @click="start" :disabled="loading" class="btn btn-primary btn-sm">
        {{ t('admin.evidence.start') }}
      </button>
    </div>

    <!-- 捕获中：状态 + 操作 -->
    <div v-else-if="flag" class="space-y-2">
      <div class="flex flex-wrap items-center gap-2 text-xs">
        <span class="rounded-full bg-red-100 px-2.5 py-0.5 font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300">
          {{ t('admin.evidence.capturing') }}
        </span>
        <span class="font-mono text-gray-500 dark:text-gray-400">{{ flag.target_key }}</span>
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
      <p class="mb-1 flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
        <span>{{ t('admin.evidence.entriesCount', { n: entries.length }) }}</span>
        <span v-if="dupTotal > 0" class="rounded bg-red-100 px-1.5 py-0.5 text-[11px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300">
          {{ t('admin.evidence.dupSummary', { n: dupTotal }) }}
        </span>
      </p>
      <div v-if="entries.length === 0" class="text-xs text-gray-400">{{ t('admin.evidence.empty') }}</div>
      <div v-else class="max-h-[32rem] space-y-2 overflow-y-auto pr-1">
        <div
          v-for="(e, i) in entries" :key="i"
          class="rounded border p-2 text-xs"
          :class="isDup(e) ? 'border-red-300 bg-red-50 dark:border-red-800 dark:bg-red-900/20' : 'border-gray-200 dark:border-dark-600'"
        >
          <div class="mb-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[11px] text-gray-500 dark:text-gray-400">
            <span v-if="isDup(e)" class="rounded bg-red-600 px-1.5 py-0.5 font-medium text-white">
              {{ t('admin.evidence.dupBadge', { n: dupCount(e) }) }}
            </span>
            <span>{{ fmtTime(e.ts) }}</span>
            <span>key: <b class="font-mono">#{{ e.api_key_id }}</b></span>
            <span v-if="e.request_id">req: <b class="font-mono">{{ e.request_id }}</b></span>
            <span>model: <b class="font-mono">{{ e.model || '—' }}</b></span>
            <span class="font-mono">{{ e.endpoint }}</span>
            <span v-if="e.truncated" class="text-amber-600 dark:text-amber-400">{{ t('admin.evidence.truncated') }}</span>
          </div>
          <pre class="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded bg-gray-50 p-2 font-mono text-[11px] text-gray-800 dark:bg-dark-700 dark:text-gray-200">{{ e.body }}</pre>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { evidenceAPI, type EvidenceFlag, type EvidenceEntry, type EvidenceTargetType } from '@/api/admin/evidence'

const props = defineProps<{ userId: number }>()
const { t } = useI18n()
const appStore = useAppStore()

const targetType = ref<EvidenceTargetType>('user')
const keyId = ref<number | ''>('')
const loading = ref(false)
const flag = ref<EvidenceFlag | null>(null)
const maxCount = ref(20)
const entries = ref<EvidenceEntry[]>([])
const entriesLoading = ref(false)
const entriesShown = ref(false)
const error = ref('')

// 当前目标 key（u:<userId> 或 k:<keyId>）；key 模式未填 id 时为空。
const target = computed<string>(() => {
  if (targetType.value === 'user') return 'u:' + props.userId
  const n = Number(keyId.value)
  return Number.isInteger(n) && n > 0 ? 'k:' + n : ''
})
const targetReady = computed(() => target.value !== '')

// 模板重复分组：simhash → 出现次数（simhash 非 "0"/"" 才计）。
const simhashCounts = computed<Record<string, number>>(() => {
  const m: Record<string, number> = {}
  for (const e of entries.value) {
    const h = e.prompt_simhash
    if (h && h !== '0') m[h] = (m[h] || 0) + 1
  }
  return m
})
function isDup(e: EvidenceEntry): boolean {
  return !!e.prompt_simhash && e.prompt_simhash !== '0' && (simhashCounts.value[e.prompt_simhash] || 0) >= 2
}
function dupCount(e: EvidenceEntry): number {
  return simhashCounts.value[e.prompt_simhash] || 0
}
// 命中模板重复的条目总数。
const dupTotal = computed(() => entries.value.filter(isDup).length)

function fmtTime(unix: number): string {
  if (!unix || unix <= 0) return '—'
  return new Date(unix * 1000).toLocaleString()
}

function resetView() {
  flag.value = null
  entries.value = []
  entriesShown.value = false
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
  const n = Number(maxCount.value)
  if (!Number.isInteger(n) || n <= 0) {
    error.value = t('admin.evidence.badCount')
    return
  }
  loading.value = true
  error.value = ''
  try {
    const id = targetType.value === 'user' ? props.userId : Number(keyId.value)
    flag.value = await evidenceAPI.startCapture(targetType.value, id, n)
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
    entries.value = await evidenceAPI.listEvidence(target.value)
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
    // 切换用户：回到用户级、清空 key、重置并重新拉状态。
    targetType.value = 'user'
    keyId.value = ''
    resetView()
    loadStatus()
  },
)

onMounted(loadStatus)
</script>
