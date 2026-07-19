<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="filter-bar">
          <Select
            v-model="statusFilter"
            :options="statusFilterOptions"
            class="status-select"
            @change="handleStatusChange"
          />
          <button class="btn btn-secondary" :disabled="loading" @click="reload">
            <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />
            {{ t('common.refresh') }}
          </button>
        </div>
      </template>

      <template #table>
        <DataTable
          :columns="columns"
          :data="feedbacks"
          :loading="loading"
          server-side-sort
          default-sort-key="created_at"
          default-sort-order="desc"
          @sort="handleSort"
        >
          <template #cell-type="{ row }">
            {{ typeLabel((row as Feedback).type) }}
          </template>
          <template #cell-content="{ row }">
            <span class="content-cell">{{ (row as Feedback).content }}</span>
          </template>
          <template #cell-status="{ row }">
            <span
              :class="[
                'badge',
                (row as Feedback).status === 'resolved' ? 'badge-success' : 'badge-warning',
              ]"
            >
              {{ statusLabel((row as Feedback).status) }}
            </span>
          </template>
          <template #cell-created_at="{ row }">
            {{ formatDateTime((row as Feedback).created_at) }}
          </template>
          <template #cell-actions="{ row }">
            <div class="row-actions">
              <button class="icon-btn" :title="t('admin.feedbacks.viewDetail')" @click="openDetail(row as Feedback)">
                <Icon name="eye" size="sm" />
              </button>
              <button
                v-if="(row as Feedback).status === 'pending'"
                class="icon-btn"
                :title="t('admin.feedbacks.markResolved')"
                @click="changeStatus(row as Feedback, 'resolved')"
              >
                <Icon name="check" size="sm" />
              </button>
              <button
                v-else
                class="icon-btn"
                :title="t('admin.feedbacks.markPending')"
                @click="changeStatus(row as Feedback, 'pending')"
              >
                <Icon name="refresh" size="sm" />
              </button>
            </div>
          </template>
          <template #empty>
            <EmptyState :title="t('admin.feedbacks.empty')" />
          </template>
        </DataTable>
      </template>

      <template #pagination>
        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:page-size="handlePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <!-- Detail dialog -->
    <BaseDialog :show="showDetail" :title="t('admin.feedbacks.detailTitle')" @close="showDetail = false">
      <div v-if="detail" class="detail">
        <div class="detail-row">
          <span class="detail-label">{{ t('admin.feedbacks.columns.type') }}</span>
          <span>{{ typeLabel(detail.type) }}</span>
        </div>
        <div class="detail-row">
          <span class="detail-label">{{ t('admin.feedbacks.columns.status') }}</span>
          <span
            :class="['badge', detail.status === 'resolved' ? 'badge-success' : 'badge-warning']"
          >
            {{ statusLabel(detail.status) }}
          </span>
        </div>
        <div class="detail-row">
          <span class="detail-label">{{ t('admin.feedbacks.columns.user') }}</span>
          <span>{{ detail.user_email || `#${detail.user_id}` }}</span>
        </div>
        <div v-if="detail.request_id" class="detail-row">
          <span class="detail-label">Request ID</span>
          <span class="detail-mono">{{ detail.request_id }}</span>
        </div>
        <div class="detail-row">
          <span class="detail-label">{{ t('admin.feedbacks.columns.createdAt') }}</span>
          <span>{{ formatDateTime(detail.created_at) }}</span>
        </div>
        <div class="detail-content">
          <span class="detail-label">{{ t('admin.feedbacks.columns.content') }}</span>
          <p>{{ detail.content }}</p>
        </div>
        <div class="detail-content">
          <span class="detail-label">{{ t('admin.feedbacks.replyLabel') }}</span>
          <textarea
            v-model="replyText"
            class="reply-textarea"
            :placeholder="t('admin.feedbacks.replyPlaceholder')"
            rows="4"
          />
          <span v-if="detail.replied_at" class="reply-meta">
            {{ t('admin.feedbacks.repliedAt') }}: {{ formatDateTime(detail.replied_at) }}
          </span>
        </div>
      </div>
      <template #footer>
        <button class="btn btn-secondary" @click="showDetail = false">{{ t('common.close') }}</button>
        <button
          class="btn btn-primary"
          :disabled="replying || !replyText.trim()"
          @click="submitReply"
        >
          {{ replying ? t('common.saving') : t('admin.feedbacks.submitReply') }}
        </button>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Column } from '@/components/common/types'
import { adminAPI } from '@/api/admin'
import type { Feedback } from '@/types'
import { useAppStore } from '@/stores/app'
import { getPersistedPageSize } from '@/composables/usePersistedPageSize'
import { formatDateTime } from '@/utils/format'

const { t } = useI18n()
const appStore = useAppStore()

const feedbacks = ref<Feedback[]>([])
const loading = ref(false)
const statusFilter = ref('')
const pagination = reactive({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0,
})
const sortState = reactive({ sort_by: 'created_at', sort_order: 'desc' as 'asc' | 'desc' })

const showDetail = ref(false)
const detail = ref<Feedback | null>(null)
const replyText = ref('')
const replying = ref(false)

let currentController: AbortController | null = null

const statusFilterOptions = computed(() => [
  { value: '', label: t('admin.feedbacks.allStatus') },
  { value: 'pending', label: t('admin.feedbacks.statusLabels.pending') },
  { value: 'resolved', label: t('admin.feedbacks.statusLabels.resolved') },
])

const columns = computed<Column[]>(() => [
  { key: 'type', label: t('admin.feedbacks.columns.type'), sortable: false },
  { key: 'content', label: t('admin.feedbacks.columns.content'), sortable: false },
  { key: 'status', label: t('admin.feedbacks.columns.status'), sortable: false },
  { key: 'created_at', label: t('admin.feedbacks.columns.createdAt'), sortable: true },
  { key: 'actions', label: t('admin.feedbacks.columns.actions'), sortable: false },
])

function typeLabel(type: string): string {
  const key = `admin.feedbacks.typeLabels.${type}`
  const label = t(key)
  return label === key ? type : label
}

function statusLabel(status: string): string {
  return status === 'resolved'
    ? t('admin.feedbacks.statusLabels.resolved')
    : t('admin.feedbacks.statusLabels.pending')
}

async function loadFeedbacks() {
  currentController?.abort()
  const controller = new AbortController()
  currentController = controller
  loading.value = true
  try {
    const res = await adminAPI.feedbacks.list(
      pagination.page,
      pagination.page_size,
      {
        status: statusFilter.value || undefined,
        sort_by: sortState.sort_by,
        sort_order: sortState.sort_order,
      },
      { signal: controller.signal },
    )
    if (controller.signal.aborted || currentController !== controller) return
    feedbacks.value = res.items
    pagination.total = res.total
  } catch (err: unknown) {
    if (controller.signal.aborted) return
    appStore.showError(
      (err as { message?: string })?.message || t('admin.feedbacks.failedToLoad'),
    )
  } finally {
    if (currentController === controller) loading.value = false
  }
}

function reload() {
  loadFeedbacks()
}

function handleStatusChange() {
  pagination.page = 1
  loadFeedbacks()
}

function handlePageChange(page: number) {
  pagination.page = page
  loadFeedbacks()
}

function handlePageSizeChange(size: number) {
  pagination.page_size = size
  pagination.page = 1
  loadFeedbacks()
}

function handleSort(key: string, order: 'asc' | 'desc') {
  sortState.sort_by = key
  sortState.sort_order = order
  loadFeedbacks()
}

function openDetail(row: Feedback) {
  detail.value = row
  replyText.value = row.admin_reply || ''
  showDetail.value = true
}

async function submitReply() {
  if (!detail.value || !replyText.value.trim() || replying.value) return
  replying.value = true
  try {
    const updated = await adminAPI.feedbacks.reply(detail.value.id, replyText.value.trim())
    const idx = feedbacks.value.findIndex((f) => f.id === updated.id)
    if (idx !== -1) feedbacks.value[idx] = updated
    detail.value = updated
    appStore.showSuccess(t('common.success'))
  } catch (err: unknown) {
    appStore.showError(
      (err as { message?: string })?.message || t('admin.feedbacks.failedToReply'),
    )
  } finally {
    replying.value = false
  }
}

async function changeStatus(row: Feedback, status: 'pending' | 'resolved') {
  try {
    const updated = await adminAPI.feedbacks.updateStatus(row.id, status)
    // update in-place
    const idx = feedbacks.value.findIndex((f) => f.id === row.id)
    if (idx !== -1) feedbacks.value[idx] = updated
    if (detail.value?.id === row.id) detail.value = updated
    appStore.showSuccess(t('common.success'))
  } catch (err: unknown) {
    appStore.showError(
      (err as { message?: string })?.message || t('admin.feedbacks.failedToUpdate'),
    )
  }
}

onMounted(loadFeedbacks)
onUnmounted(() => currentController?.abort())
</script>

<style scoped>
.filter-bar {
  display: flex;
  align-items: center;
  gap: 12px;
}

.status-select {
  width: 160px;
}

.content-cell {
  display: inline-block;
  max-width: 360px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: bottom;
}

.row-actions {
  display: flex;
  gap: 6px;
}

.icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border: 1px solid var(--color-border, #e5e7eb);
  border-radius: 6px;
  background: transparent;
  cursor: pointer;
  color: var(--color-text-secondary, #6b7280);
}

.icon-btn:hover {
  color: var(--color-text, #111827);
}

.detail {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.detail-row {
  display: flex;
  gap: 12px;
  align-items: center;
}

.detail-label {
  min-width: 88px;
  color: var(--color-text-secondary, #6b7280);
  font-size: 12px;
}

.detail-mono {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
}

.detail-content {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.detail-content p {
  margin: 0;
  white-space: pre-wrap;
  word-break: break-word;
  line-height: 1.6;
}

.reply-textarea {
  width: 100%;
  border: 1px solid var(--color-border, #e5e7eb);
  border-radius: 8px;
  padding: 10px 12px;
  font: inherit;
  font-size: 13px;
  resize: vertical;
  color: var(--color-text, #111827);
  background: var(--color-surface, #fff);
}

.reply-textarea:focus {
  outline: none;
  border-color: var(--color-primary, #6366f1);
}

.reply-meta {
  font-size: 11px;
  color: var(--color-text-secondary, #6b7280);
}
</style>
