<template>
  <BaseDialog
    :show="show"
    :title="t('admin.users.editUser')"
    width="normal"
    @close="$emit('close')"
  >
    <form v-if="user" id="edit-user-form" @submit.prevent="handleUpdateUser" class="space-y-5">
      <div>
        <label class="input-label">{{ t('admin.users.email') }}</label>
        <input v-model="form.email" type="email" class="input" />
      </div>
      <div>
        <label class="input-label">{{ t('admin.users.password') }}</label>
        <div class="flex gap-2">
          <div class="relative flex-1">
            <input v-model="form.password" type="text" class="input pr-10" :placeholder="t('admin.users.enterNewPassword')" />
            <button v-if="form.password" type="button" @click="copyPassword" class="absolute right-2 top-1/2 -translate-y-1/2 rounded-lg p-1 transition-colors hover:bg-gray-100 dark:hover:bg-dark-700" :class="passwordCopied ? 'text-green-500' : 'text-gray-400'">
              <svg v-if="passwordCopied" class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24" stroke-width="2"><path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7" /></svg>
              <svg v-else class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24" stroke-width="1.5"><path stroke-linecap="round" stroke-linejoin="round" d="M15.666 3.888A2.25 2.25 0 0013.5 2.25h-3c-1.03 0-1.9.693-2.166 1.638m7.332 0c.055.194.084.4.084.612v0a.75.75 0 01-.75.75H9a.75.75 0 01-.75-.75v0c0-.212.03-.418.084-.612m7.332 0c.646.049 1.288.11 1.927.184 1.1.128 1.907 1.077 1.907 2.185V19.5a2.25 2.25 0 01-2.25 2.25H6.75A2.25 2.25 0 014.5 19.5V6.257c0-1.108.806-2.057 1.907-2.185a48.208 48.208 0 011.927-.184" /></svg>
            </button>
          </div>
          <button type="button" @click="generatePassword" class="btn btn-secondary px-3">
            <Icon name="refresh" size="md" />
          </button>
        </div>
      </div>
      <div>
        <label class="input-label">{{ t('admin.users.username') }}</label>
        <input v-model="form.username" type="text" class="input" />
      </div>
      <div>
        <label class="input-label">{{ t('admin.users.notes') }}</label>
        <textarea v-model="form.notes" rows="3" class="input"></textarea>
      </div>
      <!-- 并发 / RPM 按平台（Claude / GPT）分别设置；格子里就是实际生效的数，保存后各平台独立计数 -->
      <div>
        <label class="input-label">{{ t('admin.users.form.platformLimits.title') }}</label>
        <div class="grid grid-cols-[6rem_1fr_1fr] items-center gap-x-3 gap-y-2">
          <span></span>
          <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.users.form.platformLimits.concurrency') }}</span>
          <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.users.form.platformLimits.rpm') }}</span>
          <template v-for="p in LIMIT_PLATFORMS" :key="p">
            <span class="text-sm text-gray-700 dark:text-gray-300">{{ t(`admin.users.form.platformLimits.${p}`) }}</span>
            <input v-model.number="platformLimits[p].concurrency" type="number" min="0" step="1" class="input" :disabled="!platformLimitsLoaded" />
            <input v-model.number="platformLimits[p].rpm_limit" type="number" min="0" step="1" class="input" :disabled="!platformLimitsLoaded" />
          </template>
        </div>
        <p class="input-hint">{{ t('admin.users.form.platformLimits.hint') }}</p>
        <p v-if="platformLimitsLoadFailed" class="input-hint text-amber-600 dark:text-amber-400">{{ t('admin.users.form.platformLimits.loadFailed') }}</p>
      </div>
      <UserAttributeForm v-model="form.customAttributes" :user-id="user?.id" />
    </form>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button @click="$emit('close')" type="button" class="btn btn-secondary">{{ t('common.cancel') }}</button>
        <button type="submit" form="edit-user-form" :disabled="submitting" class="btn btn-primary">
          {{ submitting ? t('admin.users.updating') : t('common.update') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { useClipboard } from '@/composables/useClipboard'
import { adminAPI } from '@/api/admin'
import type { AdminUser, UserAttributeValuesMap, PlatformQuotaItem, PlatformQuotaPlatform } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import UserAttributeForm from '@/components/user/UserAttributeForm.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ show: boolean, user: AdminUser | null }>()
const emit = defineEmits(['close', 'success'])
const { t } = useI18n(); const appStore = useAppStore(); const { copyToClipboard } = useClipboard()

const submitting = ref(false); const passwordCopied = ref(false)
const form = reactive({ email: '', password: '', username: '', notes: '', customAttributes: {} as UserAttributeValuesMap })

// 并发 / RPM 按平台设置（数据在 user_platform_quotas，与「平台限额」同一张表）。
// 只暴露 Claude(anthropic) / GPT(openai)；格子里显示实际生效值：该平台设过就是它，
// 没设过就是用户底层的全局值。保存后两个平台都写成明确的专属值，各自独立计数。
const LIMIT_PLATFORMS = ['anthropic', 'openai'] as const
type LimitPlatform = (typeof LIMIT_PLATFORMS)[number]
type PlatformLimitForm = { concurrency: number | null; rpm_limit: number | null }
const platformLimits = reactive<Record<LimitPlatform, PlatformLimitForm>>({
  anthropic: { concurrency: null, rpm_limit: null },
  openai: { concurrency: null, rpm_limit: null },
})
// 加载到的全部平台行（含 USD 限额）：保存时原样带回——PUT 是全量替换，不带会把 USD 限额清掉。
let platformQuotaRows: PlatformQuotaItem[] = []
const platformLimitsLoaded = ref(false)
const platformLimitsLoadFailed = ref(false)

function isLimitPlatform(p: PlatformQuotaPlatform): p is LimitPlatform {
  return (LIMIT_PLATFORMS as readonly string[]).includes(p)
}

async function loadPlatformLimits(u: AdminUser) {
  platformLimitsLoaded.value = false
  platformLimitsLoadFailed.value = false
  for (const p of LIMIT_PLATFORMS) {
    platformLimits[p].concurrency = u.concurrency
    platformLimits[p].rpm_limit = u.rpm_limit ?? 0
  }
  try {
    const data = await adminAPI.users.getPlatformQuotas(u.id)
    platformQuotaRows = data.platform_quotas || []
    for (const row of platformQuotaRows) {
      if (!isLimitPlatform(row.platform)) continue
      if (typeof row.concurrency === 'number') platformLimits[row.platform].concurrency = row.concurrency
      if (typeof row.rpm_limit === 'number') platformLimits[row.platform].rpm_limit = row.rpm_limit
    }
    platformLimitsLoaded.value = true
  } catch {
    // 拿不到既有平台行就不允许改这一块（否则全量替换会把 USD 限额清掉）
    platformQuotaRows = []
    platformLimitsLoadFailed.value = true
  }
}

function toIntLimit(v: number | null): number | null {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0 ? Math.floor(v) : null
}

async function savePlatformLimits(userId: number) {
  if (!platformLimitsLoaded.value) return
  const existing = new Map(platformQuotaRows.map((r) => [r.platform, r] as const))
  const payload = platformQuotaRows.map((row) => ({
    platform: row.platform,
    daily_limit_usd: row.daily_limit_usd ?? null,
    weekly_limit_usd: row.weekly_limit_usd ?? null,
    monthly_limit_usd: row.monthly_limit_usd ?? null,
    concurrency: isLimitPlatform(row.platform) ? toIntLimit(platformLimits[row.platform].concurrency) : (row.concurrency ?? null),
    rpm_limit: isLimitPlatform(row.platform) ? toIntLimit(platformLimits[row.platform].rpm_limit) : (row.rpm_limit ?? null),
  }))
  for (const p of LIMIT_PLATFORMS) {
    if (existing.has(p)) continue
    payload.push({
      platform: p,
      daily_limit_usd: null,
      weekly_limit_usd: null,
      monthly_limit_usd: null,
      concurrency: toIntLimit(platformLimits[p].concurrency),
      rpm_limit: toIntLimit(platformLimits[p].rpm_limit),
    })
  }
  await adminAPI.users.updatePlatformQuotas(userId, payload)
}

watch(() => props.user, (u) => {
  if (u) {
    Object.assign(form, { email: u.email, password: '', username: u.username || '', notes: u.notes || '', customAttributes: {} })
    passwordCopied.value = false
    void loadPlatformLimits(u)
  }
}, { immediate: true })

const generatePassword = () => {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789!@#$%^&*'
  let p = ''; for (let i = 0; i < 16; i++) p += chars.charAt(Math.floor(Math.random() * chars.length))
  form.password = p
}
const copyPassword = async () => {
  if (form.password && await copyToClipboard(form.password, t('admin.users.passwordCopied'))) {
    passwordCopied.value = true; setTimeout(() => passwordCopied.value = false, 2000)
  }
}
const handleUpdateUser = async () => {
  if (!props.user) return
  if (!form.email.trim()) {
    appStore.showError(t('admin.users.emailRequired'))
    return
  }
  if (platformLimitsLoaded.value) {
    for (const p of LIMIT_PLATFORMS) {
      if (toIntLimit(platformLimits[p].concurrency) === null || toIntLimit(platformLimits[p].rpm_limit) === null) {
        appStore.showError(t('admin.users.form.platformLimits.invalid'))
        return
      }
    }
  }
  submitting.value = true
  try {
    const data: any = { email: form.email, username: form.username, notes: form.notes }
    if (form.password.trim()) data.password = form.password.trim()
    await adminAPI.users.update(props.user.id, data)
    await savePlatformLimits(props.user.id)
    if (Object.keys(form.customAttributes).length > 0) await adminAPI.userAttributes.updateUserAttributeValues(props.user.id, form.customAttributes)
    appStore.showSuccess(t('admin.users.userUpdated'))
    emit('success'); emit('close')
  } catch (e: any) {
    appStore.showError(e.response?.data?.detail || t('admin.users.failedToUpdate'))
  } finally { submitting.value = false }
}
</script>
