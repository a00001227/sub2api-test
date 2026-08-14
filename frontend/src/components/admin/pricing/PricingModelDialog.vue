<template>
  <div class="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/50 py-8" @click.self="$emit('close')">
    <div class="w-full max-w-2xl rounded-lg bg-white shadow-xl dark:bg-dark-800">
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-gray-200 px-6 py-4 dark:border-dark-600">
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ isEdit ? t('admin.pricingDisplay.dialogEditTitle') : t('admin.pricingDisplay.dialogCreateTitle') }}
        </h2>
        <button @click="$emit('close')" class="text-gray-400 hover:text-gray-600 dark:hover:text-gray-200">
          <Icon name="x" size="md" />
        </button>
      </div>

      <form @submit.prevent="save" class="divide-y divide-gray-100 dark:divide-dark-600">
        <!-- Basic info -->
        <div class="px-6 py-4 space-y-4">
          <h3 class="text-sm font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.sectionModelInfo') }}</h3>

          <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <div class="sm:col-span-3">
              <label class="label">{{ t('admin.pricingDisplay.labelModelName') }}</label>
              <input v-model="form.model" class="input" placeholder="e.g. claude-sonnet-4-6" required />
            </div>

            <div>
              <label class="label">{{ t('admin.pricingDisplay.labelType') }}</label>
              <select v-model="form.model_type" class="input" required>
                <option value="text">{{ t('admin.pricingDisplay.typeText') }}</option>
                <option value="image">{{ t('admin.pricingDisplay.typeImage') }}</option>
              </select>
            </div>

            <div>
              <label class="label">{{ t('admin.pricingDisplay.labelUserType') }}</label>
              <select v-model="form.user_type" class="input" required>
                <option value="end_user">{{ t('admin.pricingDisplay.userTypeEndUser') }}</option>
                <option value="channel_user">{{ t('admin.pricingDisplay.userTypeChannelUser') }}</option>
              </select>
            </div>

            <div class="flex items-end">
              <label class="flex items-center gap-2 cursor-pointer">
                <input type="checkbox" v-model="form.enabled" class="rounded" />
                <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.pricingDisplay.labelEnabled') }}</span>
              </label>
            </div>
          </div>
        </div>

        <!-- Text pricing: 4 rows × (官方价 | 你的价 | 折扣), 你的价 ⇄ 折扣 two-way -->
        <div v-if="form.model_type === 'text'" class="px-6 py-4 space-y-3">
          <h3 class="text-sm font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.sectionTextPricing') }}</h3>
          <p class="text-xs text-gray-400 dark:text-gray-500">{{ t('admin.pricingDisplay.textPricingHint') }}</p>

          <div class="grid grid-cols-[64px_1fr_1fr_86px] items-center gap-2 px-1 text-xs text-gray-400 dark:text-gray-500">
            <span></span>
            <span>{{ t('admin.pricingDisplay.colOfficial') }}</span>
            <span>{{ t('admin.pricingDisplay.colYourPrice') }}</span>
            <span>{{ t('admin.pricingDisplay.colDiscount') }}</span>
          </div>

          <div
            v-for="row in priceRows"
            :key="row.label"
            class="grid grid-cols-[64px_1fr_1fr_86px] items-center gap-2"
          >
            <span class="text-sm text-gray-600 dark:text-gray-300">{{ row.label }}</span>
            <!-- 官方价 /MTok -->
            <div class="relative">
              <span class="absolute left-2.5 top-1/2 -translate-y-1/2 text-xs text-gray-400">$</span>
              <input :value="row.official.get()" @input="row.official.set(($event.target as HTMLInputElement).value)"
                type="number" step="0.0001" min="0" class="input pl-5 font-mono text-sm" placeholder="—" />
            </div>
            <!-- 你的价 /MTok -->
            <div class="relative">
              <span class="absolute left-2.5 top-1/2 -translate-y-1/2 text-xs text-gray-400">$</span>
              <input :value="row.price.get()" @input="row.price.set(($event.target as HTMLInputElement).value)"
                type="number" step="0.0001" min="0" class="input pl-5 font-mono text-sm" placeholder="—" />
            </div>
            <!-- 折扣 % -->
            <div class="relative">
              <input :value="row.discount.get()" @input="row.discount.set(($event.target as HTMLInputElement).value)"
                type="number" step="0.1" class="input pr-6 font-mono text-sm text-right"
                :disabled="!(row.officialRaw()! > 0)" placeholder="—" />
              <span class="absolute right-2.5 top-1/2 -translate-y-1/2 text-xs text-gray-400">%</span>
            </div>
          </div>
          <p class="px-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.pricingDisplay.unitMtok') }}</p>
        </div>

        <!-- Image pricing -->
        <div v-else-if="form.model_type === 'image'" class="px-6 py-4 space-y-4">
          <h3 class="text-sm font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.sectionImagePricing') }}</h3>
          <p class="text-xs text-gray-400 dark:text-gray-500">{{ t('admin.pricingDisplay.imagePricingHint') }}</p>

          <div class="space-y-2">
            <div
              v-for="(entry, idx) in imageResolutions"
              :key="idx"
              class="flex items-center gap-2"
            >
              <input
                v-model="entry.key"
                class="input w-28 font-mono"
                placeholder="e.g. 1k"
              />
              <span class="text-gray-400">→</span>
              <div class="relative flex-1">
                <span class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 text-sm">$</span>
                <input
                  v-model.number="entry.price"
                  type="number"
                  step="0.0001"
                  min="0"
                  class="input pl-6"
                  placeholder="0.0000"
                />
              </div>
              <button type="button" @click="removeResolution(idx)" class="text-red-400 hover:text-red-600">
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </div>

          <button type="button" @click="addResolution" class="btn btn-secondary btn-sm">
            <Icon name="plus" size="sm" class="mr-1" />
            {{ t('admin.pricingDisplay.addResolution') }}
          </button>

          <!-- Image saving override -->
          <div class="mt-4 border-t border-gray-100 pt-4 dark:border-dark-600">
            <label class="label">{{ t('admin.pricingDisplay.labelSavingOverride') }}</label>
            <p class="mb-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.pricingDisplay.savingOverrideHint') }}</p>
            <input
              v-model.number="form.saving_percent"
              type="number"
              step="0.01"
              min="0"
              max="1"
              class="input w-40"
              placeholder="0.73"
            />
          </div>
        </div>

        <!-- Footer -->
        <div class="flex items-center justify-end gap-3 px-6 py-4">
          <button type="button" @click="$emit('close')" class="btn btn-secondary">{{ t('admin.pricingDisplay.cancel') }}</button>
          <button type="submit" :disabled="saving" class="btn btn-primary">
            {{ saving ? t('admin.pricingDisplay.btnSaving') : (isEdit ? t('admin.pricingDisplay.btnSaveChanges') : t('admin.pricingDisplay.btnCreate')) }}
          </button>
        </div>
      </form>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { pricingApi, type PricingModelRecord, type CreatePricingModelPayload } from '@/api/admin/pricing'
import { useAppStore } from '@/stores/app'

const props = defineProps<{
  record: PricingModelRecord | null
}>()

const emit = defineEmits<{
  close: []
  saved: []
}>()

const { t } = useI18n()
const appStore = useAppStore()
const saving = ref(false)
const isEdit = computed(() => !!props.record)

interface ResolutionEntry {
  key: string
  price: number
}

const imageResolutions = ref<ResolutionEntry[]>([])

type PriceKey =
  | 'input_price' | 'output_price' | 'cache_read_price' | 'cache_write_price'
type OfficialKey =
  | 'official_input_price' | 'official_output_price'
  | 'official_cache_read_price' | 'official_cache_write_price'

const form = ref<CreatePricingModelPayload>({
  model: '',
  model_type: 'text',
  user_type: 'end_user',
  enabled: true,
  input_price: null,
  output_price: null,
  cache_read_price: null,
  cache_write_price: null,
  official_input_price: null,
  official_output_price: null,
  official_cache_read_price: null,
  official_cache_write_price: null,
  saving_percent: undefined,
})

onMounted(() => {
  if (props.record) {
    const r = props.record
    form.value = {
      model: r.model,
      model_type: r.model_type,
      user_type: r.user_type,
      enabled: r.enabled,
      input_price: r.input_price,
      output_price: r.output_price,
      cache_read_price: r.cache_read_price,
      cache_write_price: r.cache_write_price,
      official_input_price: r.official_input_price,
      official_output_price: r.official_output_price,
      official_cache_read_price: r.official_cache_read_price,
      official_cache_write_price: r.official_cache_write_price,
      saving_percent: r.saving_percent ?? undefined,
    }
    if (r.model_type === 'image') {
      imageResolutions.value = r.image_resolutions
        ? Object.entries(r.image_resolutions).map(([key, price]) => ({ key, price }))
        : []
    }
  }
})

// --- $/MTok <-> per-token helpers -------------------------------------------
// Backend stores USD per token; the UI edits in USD per 1M tokens.
function toMTok(perToken: number | null | undefined): number | null {
  if (perToken == null || !isFinite(perToken)) return null
  return Number((perToken * 1_000_000).toFixed(6))
}
function setPerToken(key: PriceKey | OfficialKey, mtokRaw: string) {
  if (mtokRaw === '') { form.value[key] = null; return }
  const mtok = parseFloat(mtokRaw)
  form.value[key] = isFinite(mtok) ? mtok / 1_000_000 : null
}

// A two-way $/MTok cell bound to a per-token form field (get()/set for template).
function mtokCell(key: PriceKey | OfficialKey) {
  return {
    get: () => toMTok(form.value[key] as number | null),
    set: (raw: string) => setPerToken(key, raw),
  }
}

// A two-way discount(%) cell: derived from your-price vs official (both per-token).
// get: (official - price) / official * 100 ; set: price = official * (1 - d/100).
function discountCell(priceKey: PriceKey, officialKey: OfficialKey) {
  return {
    get: (): number | null => {
      const off = form.value[officialKey] as number | null
      const p = form.value[priceKey] as number | null
      if (off == null || !(off > 0) || p == null) return null
      return Number((((off - p) / off) * 100).toFixed(2))
    },
    set: (raw: string) => {
      const off = form.value[officialKey] as number | null
      if (off == null || !(off > 0)) return
      if (raw === '') return
      const d = parseFloat(raw)
      if (!isFinite(d)) return
      form.value[priceKey] = Math.max(0, off * (1 - d / 100))
    },
  }
}

const priceRows = computed(() => [
  {
    label: t('admin.pricingDisplay.rowInput'),
    official: mtokCell('official_input_price'),
    officialRaw: () => form.value.official_input_price,
    price: mtokCell('input_price'),
    discount: discountCell('input_price', 'official_input_price'),
  },
  {
    label: t('admin.pricingDisplay.rowOutput'),
    official: mtokCell('official_output_price'),
    officialRaw: () => form.value.official_output_price,
    price: mtokCell('output_price'),
    discount: discountCell('output_price', 'official_output_price'),
  },
  {
    label: t('admin.pricingDisplay.rowCacheRead'),
    official: mtokCell('official_cache_read_price'),
    officialRaw: () => form.value.official_cache_read_price,
    price: mtokCell('cache_read_price'),
    discount: discountCell('cache_read_price', 'official_cache_read_price'),
  },
  {
    label: t('admin.pricingDisplay.rowCacheWrite'),
    official: mtokCell('official_cache_write_price'),
    officialRaw: () => form.value.official_cache_write_price,
    price: mtokCell('cache_write_price'),
    discount: discountCell('cache_write_price', 'official_cache_write_price'),
  },
])

function addResolution() {
  imageResolutions.value.push({ key: '', price: 0 })
}

function removeResolution(idx: number) {
  imageResolutions.value.splice(idx, 1)
}

async function save() {
  saving.value = true
  try {
    const payload: CreatePricingModelPayload = { ...form.value }

    if (form.value.model_type === 'image') {
      const resMap: Record<string, number> = {}
      for (const entry of imageResolutions.value) {
        const key = entry.key.trim()
        const price = entry.price
        if (key && isFinite(price)) {
          resMap[key] = price
        }
      }
      payload.image_resolutions = resMap
      payload.input_price = null
      payload.output_price = null
      payload.cache_read_price = null
      payload.cache_write_price = null
      payload.official_input_price = null
      payload.official_output_price = null
      payload.official_cache_read_price = null
      payload.official_cache_write_price = null
      if (payload.saving_percent != null && !isFinite(payload.saving_percent)) {
        payload.saving_percent = undefined
      }
    } else {
      payload.image_resolutions = undefined
      payload.saving_percent = undefined
    }

    if (isEdit.value && props.record) {
      await pricingApi.updateModel(props.record.id, payload)
      appStore.showSuccess(t('admin.pricingDisplay.saveSuccess'))
    } else {
      await pricingApi.createModel(payload)
      appStore.showSuccess(t('admin.pricingDisplay.createSuccess'))
    }
    emit('saved')
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.pricingDisplay.saveFail'))
  } finally {
    saving.value = false
  }
}
</script>
