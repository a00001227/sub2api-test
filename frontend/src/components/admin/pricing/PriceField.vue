<template>
  <div>
    <label v-if="label" class="label">{{ label }}</label>
    <div class="relative">
      <span class="absolute left-3 top-1/2 -translate-y-1/2 text-xs text-gray-400">$</span>
      <input
        :value="displayValue"
        @input="onInput"
        type="number"
        step="0.0001"
        min="0"
        class="input pl-6 pr-14 font-mono text-sm"
        :placeholder="placeholder"
      />
      <span class="absolute right-3 top-1/2 -translate-y-1/2 text-xs text-gray-400">/MTok</span>
    </div>
    <p v-if="modelValue != null" class="mt-0.5 text-xs text-gray-400 dark:text-gray-500">
      = ${{ perToken }}/token
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'

// Backend stores USD per token; this field edits in USD per 1M tokens (/MTok),
// which is how prices are quoted officially. We convert on the boundary.
const props = defineProps<{
  modelValue: number | null | undefined
  label?: string
  placeholder?: string
}>()

const emit = defineEmits<{
  'update:modelValue': [value: number | null]
}>()

// per-token (stored) → per-1M-tokens (shown/edited)
const displayValue = computed(() => {
  if (props.modelValue == null) return ''
  return Number((props.modelValue * 1_000_000).toFixed(6))
})

// small reference line: the raw per-token value actually stored
const perToken = computed(() => {
  if (props.modelValue == null || !isFinite(props.modelValue)) return '—'
  return String(Number(props.modelValue.toPrecision(6)))
})

function onInput(e: Event) {
  const raw = (e.target as HTMLInputElement).value
  if (raw === '' || raw == null) {
    emit('update:modelValue', null)
    return
  }
  const perMTok = parseFloat(raw)
  if (!isFinite(perMTok)) {
    emit('update:modelValue', null)
    return
  }
  // per-1M-tokens (entered) → per-token (stored)
  emit('update:modelValue', perMTok / 1_000_000)
}
</script>
