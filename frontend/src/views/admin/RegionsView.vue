<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
            <button
              @click="load"
              :disabled="loading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button @click="openCreate" class="btn btn-primary">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t('admin.regions.create') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable :columns="columns" :data="regions" :loading="loading">
          <template #cell-code="{ value }">
            <code class="code text-xs">{{ value }}</code>
          </template>
          <template #cell-enabled="{ value }">
            <span :class="['badge', value ? 'badge-success' : 'badge-gray']">
              {{ value ? t('admin.regions.enabled') : t('admin.regions.disabled') }}
            </span>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex items-center gap-1">
              <button
                class="rounded p-1 text-gray-400 hover:text-primary-600"
                :title="t('common.edit')"
                @click="openEdit(row)"
              >
                <Icon name="edit" size="sm" />
              </button>
              <button
                class="rounded p-1 text-gray-400 hover:text-danger-600"
                :title="t('common.delete')"
                @click="askDelete(row)"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </template>
        </DataTable>
      </template>
    </TablePageLayout>

    <!-- Create/Edit Dialog -->
    <BaseDialog
      :show="showEdit"
      :title="isEditing ? t('admin.regions.edit') : t('admin.regions.create')"
      @close="closeEdit"
    >
      <form id="region-form" @submit.prevent="save" class="space-y-4">
        <div>
          <label class="input-label">{{ t('admin.regions.code') }}</label>
          <input
            v-model="form.code"
            type="text"
            class="input uppercase"
            :placeholder="t('admin.regions.codePlaceholder')"
            required
          />
          <p class="input-hint mt-1">{{ t('admin.regions.codeHint') }}</p>
        </div>
        <div class="grid grid-cols-2 gap-3">
          <div>
            <label class="input-label">{{ t('admin.regions.nameEn') }}</label>
            <input v-model="form.name_en" type="text" class="input" placeholder="San Jose" required />
          </div>
          <div>
            <label class="input-label">{{ t('admin.regions.nameZh') }}</label>
            <input v-model="form.name_zh" type="text" class="input" placeholder="圣何塞" required />
          </div>
        </div>
        <div class="grid grid-cols-2 gap-3">
          <div>
            <label class="input-label">{{ t('admin.regions.sortOrder') }}</label>
            <input v-model.number="form.sort_order" type="number" class="input" />
          </div>
          <div class="flex items-end">
            <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-200">
              <input v-model="form.enabled" type="checkbox" class="h-4 w-4" />
              {{ t('admin.regions.enabledLabel') }}
            </label>
          </div>
        </div>
        <p v-if="formError" class="text-sm text-danger-600">{{ formError }}</p>
      </form>
      <template #footer>
        <button class="btn btn-secondary" @click="closeEdit">{{ t('common.cancel') }}</button>
        <button type="submit" form="region-form" class="btn btn-primary" :disabled="saving">
          {{ t('common.save') }}
        </button>
      </template>
    </BaseDialog>

    <!-- Delete Confirmation -->
    <ConfirmDialog
      :show="showDelete"
      :title="t('admin.regions.delete')"
      :message="t('admin.regions.deleteConfirm')"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmDelete"
      @cancel="showDelete = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { Region } from '@/types'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()

const regions = ref<Region[]>([])
const loading = ref(false)
const saving = ref(false)
const showEdit = ref(false)
const showDelete = ref(false)
const isEditing = ref(false)
const editingId = ref<number | null>(null)
const deletingId = ref<number | null>(null)
const formError = ref('')

const form = reactive({
  code: '',
  name_en: '',
  name_zh: '',
  sort_order: 0,
  enabled: true
})

const columns = computed<Column[]>(() => [
  { key: 'code', label: t('admin.regions.code'), sortable: false },
  { key: 'name_en', label: t('admin.regions.nameEn'), sortable: false },
  { key: 'name_zh', label: t('admin.regions.nameZh'), sortable: false },
  { key: 'sort_order', label: t('admin.regions.sortOrder'), sortable: false },
  { key: 'enabled', label: t('admin.regions.status'), sortable: false },
  { key: 'actions', label: t('common.actions'), sortable: false }
])

async function load() {
  loading.value = true
  try {
    regions.value = await adminAPI.regions.list()
  } finally {
    loading.value = false
  }
}

function resetForm() {
  form.code = ''
  form.name_en = ''
  form.name_zh = ''
  form.sort_order = 0
  form.enabled = true
  formError.value = ''
}

function openCreate() {
  resetForm()
  isEditing.value = false
  editingId.value = null
  showEdit.value = true
}

function openEdit(row: Region) {
  form.code = row.code
  form.name_en = row.name_en
  form.name_zh = row.name_zh
  form.sort_order = row.sort_order
  form.enabled = row.enabled
  formError.value = ''
  isEditing.value = true
  editingId.value = row.id
  showEdit.value = true
}

function closeEdit() {
  showEdit.value = false
}

async function save() {
  formError.value = ''
  saving.value = true
  try {
    const payload = {
      code: form.code.trim().toUpperCase(),
      name_en: form.name_en.trim(),
      name_zh: form.name_zh.trim(),
      sort_order: form.sort_order,
      enabled: form.enabled
    }
    if (isEditing.value && editingId.value != null) {
      await adminAPI.regions.update(editingId.value, payload)
    } else {
      await adminAPI.regions.create(payload)
    }
    showEdit.value = false
    await load()
  } catch (e: unknown) {
    formError.value = (e as { message?: string })?.message || t('admin.regions.saveError')
  } finally {
    saving.value = false
  }
}

function askDelete(row: Region) {
  deletingId.value = row.id
  showDelete.value = true
}

async function confirmDelete() {
  if (deletingId.value == null) return
  try {
    await adminAPI.regions.remove(deletingId.value)
    showDelete.value = false
    await load()
  } finally {
    deletingId.value = null
  }
}

onMounted(load)
</script>
