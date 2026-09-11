<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-8">
      <!-- Header -->
      <div class="mb-4 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.promptAudit.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.description') }}</p>
        </div>
        <button
          @click="refreshAll"
          :disabled="listLoading || statusLoading"
          class="btn btn-secondary"
          :title="t('admin.promptAudit.refresh')"
        >
          <Icon name="refresh" size="md" :class="(listLoading || statusLoading) ? 'animate-spin' : ''" />
        </button>
      </div>

      <!-- Config -->
      <div class="card mb-6 p-5">
        <h2 class="mb-1 text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.promptAudit.config') }}</h2>
        <p class="mb-4 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.configHint') }}</p>
        <div class="flex flex-col gap-5 sm:flex-row sm:items-end sm:gap-8">
          <div class="flex items-center gap-3">
            <Toggle v-model="form.enabled" />
            <div>
              <div class="text-sm font-medium text-gray-800 dark:text-gray-100">{{ t('admin.promptAudit.enabled') }}</div>
              <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.enabledHint') }}</div>
            </div>
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium text-gray-800 dark:text-gray-100" for="pa-retention">
              {{ t('admin.promptAudit.retentionDays') }}
            </label>
            <input
              id="pa-retention"
              v-model.number="form.retention_days"
              type="number"
              min="1"
              max="3650"
              class="input w-32"
            />
            <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.retentionHint') }}</div>
          </div>
          <div class="sm:ml-auto">
            <button @click="saveConfig" :disabled="savingConfig" class="btn btn-primary">
              <Icon v-if="savingConfig" name="refresh" size="sm" class="animate-spin" />
              {{ t('admin.promptAudit.save') }}
            </button>
          </div>
        </div>
      </div>

      <!-- Status -->
      <div v-if="status" class="mb-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <div class="card px-3 py-2">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.queue') }}</div>
          <div class="mt-0.5 text-sm font-semibold text-gray-900 dark:text-white">{{ status.queue_length }} / {{ status.queue_capacity }}</div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.stored') }}</div>
          <div class="mt-0.5 text-sm font-semibold text-green-600 dark:text-green-400">{{ status.stored }}</div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.dropped') }}</div>
          <div class="mt-0.5 text-sm font-semibold" :class="status.dropped > 0 ? 'text-amber-600 dark:text-amber-400' : 'text-gray-900 dark:text-white'">{{ status.dropped }}</div>
        </div>
        <div class="card px-3 py-2">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.retentionDays') }}</div>
          <div class="mt-0.5 text-sm font-semibold text-gray-900 dark:text-white">{{ status.retention_days }}</div>
        </div>
      </div>

      <!-- Disabled hint -->
      <div v-if="status && !status.enabled" class="mb-6 rounded-lg border border-gray-200 bg-gray-50 px-4 py-3 text-sm text-gray-600 dark:border-dark-500 dark:bg-dark-700 dark:text-gray-300">
        {{ t('admin.promptAudit.disabledTip') }}
      </div>

      <!-- Result status bar -->
      <div v-if="summary" class="mb-6">
        <div class="mb-2 flex items-center gap-2">
          <h2 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.promptAudit.resultBar') }}</h2>
          <span class="text-xs text-gray-400" :title="t('admin.promptAudit.resultBarHint')">
            <Icon name="infoCircle" size="sm" />
          </span>
        </div>
        <div class="grid grid-cols-3 gap-3 sm:grid-cols-6">
          <button
            v-for="card in resultCards"
            :key="card.key || 'all'"
            type="button"
            @click="toggleResultFilter(card.key)"
            class="card px-3 py-2 text-left transition"
            :class="filters.result === card.key ? 'ring-2 ring-primary-500' : 'hover:bg-gray-50 dark:hover:bg-dark-700/50'"
          >
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ card.label }}</div>
            <div class="mt-0.5 text-lg font-semibold tabular-nums" :class="card.class">{{ card.count }}</div>
          </button>
        </div>
      </div>

      <!-- Records -->
      <div class="card p-5">
        <div class="mb-4 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h2 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.promptAudit.records') }}</h2>
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.recordsHint') }}</p>
          </div>
          <button
            v-if="items.length > 0"
            @click="askDeleteAll"
            class="btn btn-secondary btn-sm text-red-600 dark:text-red-400"
          >
            <Icon name="trash" size="sm" />
            {{ t('admin.promptAudit.deleteAll') }}
          </button>
        </div>

        <!-- Filters -->
        <div class="mb-4 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <div class="lg:col-span-2">
            <label class="mb-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.search') }}</label>
            <input v-model="filters.search" @keyup.enter="applyFilters" type="text" class="input w-full" :placeholder="t('admin.promptAudit.searchPlaceholder')" />
          </div>
          <div>
            <label class="mb-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.from') }}</label>
            <input v-model="filters.from" type="date" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.to') }}</label>
            <input v-model="filters.to" type="date" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.result.label') }}</label>
            <select v-model="filters.result" @change="applyFilters" class="input w-full">
              <option value="">{{ t('admin.promptAudit.result.all') }}</option>
              <option value="hit">{{ t('admin.promptAudit.result.hit') }}</option>
              <option value="blocked">{{ t('admin.promptAudit.result.blocked') }}</option>
              <option value="error">{{ t('admin.promptAudit.result.error') }}</option>
              <option value="allow">{{ t('admin.promptAudit.result.allow') }}</option>
              <option value="unaudited">{{ t('admin.promptAudit.result.unaudited') }}</option>
            </select>
          </div>
          <div>
            <label class="mb-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.userId') }}</label>
            <input v-model.number="filters.user_id" type="number" min="1" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.apiKeyId') }}</label>
            <input v-model.number="filters.api_key_id" type="number" min="1" class="input w-full" />
          </div>
          <div>
            <label class="mb-1 block text-xs text-gray-500 dark:text-gray-400">{{ t('admin.promptAudit.groupId') }}</label>
            <input v-model.number="filters.group_id" type="number" min="1" class="input w-full" />
          </div>
          <div class="flex items-end gap-2">
            <button @click="applyFilters" class="btn btn-primary btn-sm">
              <Icon name="search" size="sm" />
              {{ t('admin.promptAudit.search') }}
            </button>
            <button @click="resetFilters" :disabled="listLoading" class="btn btn-secondary btn-sm" :title="t('admin.promptAudit.reset')">
              <Icon name="refresh" size="sm" :class="listLoading ? 'animate-spin' : ''" />
            </button>
          </div>
        </div>

        <!-- Table -->
        <div class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-600">
            <thead>
              <tr class="text-left text-xs text-gray-500 dark:text-gray-400">
                <th class="px-3 py-2 font-medium">{{ t('admin.promptAudit.time') }}</th>
                <th class="px-3 py-2 font-medium">{{ t('admin.promptAudit.user') }}</th>
                <th class="px-3 py-2 font-medium">{{ t('admin.promptAudit.apiKey') }}</th>
                <th class="px-3 py-2 font-medium">{{ t('admin.promptAudit.group') }}</th>
                <th class="px-3 py-2 font-medium">{{ t('admin.promptAudit.result.label') }}</th>
                <th class="px-3 py-2 font-medium">{{ t('admin.promptAudit.model') }}</th>
                <th class="px-3 py-2 font-medium">{{ t('admin.promptAudit.protocol') }}</th>
                <th class="px-3 py-2 text-right font-medium">{{ t('admin.promptAudit.promptLength') }}</th>
                <th class="px-3 py-2 text-right font-medium">{{ t('admin.promptAudit.messageCount') }}</th>
                <th class="px-3 py-2 text-right font-medium">{{ t('admin.promptAudit.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-if="listLoading">
                <td colspan="10" class="px-3 py-8 text-center text-gray-400">
                  <Icon name="refresh" size="lg" class="animate-spin" />
                </td>
              </tr>
              <tr v-else-if="items.length === 0">
                <td colspan="10" class="px-3 py-8 text-center text-gray-400">{{ t('admin.promptAudit.empty') }}</td>
              </tr>
              <tr v-for="ev in items" :key="ev.id" class="text-gray-700 hover:bg-gray-50 dark:text-gray-200 dark:hover:bg-dark-700/50">
                <td class="whitespace-nowrap px-3 py-2 text-xs">{{ formatTime(ev.created_at) }}</td>
                <td class="px-3 py-2">
                  <div class="text-xs">{{ ev.user_email || '-' }}</div>
                  <div v-if="ev.user_id" class="text-[11px] text-gray-400">#{{ ev.user_id }}</div>
                </td>
                <td class="px-3 py-2 text-xs">{{ ev.api_key_name || '-' }}</td>
                <td class="px-3 py-2 text-xs">{{ ev.group_name || '-' }}</td>
                <td class="px-3 py-2">
                  <span
                    class="inline-flex rounded-md px-2 py-1 text-xs font-medium"
                    :class="resultBadgeClass(ev.result)"
                    :title="ev.moderation_category ? t('admin.promptAudit.result.categoryTip', { c: ev.moderation_category }) : ''"
                  >
                    {{ resultLabel(ev.result) }}
                  </span>
                </td>
                <td class="px-3 py-2 text-xs">{{ ev.model || '-' }}</td>
                <td class="px-3 py-2 text-xs">{{ ev.protocol || '-' }}</td>
                <td class="px-3 py-2 text-right text-xs tabular-nums">{{ ev.prompt_length }}</td>
                <td class="px-3 py-2 text-right text-xs tabular-nums">{{ ev.message_count }}</td>
                <td class="whitespace-nowrap px-3 py-2 text-right">
                  <button @click="openDetail(ev)" class="btn btn-secondary btn-sm" :title="t('admin.promptAudit.view')">
                    <Icon name="eye" size="sm" />
                  </button>
                  <button @click="askDelete(ev)" class="btn btn-secondary btn-sm ml-1 text-red-600 dark:text-red-400" :title="t('admin.promptAudit.delete')">
                    <Icon name="trash" size="sm" />
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- Pagination -->
        <div v-if="total > 0" class="mt-4">
          <Pagination
            :total="total"
            :page="page"
            :page-size="pageSize"
            @update:page="onPageChange"
            @update:page-size="onPageSizeChange"
          />
        </div>
      </div>
    </div>

    <!-- Detail dialog -->
    <BaseDialog :show="detailOpen" :title="t('admin.promptAudit.detail')" width="extra-wide" @close="detailOpen = false">
      <div v-if="detail" class="space-y-4">
        <div class="grid grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-3">
          <div><span class="text-gray-400">{{ t('admin.promptAudit.time') }}:</span> {{ formatTime(detail.created_at) }}</div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.user') }}:</span> {{ detail.user_email || '-' }}</div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.apiKey') }}:</span> {{ detail.api_key_name || '-' }}</div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.group') }}:</span> {{ detail.group_name || '-' }}</div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.model') }}:</span> {{ detail.model || '-' }}</div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.protocol') }}:</span> {{ detail.protocol || '-' }}</div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.endpoint') }}:</span> {{ detail.endpoint || '-' }}</div>
          <div>
            <span class="text-gray-400">{{ t('admin.promptAudit.result.label') }}:</span>
            <span class="ml-1 inline-flex rounded-md px-1.5 py-0.5 text-xs font-medium" :class="resultBadgeClass(detail.result)">{{ resultLabel(detail.result) }}</span>
            <span v-if="detail.moderation_category" class="ml-1 text-gray-400">· {{ detail.moderation_category }}</span>
          </div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.messageCount') }}:</span> {{ detail.message_count }}</div>
          <div><span class="text-gray-400">{{ t('admin.promptAudit.promptLength') }}:</span> {{ detail.prompt_length }}</div>
          <div class="col-span-2 sm:col-span-3 break-all"><span class="text-gray-400">{{ t('admin.promptAudit.requestId') }}:</span> {{ detail.request_id || '-' }}</div>
        </div>
        <div>
          <div class="mb-1 flex items-center justify-between">
            <span class="text-sm font-medium text-gray-800 dark:text-gray-100">{{ t('admin.promptAudit.fullPrompt') }}</span>
            <button @click="copyPrompt" class="btn btn-secondary btn-sm">
              <Icon name="copy" size="sm" />
              {{ copied ? t('admin.promptAudit.copied') : t('admin.promptAudit.copy') }}
            </button>
          </div>
          <pre class="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-lg border border-gray-200 bg-gray-50 p-3 text-xs text-gray-800 dark:border-dark-600 dark:bg-dark-900 dark:text-gray-200">{{ detail.full_prompt }}</pre>
        </div>
      </div>
    </BaseDialog>

    <!-- Delete confirm -->
    <ConfirmDialog
      :show="confirmOpen"
      :title="t('admin.promptAudit.delete')"
      :message="confirmMessage"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="onConfirm"
      @cancel="confirmOpen = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { useAppStore } from '@/stores'
import {
  promptAuditAPI,
  type PromptAuditEvent,
  type PromptAuditStatus,
  type PromptAuditResult,
  type PromptAuditResultSummary,
  type ListPromptAuditEventsParams,
  type PromptAuditSummaryParams,
} from '@/api/admin/promptAudit'

const { t } = useI18n()
const appStore = useAppStore()

const form = reactive<{ enabled: boolean; retention_days: number }>({ enabled: false, retention_days: 30 })
const status = ref<PromptAuditStatus | null>(null)
const statusLoading = ref(false)
const savingConfig = ref(false)

const items = ref<PromptAuditEvent[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const listLoading = ref(false)
const summary = ref<PromptAuditResultSummary | null>(null)

const filters = reactive<{
  search: string
  result: PromptAuditResult | ''
  from: string
  to: string
  user_id: number | null
  api_key_id: number | null
  group_id: number | null
}>({
  search: '',
  result: '',
  from: '',
  to: '',
  user_id: null,
  api_key_id: null,
  group_id: null,
})

// 结果徽章：与风控中心保持一致的配色/语义。
function resultLabel(r: PromptAuditResult): string {
  return t(`admin.promptAudit.result.${r}`)
}
function resultBadgeClass(r: PromptAuditResult): string {
  switch (r) {
    case 'blocked':
      return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'error':
      return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
    case 'hit':
      return 'bg-pink-100 text-pink-700 dark:bg-pink-900/30 dark:text-pink-300'
    case 'allow':
      return 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
    default:
      return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
  }
}

// 结果状态栏卡片（key 为空串=清除过滤，展示总数）。
const resultCards = computed(() => {
  const s = summary.value
  if (!s) return []
  return [
    { key: '' as const, label: t('admin.promptAudit.result.all'), count: s.total, class: 'text-gray-900 dark:text-white' },
    { key: 'hit' as const, label: t('admin.promptAudit.result.hit'), count: s.hit, class: 'text-pink-600 dark:text-pink-400' },
    { key: 'blocked' as const, label: t('admin.promptAudit.result.blocked'), count: s.blocked, class: 'text-red-600 dark:text-red-400' },
    { key: 'error' as const, label: t('admin.promptAudit.result.error'), count: s.error, class: 'text-amber-600 dark:text-amber-400' },
    { key: 'allow' as const, label: t('admin.promptAudit.result.allow'), count: s.allow, class: 'text-green-600 dark:text-green-400' },
    { key: 'unaudited' as const, label: t('admin.promptAudit.result.unaudited'), count: s.unaudited, class: 'text-gray-500 dark:text-gray-400' },
  ]
})

function toggleResultFilter(key: PromptAuditResult | '') {
  filters.result = filters.result === key ? '' : key
  applyFilters()
}

const detailOpen = ref(false)
const detail = ref<PromptAuditEvent | null>(null)
const copied = ref(false)

const confirmOpen = ref(false)
const confirmMode = ref<'one' | 'all'>('one')
const pendingId = ref<number | null>(null)
const confirmMessage = computed(() =>
  confirmMode.value === 'all' ? t('admin.promptAudit.deleteAllConfirm') : t('admin.promptAudit.deleteConfirm')
)

function formatTime(iso: string): string {
  if (!iso) return '-'
  try {
    return new Date(iso).toLocaleString()
  } catch {
    return iso
  }
}

function buildSummaryParams(): PromptAuditSummaryParams {
  const p: PromptAuditSummaryParams = {}
  if (filters.search.trim()) p.search = filters.search.trim()
  if (filters.from) p.from = filters.from
  if (filters.to) p.to = filters.to
  if (filters.user_id && filters.user_id > 0) p.user_id = filters.user_id
  if (filters.api_key_id && filters.api_key_id > 0) p.api_key_id = filters.api_key_id
  if (filters.group_id && filters.group_id > 0) p.group_id = filters.group_id
  return p
}

function buildParams(): ListPromptAuditEventsParams {
  const p: ListPromptAuditEventsParams = { ...buildSummaryParams(), page: page.value, page_size: pageSize.value }
  if (filters.result) p.result = filters.result
  return p
}

async function loadConfig() {
  try {
    const cfg = await promptAuditAPI.getConfig()
    form.enabled = cfg.enabled
    form.retention_days = cfg.retention_days
  } catch {
    appStore.showError(t('admin.promptAudit.loadFailed'))
  }
}

async function loadStatus() {
  statusLoading.value = true
  try {
    status.value = await promptAuditAPI.getStatus()
  } catch {
    // 非致命，忽略
  } finally {
    statusLoading.value = false
  }
}

async function loadList() {
  listLoading.value = true
  try {
    const resp = await promptAuditAPI.listEvents(buildParams())
    items.value = resp.items ?? []
    total.value = resp.total ?? 0
    page.value = resp.page ?? page.value
    pageSize.value = resp.page_size ?? pageSize.value
  } catch {
    appStore.showError(t('admin.promptAudit.loadFailed'))
  } finally {
    listLoading.value = false
  }
}

async function loadSummary() {
  try {
    summary.value = await promptAuditAPI.getSummary(buildSummaryParams())
  } catch {
    // 非致命，忽略（状态栏缺失不影响列表）
  }
}

async function refreshAll() {
  await Promise.all([loadStatus(), loadSummary(), loadList()])
}

async function saveConfig() {
  savingConfig.value = true
  try {
    const cfg = await promptAuditAPI.updateConfig({ enabled: form.enabled, retention_days: form.retention_days })
    form.enabled = cfg.enabled
    form.retention_days = cfg.retention_days
    appStore.showSuccess(t('admin.promptAudit.saved'))
    await loadStatus()
  } catch {
    appStore.showError(t('admin.promptAudit.saveFailed'))
  } finally {
    savingConfig.value = false
  }
}

function applyFilters() {
  page.value = 1
  loadList()
  loadSummary()
}

function resetFilters() {
  filters.search = ''
  filters.result = ''
  filters.from = ''
  filters.to = ''
  filters.user_id = null
  filters.api_key_id = null
  filters.group_id = null
  page.value = 1
  loadList()
  loadSummary()
}

function onPageChange(p: number) {
  page.value = p
  loadList()
}

function onPageSizeChange(ps: number) {
  pageSize.value = ps
  page.value = 1
  loadList()
}

async function openDetail(ev: PromptAuditEvent) {
  copied.value = false
  detailOpen.value = true
  detail.value = ev
  try {
    detail.value = await promptAuditAPI.getEvent(ev.id)
  } catch {
    appStore.showError(t('admin.promptAudit.loadFailed'))
  }
}

async function copyPrompt() {
  if (!detail.value) return
  try {
    await navigator.clipboard.writeText(detail.value.full_prompt || '')
    copied.value = true
    setTimeout(() => (copied.value = false), 1500)
  } catch {
    // ignore clipboard failure
  }
}

function askDelete(ev: PromptAuditEvent) {
  confirmMode.value = 'one'
  pendingId.value = ev.id
  confirmOpen.value = true
}

function askDeleteAll() {
  confirmMode.value = 'all'
  pendingId.value = null
  confirmOpen.value = true
}

async function onConfirm() {
  try {
    if (confirmMode.value === 'all') {
      const res = await promptAuditAPI.deleteAll()
      appStore.showSuccess(t('admin.promptAudit.deleteAllDone', { n: res.deleted }))
    } else if (pendingId.value != null) {
      await promptAuditAPI.deleteEvent(pendingId.value)
      appStore.showSuccess(t('admin.promptAudit.deleted'))
    }
    confirmOpen.value = false
    await refreshAll()
  } catch {
    appStore.showError(t('admin.promptAudit.saveFailed'))
    confirmOpen.value = false
  }
}

onMounted(async () => {
  await loadConfig()
  await refreshAll()
})
</script>
