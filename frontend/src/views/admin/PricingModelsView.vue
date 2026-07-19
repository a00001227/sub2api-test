<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-8">
      <!-- Header -->
      <div class="mb-6 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.pricingDisplay.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.pricingDisplay.description') }}
          </p>
        </div>
        <div class="flex items-center gap-3">
          <button @click="loadModels" :disabled="loading" class="btn btn-secondary" :title="t('common.refresh', 'Refresh')">
            <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
          </button>
          <button @click="openCreate" class="btn btn-primary">
            <Icon name="plus" size="md" class="mr-2" />
            {{ t('admin.pricingDisplay.addModel') }}
          </button>
        </div>
      </div>

      <!-- Filter bar -->
      <div class="mb-4 flex flex-wrap items-center gap-3">
        <select v-model="filterType" class="input w-36">
          <option value="">{{ t('admin.pricingDisplay.allTypes') }}</option>
          <option value="text">{{ t('admin.pricingDisplay.typeText') }}</option>
          <option value="image">{{ t('admin.pricingDisplay.typeImage') }}</option>
        </select>
        <select v-model="filterUserType" class="input w-40">
          <option value="">{{ t('admin.pricingDisplay.allUserTypes') }}</option>
          <option value="end_user">{{ t('admin.pricingDisplay.userTypeEndUser') }}</option>
          <option value="channel_user">{{ t('admin.pricingDisplay.userTypeChannelUser') }}</option>
        </select>
        <select v-model="filterEnabled" class="input w-36">
          <option value="">{{ t('admin.pricingDisplay.allStatus') }}</option>
          <option value="true">{{ t('admin.pricingDisplay.enabled') }}</option>
          <option value="false">{{ t('admin.pricingDisplay.disabled') }}</option>
        </select>
      </div>

      <!-- Table -->
      <div class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-500">
        <div v-if="loading" class="flex items-center justify-center py-16">
          <Icon name="refresh" size="xl" class="animate-spin text-gray-400" />
        </div>
        <div v-else-if="filteredModels.length === 0" class="py-16 text-center text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.pricingDisplay.noModels') }}
        </div>
        <table v-else class="min-w-full divide-y divide-gray-200 dark:divide-dark-500">
          <thead class="bg-gray-50 dark:bg-dark-700">
            <tr>
              <th class="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colModel') }}</th>
              <th class="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colType') }}</th>
              <th class="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colUserType') }}</th>
              <th class="px-4 py-3 text-right text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colInputOutput') }}</th>
              <th class="px-4 py-3 text-right text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colSaving') }}</th>
              <th class="px-4 py-3 text-center text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colEnabled') }}</th>
              <th class="px-4 py-3 text-right text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colUpdated') }}</th>
              <th class="px-4 py-3 text-right text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.colActions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-600 dark:bg-dark-800">
            <tr v-for="m in filteredModels" :key="m.id" class="hover:bg-gray-50 dark:hover:bg-dark-700">
              <td class="px-4 py-3 font-mono text-sm font-medium text-gray-900 dark:text-white">{{ m.model }}</td>
              <td class="px-4 py-3">
                <span :class="typeBadgeClass(m.model_type)" class="rounded px-2 py-0.5 text-xs font-medium">
                  {{ m.model_type === 'text' ? t('admin.pricingDisplay.typeText') : t('admin.pricingDisplay.typeImage') }}
                </span>
              </td>
              <td class="px-4 py-3 text-sm text-gray-600 dark:text-gray-300">
                {{ m.user_type === 'end_user' ? t('admin.pricingDisplay.userTypeEndUser') : t('admin.pricingDisplay.userTypeChannelUser') }}
              </td>
              <td class="px-4 py-3 text-right font-mono text-xs text-gray-700 dark:text-gray-300">
                <template v-if="m.model_type === 'text'">
                  <span class="block">in: {{ fmtPrice(m.input_price) }}</span>
                  <span class="block">out: {{ fmtPrice(m.output_price) }}</span>
                </template>
                <template v-else>
                  <span class="block text-gray-400 dark:text-gray-500">
                    {{ t('admin.pricingDisplay.resolutions', { n: imageResolutionCount(m) }) }}
                  </span>
                </template>
              </td>
              <td class="px-4 py-3 text-right text-sm font-semibold" :class="savingClass(m.saving_percent)">
                {{ fmtPercent(m.saving_percent) }}
              </td>
              <td class="px-4 py-3 text-center">
                <button
                  @click="toggleEnabled(m)"
                  :disabled="toggling.has(m.id)"
                  :class="m.enabled ? 'bg-green-500 hover:bg-green-600' : 'bg-gray-300 hover:bg-gray-400 dark:bg-dark-500'"
                  class="relative inline-flex h-5 w-9 items-center rounded-full transition-colors focus:outline-none disabled:opacity-50"
                >
                  <span
                    :class="m.enabled ? 'translate-x-4' : 'translate-x-0.5'"
                    class="inline-block h-4 w-4 transform rounded-full bg-white transition-transform"
                  />
                </button>
              </td>
              <td class="px-4 py-3 text-right text-xs text-gray-500 dark:text-gray-400">
                {{ fmtDate(m.updated_at) }}
              </td>
              <td class="px-4 py-3 text-right">
                <div class="flex items-center justify-end gap-2">
                  <button @click="openEdit(m)" class="btn btn-secondary btn-xs">{{ t('admin.pricingDisplay.edit') }}</button>
                  <button @click="confirmDelete(m)" class="btn btn-danger btn-xs">{{ t('admin.pricingDisplay.delete') }}</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Create / Edit Dialog -->
    <PricingModelDialog
      v-if="dialogOpen"
      :record="editingRecord"
      @close="dialogOpen = false"
      @saved="onSaved"
    />

    <!-- Delete confirmation -->
    <div
      v-if="deleteTarget"
      class="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      @click.self="deleteTarget = null"
    >
      <div class="w-full max-w-sm rounded-lg bg-white p-6 shadow-xl dark:bg-dark-800">
        <h3 class="mb-2 text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.pricingDisplay.deleteTitle') }}</h3>
        <p class="mb-4 text-sm text-gray-600 dark:text-gray-400">
          {{ t('admin.pricingDisplay.deleteConfirm', { model: deleteTarget.model, userType: deleteTarget.user_type }) }}
        </p>
        <div class="flex justify-end gap-3">
          <button @click="deleteTarget = null" class="btn btn-secondary">{{ t('admin.pricingDisplay.cancel') }}</button>
          <button @click="doDelete" :disabled="deleteLoading" class="btn btn-danger">
            {{ deleteLoading ? t('admin.pricingDisplay.deleting') : t('admin.pricingDisplay.delete') }}
          </button>
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
import PricingModelDialog from '@/components/admin/pricing/PricingModelDialog.vue'
import { pricingApi, type PricingModelRecord } from '@/api/admin/pricing'
import { useAppStore } from '@/stores/app'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const models = ref<PricingModelRecord[]>([])
const filterType = ref('')
const filterUserType = ref('')
const filterEnabled = ref('')
const dialogOpen = ref(false)
const editingRecord = ref<PricingModelRecord | null>(null)
const deleteTarget = ref<PricingModelRecord | null>(null)
const deleteLoading = ref(false)

const filteredModels = computed(() => {
  return models.value.filter((m) => {
    if (filterType.value && m.model_type !== filterType.value) return false
    if (filterUserType.value && m.user_type !== filterUserType.value) return false
    if (filterEnabled.value !== '') {
      const en = filterEnabled.value === 'true'
      if (m.enabled !== en) return false
    }
    return true
  })
})

async function loadModels() {
  loading.value = true
  try {
    models.value = await pricingApi.listModels()
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.pricingDisplay.loadFail'))
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingRecord.value = null
  dialogOpen.value = true
}

function openEdit(m: PricingModelRecord) {
  editingRecord.value = { ...m }
  dialogOpen.value = true
}

function onSaved() {
  dialogOpen.value = false
  loadModels()
}

function confirmDelete(m: PricingModelRecord) {
  deleteTarget.value = m
}

async function doDelete() {
  if (!deleteTarget.value) return
  deleteLoading.value = true
  try {
    await pricingApi.deleteModel(deleteTarget.value.id)
    appStore.showSuccess(t('admin.pricingDisplay.deleteSuccess'))
    deleteTarget.value = null
    loadModels()
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.pricingDisplay.deleteFail'))
  } finally {
    deleteLoading.value = false
  }
}

const toggling = ref(new Set<number>())

async function toggleEnabled(m: PricingModelRecord) {
  if (toggling.value.has(m.id)) return
  toggling.value.add(m.id)
  try {
    await pricingApi.updateModel(m.id, { enabled: !m.enabled })
    m.enabled = !m.enabled
    appStore.showSuccess(m.enabled ? t('admin.pricingDisplay.modelEnabled') : t('admin.pricingDisplay.modelDisabled'))
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.pricingDisplay.toggleFail'))
  } finally {
    toggling.value.delete(m.id)
  }
}

function typeBadgeClass(type: string) {
  return type === 'text'
    ? 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
    : 'bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-300'
}

function savingClass(pct: number) {
  if (pct >= 0.5) return 'text-green-600 dark:text-green-400'
  if (pct > 0) return 'text-yellow-600 dark:text-yellow-400'
  return 'text-gray-500 dark:text-gray-400'
}

function fmtPrice(v: number | null) {
  if (v == null) return '—'
  return `$${v.toFixed(4)}/MTok`
}

function fmtPercent(v: number) {
  if (v == null) return '—'
  if (!isFinite(v) || v === 0) return '—'
  return `${(v * 100).toFixed(1)}%`
}

function fmtDate(s: string) {
  return new Date(s).toLocaleDateString()
}

function imageResolutionCount(m: PricingModelRecord) {
  return Object.keys(m.image_resolutions ?? {}).length
}

onMounted(loadModels)
</script>
