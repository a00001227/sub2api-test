<template>
  <BaseDialog
    :show="show"
    :title="t('admin.users.createUser')"
    width="normal"
    @close="$emit('close')"
  >
    <form id="create-user-form" @submit.prevent="submit" class="space-y-5">
      <div>
        <label class="input-label">{{ t('admin.users.email') }}</label>
        <input v-model="form.email" type="email" required class="input" :placeholder="t('admin.users.enterEmail')" />
      </div>
      <div>
        <label class="input-label">{{ t('admin.users.password') }}</label>
        <div class="flex gap-2">
          <div class="relative flex-1">
            <input v-model="form.password" type="text" required class="input pr-10" :placeholder="t('admin.users.enterPassword')" />
          </div>
          <button type="button" @click="generateRandomPassword" class="btn btn-secondary px-3">
            <Icon name="refresh" size="md" />
          </button>
        </div>
      </div>
      <div>
        <label class="input-label">{{ t('admin.users.username') }}</label>
        <input v-model="form.username" type="text" class="input" :placeholder="t('admin.users.enterUsername')" />
      </div>
      <div>
        <label class="input-label">{{ t('admin.users.columns.balance') }}</label>
        <input v-model="form.balance" type="number" step="any" class="input" />
      </div>
      <!-- 并发 / RPM 按平台（Claude / GPT）设置，预填系统「用户默认值」 -->
      <div>
        <label class="input-label">{{ t('admin.users.form.platformLimits.title') }}</label>
        <div class="grid grid-cols-[6rem_1fr_1fr] items-center gap-x-3 gap-y-2">
          <span></span>
          <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.users.form.platformLimits.concurrency') }}</span>
          <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.users.form.platformLimits.rpm') }}</span>
          <template v-for="p in LIMIT_PLATFORMS" :key="p">
            <span class="text-sm text-gray-700 dark:text-gray-300">{{ t(`admin.users.form.platformLimits.${p}`) }}</span>
            <input v-model.number="platformLimits[p].concurrency" type="number" min="0" step="1" class="input" />
            <input v-model.number="platformLimits[p].rpm_limit" type="number" min="0" step="1" class="input" />
          </template>
        </div>
        <p class="input-hint">{{ t('admin.users.form.platformLimits.hint') }}</p>
      </div>
    </form>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button @click="$emit('close')" type="button" class="btn btn-secondary">{{ t('common.cancel') }}</button>
        <button type="submit" form="create-user-form" :disabled="loading" class="btn btn-primary">
          {{ loading ? t('admin.users.creating') : t('common.create') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'; import { adminAPI } from '@/api/admin'
import { useForm } from '@/composables/useForm'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits(['close', 'success']); const { t } = useI18n()

const form = reactive({ email: '', password: '', username: '', notes: '', balance: '' })

// 并发 / RPM 按平台（Claude / GPT）设置：打开时预填系统「用户默认值」，创建后写入该用户的平台记录。
// 底层的全局 concurrency / rpm_limit 用系统默认值（只给未暴露的平台兜底）。
const LIMIT_PLATFORMS = ['anthropic', 'openai'] as const
type LimitPlatform = (typeof LIMIT_PLATFORMS)[number]
const platformLimits = reactive<Record<LimitPlatform, { concurrency: number | null; rpm_limit: number | null }>>({
  anthropic: { concurrency: 1, rpm_limit: 0 },
  openai: { concurrency: 1, rpm_limit: 0 },
})
const globalDefaults = reactive({ concurrency: 1, rpm_limit: 0 })

async function loadDefaults() {
  try {
    const s = await adminAPI.settings.getSettings()
    globalDefaults.concurrency = s.default_concurrency || 1
    globalDefaults.rpm_limit = s.default_user_rpm_limit || 0
    for (const p of LIMIT_PLATFORMS) {
      const q = s.default_platform_quotas?.[p]
      platformLimits[p].concurrency = typeof q?.concurrency === 'number' ? q.concurrency : globalDefaults.concurrency
      platformLimits[p].rpm_limit = typeof q?.rpm === 'number' ? q.rpm : globalDefaults.rpm_limit
    }
  } catch {
    // 拿不到系统默认值就保留表单当前值（1 / 0），管理员可手填
  }
}

function toIntLimit(v: number | null): number | null {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0 ? Math.floor(v) : null
}

const { loading, submit } = useForm({
  form,
  submitFn: async (data) => {
    for (const p of LIMIT_PLATFORMS) {
      if (toIntLimit(platformLimits[p].concurrency) === null || toIntLimit(platformLimits[p].rpm_limit) === null) {
        throw new Error(t('admin.users.form.platformLimits.invalid'))
      }
    }
    const { balance: rawBalance, ...rest } = data
    const balance = String(rawBalance).trim()
    const payload: typeof rest & { balance?: number; concurrency: number; rpm_limit: number } = {
      ...rest,
      concurrency: globalDefaults.concurrency,
      rpm_limit: globalDefaults.rpm_limit,
    }
    if (balance !== '') {
      payload.balance = Number(balance)
    }
    const created = await adminAPI.users.create(payload)
    await adminAPI.users.updatePlatformQuotas(
      created.id,
      LIMIT_PLATFORMS.map((p) => ({
        platform: p,
        daily_limit_usd: null,
        weekly_limit_usd: null,
        monthly_limit_usd: null,
        concurrency: toIntLimit(platformLimits[p].concurrency),
        rpm_limit: toIntLimit(platformLimits[p].rpm_limit),
      })),
    )
    emit('success'); emit('close')
  },
  successMsg: t('admin.users.userCreated')
})

watch(() => props.show, (v) => {
  if (v) {
    Object.assign(form, { email: '', password: '', username: '', notes: '', balance: '' })
    void loadDefaults()
  }
})

const generateRandomPassword = () => {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789!@#$%^&*'
  let p = ''; for (let i = 0; i < 16; i++) p += chars.charAt(Math.floor(Math.random() * chars.length))
  form.password = p
}
</script>
