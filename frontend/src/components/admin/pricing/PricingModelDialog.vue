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

        <!-- Text pricing -->
        <div v-if="form.model_type === 'text'" class="px-6 py-4 space-y-4">
          <h3 class="text-sm font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">{{ t('admin.pricingDisplay.sectionTextPricing') }}</h3>
          <p class="text-xs text-gray-400 dark:text-gray-500">{{ t('admin.pricingDisplay.textPricingHint') }}</p>

          <div class="grid grid-cols-2 gap-4 sm:grid-cols-2">
            <PriceField v-model="form.input_price" :label="t('admin.pricingDisplay.labelInputPrice')" />
            <PriceField v-model="form.output_price" :label="t('admin.pricingDisplay.labelOutputPrice')" />
            <PriceField v-model="form.cache_read_price" :label="t('admin.pricingDisplay.labelCacheReadPrice')" />
            <PriceField v-model="form.cache_write_price" :label="t('admin.pricingDisplay.labelCacheWritePrice')" />
          </div>

          <div class="mt-4">
            <h4 class="mb-3 text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.pricingDisplay.officialPricesTitle') }}</h4>
            <div class="grid grid-cols-2 gap-4">
              <PriceField v-model="form.official_input_price" :label="t('admin.pricingDisplay.labelOfficialInput')" />
              <PriceField v-model="form.official_output_price" :label="t('admin.pricingDisplay.labelOfficialOutput')" />
            </div>
          </div>

          <!-- Real-time preview -->
          <div v-if="textPreview" class="mt-4 rounded-lg border border-green-200 bg-green-50 p-4 dark:border-green-800 dark:bg-green-900/20">
            <h4 class="mb-2 text-sm font-semibold text-green-700 dark:text-green-300">{{ t('admin.pricingDisplay.savingsPreview') }}</h4>
            <div class="grid grid-cols-2 gap-2 text-xs font-mono">
              <div class="text-gray-600 dark:text-gray-400">{{ t('admin.pricingDisplay.previewRealCost') }}</div>
              <div class="text-right font-semibold text-gray-900 dark:text-white">{{ textPreview.realCost }}</div>
              <div class="text-gray-600 dark:text-gray-400">{{ t('admin.pricingDisplay.previewOfficialCost') }}</div>
              <div class="text-right text-gray-700 dark:text-gray-300">{{ textPreview.officialCost }}</div>
              <div class="text-gray-600 dark:text-gray-400">{{ t('admin.pricingDisplay.previewSaving') }}</div>
              <div class="text-right font-bold" :class="textPreview.savingPct >= 0 ? 'text-green-600 dark:text-green-400' : 'text-red-600'">
                {{ textPreview.savingStr }}
              </div>
            </div>
          </div>
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
import PriceField from './PriceField.vue'
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
      saving_percent: r.saving_percent ?? undefined,
    }
    // Always load image_resolutions for image models, even if empty
    if (r.model_type === 'image') {
      imageResolutions.value = r.image_resolutions
        ? Object.entries(r.image_resolutions).map(([key, price]) => ({ key, price }))
        : []
    }
  }
})

const textPreview = computed(() => {
  if (form.value.model_type !== 'text') return null
  const input = form.value.input_price ?? 0
  const output = form.value.output_price ?? 0
  const offInput = form.value.official_input_price ?? 0
  const offOutput = form.value.official_output_price ?? 0

  const realCost = input + output
  const officialCost = offInput + offOutput
  const savingPct = officialCost > 0 ? (officialCost - realCost) / officialCost : 0

  return {
    realCost: fmtTokenPrice(realCost),
    officialCost: fmtTokenPrice(officialCost),
    savingPct,
    savingStr: officialCost > 0 ? `${(savingPct * 100).toFixed(1)}%` : '—',
  }
})

function fmtTokenPrice(v: number) {
  if (!v) return '$0/MTok'
  return `$${v.toFixed(4)}/MTok`
}

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
      // Sanitize saving_percent: treat NaN/Infinity as absent
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
