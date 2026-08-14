<template>
  <div class="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/50 py-8" @click.self="$emit('close')">
    <div class="flex max-h-[85vh] w-full max-w-2xl flex-col rounded-lg bg-white shadow-xl dark:bg-dark-800">
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-gray-200 px-6 py-4 dark:border-dark-600">
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.pricingDisplay.catalog.title') }}</h2>
          <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.catalog.subtitle') }}</p>
        </div>
        <button @click="$emit('close')" class="text-gray-400 hover:text-gray-600 dark:hover:text-gray-200">
          <Icon name="x" size="md" />
        </button>
      </div>

      <!-- Toolbar -->
      <div class="space-y-3 border-b border-gray-100 px-6 py-4 dark:border-dark-600">
        <div class="flex items-center justify-between gap-3">
          <!-- Platform tabs -->
          <div class="flex flex-wrap gap-1.5">
            <button
              v-for="p in platforms"
              :key="p.value || 'all'"
              type="button"
              @click="platform = p.value"
              :class="platform === p.value
                ? 'bg-primary-600 text-white'
                : 'bg-gray-100 text-gray-600 hover:bg-gray-200 dark:bg-dark-700 dark:text-gray-300 dark:hover:bg-dark-600'"
              class="rounded-full px-3 py-1 text-xs font-medium transition-colors"
            >
              {{ p.label }}
            </button>
          </div>
          <button @click="doSync" :disabled="syncing" class="btn btn-secondary btn-sm shrink-0">
            <Icon name="refresh" size="sm" :class="['mr-1', syncing ? 'animate-spin' : '']" />
            {{ t('admin.pricingDisplay.catalog.sync') }}
          </button>
        </div>

        <!-- Search -->
        <div class="relative">
          <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400">
            <Icon name="search" size="sm" />
          </span>
          <input
            v-model="query"
            class="input pl-9"
            :placeholder="t('admin.pricingDisplay.catalog.searchPlaceholder')"
          />
        </div>
      </div>

      <!-- List -->
      <div class="min-h-0 flex-1 overflow-y-auto px-6 py-2">
        <div v-if="loading" class="flex items-center justify-center py-16">
          <Icon name="refresh" size="xl" class="animate-spin text-gray-400" />
        </div>
        <div v-else-if="filteredEntries.length === 0" class="py-16 text-center text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.pricingDisplay.catalog.empty') }}
        </div>
        <ul v-else class="divide-y divide-gray-100 dark:divide-dark-600">
          <li
            v-for="e in filteredEntries"
            :key="e.model"
            @click="toggle(e)"
            :class="[
              e.added ? 'cursor-not-allowed opacity-50' : 'cursor-pointer hover:bg-gray-50 dark:hover:bg-dark-700',
              selected.has(e.model) ? 'bg-primary-50 dark:bg-primary-900/20' : '',
            ]"
            class="flex items-center gap-3 rounded-md px-2 py-2.5 transition-colors"
          >
            <input
              type="checkbox"
              :checked="selected.has(e.model)"
              :disabled="e.added"
              class="rounded"
              @click.stop="toggle(e)"
            />
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span class="truncate font-mono text-sm font-medium text-gray-900 dark:text-white">{{ e.model }}</span>
                <span class="shrink-0 text-[11px] text-gray-400">{{ e.platform }}</span>
                <span :class="typeBadgeClass(e.model_type)" class="shrink-0 rounded px-1.5 py-0.5 text-[10px] font-medium">
                  {{ e.model_type === 'text' ? t('admin.pricingDisplay.typeText') : t('admin.pricingDisplay.typeImage') }}
                </span>
                <span v-if="e.added" class="shrink-0 rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-600 dark:text-gray-400">
                  {{ t('admin.pricingDisplay.catalog.added') }}
                </span>
              </div>
              <div class="mt-0.5 font-mono text-[11px] text-gray-500 dark:text-gray-400">
                <template v-if="e.model_type === 'text'">
                  {{ t('admin.pricingDisplay.catalog.officialInline', { input: fmtTokenPrice(e.input_price), output: fmtTokenPrice(e.output_price) }) }}
                </template>
                <template v-else>
                  {{ t('admin.pricingDisplay.catalog.officialImageInline', { price: fmtImagePrice(e.image_price) }) }}
                </template>
              </div>
            </div>
          </li>
        </ul>
      </div>

      <!-- Footer -->
      <div class="flex items-center justify-between border-t border-gray-200 px-6 py-4 dark:border-dark-600">
        <button type="button" @click="$emit('manual-create')" class="btn btn-secondary">
          {{ t('admin.pricingDisplay.catalog.manualCreate') }}
        </button>
        <button type="button" @click="confirmAdd" :disabled="selected.size === 0 || adding" class="btn btn-primary">
          {{ adding ? t('admin.pricingDisplay.catalog.adding') : t('admin.pricingDisplay.catalog.confirm', { count: selected.size }) }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { pricingApi, type CatalogEntry } from '@/api/admin/pricing'
import { useAppStore } from '@/stores/app'

const emit = defineEmits<{
  close: []
  added: []
  'manual-create': []
}>()

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const syncing = ref(false)
const adding = ref(false)
const entries = ref<CatalogEntry[]>([])
const platform = ref('')
const query = ref('')
const selected = ref<Set<string>>(new Set())

const platforms = computed(() => [
  { value: '', label: t('admin.pricingDisplay.catalog.platformAll') },
  { value: 'anthropic', label: 'anthropic' },
  { value: 'openai', label: 'openai' },
  { value: 'gemini', label: 'gemini' },
])

const filteredEntries = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return entries.value
  return entries.value.filter((e) => e.model.toLowerCase().includes(q))
})

async function loadCatalog() {
  loading.value = true
  try {
    const res = await pricingApi.listCatalog(platform.value || undefined)
    entries.value = res.models ?? []
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.pricingDisplay.catalog.loadFail'))
  } finally {
    loading.value = false
  }
}

watch(platform, loadCatalog)

async function doSync() {
  syncing.value = true
  try {
    const res = await pricingApi.syncCatalog()
    appStore.showSuccess(t('admin.pricingDisplay.catalog.syncSuccess', { total: res.total }))
    await loadCatalog()
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.pricingDisplay.catalog.syncFail'))
  } finally {
    syncing.value = false
  }
}

function toggle(e: CatalogEntry) {
  if (e.added) return
  const next = new Set(selected.value)
  if (next.has(e.model)) next.delete(e.model)
  else next.add(e.model)
  selected.value = next
}

async function confirmAdd() {
  if (selected.value.size === 0) return
  adding.value = true
  try {
    const res = await pricingApi.createFromCatalog(Array.from(selected.value))
    appStore.showSuccess(
      t('admin.pricingDisplay.catalog.addResult', { created: res.created, skipped: res.skipped }),
    )
    selected.value = new Set()
    emit('added')
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.pricingDisplay.catalog.addFail'))
  } finally {
    adding.value = false
  }
}

function typeBadgeClass(type: string) {
  return type === 'text'
    ? 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
    : 'bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-300'
}

// Official prices are USD per token; show them per-MTok for readability.
function fmtTokenPrice(v: number) {
  if (!v || !isFinite(v)) return '—'
  return `$${(v * 1_000_000).toFixed(2)}/MTok`
}

function fmtImagePrice(v: number) {
  if (!v || !isFinite(v)) return '—'
  return `$${v.toFixed(4)}`
}

onMounted(loadCatalog)
</script>
