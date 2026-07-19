<template>
  <div>
    <label v-if="label" class="label">{{ label }}</label>
    <div class="relative">
      <span class="absolute left-3 top-1/2 -translate-y-1/2 text-xs text-gray-400">$</span>
      <input
        :value="displayValue"
        @input="onInput"
        type="number"
        step="0.000000001"
        min="0"
        class="input pl-6 font-mono text-sm"
        :placeholder="placeholder"
      />
    </div>
    <p v-if="modelValue != null" class="mt-0.5 text-xs text-gray-400 dark:text-gray-500">
      = ${{ perMTok }}/MTok
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{
  modelValue: number | null | undefined
  label?: string
  placeholder?: string
}>()

const emit = defineEmits<{
  'update:modelValue': [value: number | null]
}>()

const displayValue = computed(() => {
  if (props.modelValue == null) return ''
  return props.modelValue
})

const perMTok = computed(() => {
  if (props.modelValue == null || !isFinite(props.modelValue)) return '—'
  return props.modelValue.toFixed(4)
})

function onInput(e: Event) {
  const raw = (e.target as HTMLInputElement).value
  if (raw === '' || raw == null) {
    emit('update:modelValue', null)
  } else {
    emit('update:modelValue', parseFloat(raw))
  }
}
</script>
