<template>
  <div class="rounded-lg border border-gray-200 dark:border-dark-500">
    <button
      class="flex w-full items-center justify-between px-4 py-3 text-left"
      :aria-expanded="open"
      @click="open = !open"
    >
      <span class="flex items-center gap-2">
        <span class="text-sm font-semibold text-gray-800 dark:text-gray-200">{{ t('admin.riskAllowlist.title') }}</span>
        <span class="rounded-full bg-emerald-100 px-2 py-0.5 text-[11px] font-medium text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300">
          {{ t('admin.riskAllowlist.summary', { users: users.length, groups: groupIds.length }) }}
        </span>
      </span>
      <span class="text-xs text-primary-600 dark:text-primary-400">{{ open ? t('admin.riskAllowlist.collapse') : t('admin.riskAllowlist.expand') }}</span>
    </button>

    <div v-if="open" class="space-y-4 border-t border-gray-100 px-4 py-3 dark:border-dark-600">
      <p class="text-[11px] text-gray-500 dark:text-gray-400">{{ t('admin.riskAllowlist.hint') }}</p>
      <p class="text-[11px] text-amber-600 dark:text-amber-400">{{ t('admin.riskAllowlist.purgeHint') }}</p>

      <div v-if="loadError" class="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ loadError }}</div>

      <!-- 用户白名单 -->
      <div>
        <div class="mb-2 flex items-center justify-between gap-2">
          <label class="input-label mb-0">{{ t('admin.riskControl.allowedUsers') }}</label>
          <span class="inline-flex rounded-md bg-gray-100 px-2 py-1 text-xs text-gray-500 dark:bg-dark-700 dark:text-gray-300">
            {{ t('admin.riskControl.allowedUserCount', { count: users.length }) }}
          </span>
        </div>
        <div ref="userSearchRef" class="relative">
          <input
            v-model="userKeyword"
            type="text"
            class="input"
            :placeholder="t('admin.riskControl.allowedUsersPlaceholder')"
            @input="debounceUserSearch"
            @focus="showUserDropdown = true"
            @keydown.enter.prevent="addUserByKeyword"
          />
          <div
            v-if="showUserDropdown && userResults.length > 0"
            class="absolute z-50 mt-1 max-h-60 w-full overflow-auto rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-600 dark:bg-dark-800"
          >
            <button
              v-for="u in userResults"
              :key="u.id"
              type="button"
              class="flex w-full items-center justify-between px-4 py-2 text-left text-sm hover:bg-gray-100 dark:hover:bg-dark-700"
              @click="addUser(u)"
            >
              <span>{{ u.email }}</span>
              <span class="ml-2 text-xs text-gray-400">#{{ u.id }}</span>
            </button>
          </div>
        </div>
        <div v-if="users.length > 0" class="mt-2 flex flex-wrap gap-2">
          <span
            v-for="u in users"
            :key="u.id"
            class="inline-flex items-center gap-1 rounded-md bg-emerald-50 px-2 py-1 text-xs text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300"
          >
            <span class="font-mono">#{{ u.id }}</span>
            <span>{{ u.email || t('admin.riskControl.allowedUserUnknownEmail') }}</span>
            <button type="button" class="ml-1 text-emerald-500 hover:text-emerald-700 dark:hover:text-emerald-200" :aria-label="t('common.delete')" @click="removeUser(u.id)">✕</button>
          </span>
        </div>
      </div>

      <!-- 分组白名单 -->
      <div>
        <div class="mb-2 flex items-center justify-between gap-2">
          <label class="input-label mb-0">{{ t('admin.riskControl.allowedGroups') }}</label>
          <span class="inline-flex rounded-md bg-gray-100 px-2 py-1 text-xs text-gray-500 dark:bg-dark-700 dark:text-gray-300">
            {{ t('admin.riskControl.allowedGroupCount', { count: groupIds.length }) }}
          </span>
        </div>
        <select class="input" :value="''" @change="onPickGroup">
          <option value="">{{ t('admin.riskControl.allowedGroupsPlaceholder') }}</option>
          <option v-for="g in groupOptions" :key="g.id" :value="g.id">{{ g.name }} · {{ g.platform }}{{ g.lane && g.lane !== 'normal' ? ' · ' + g.lane : '' }}</option>
        </select>
        <div v-if="groupIds.length > 0" class="mt-2 flex flex-wrap gap-2">
          <span
            v-for="id in groupIds"
            :key="id"
            class="inline-flex items-center gap-1 rounded-md bg-emerald-50 px-2 py-1 text-xs text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300"
          >
            <span class="font-mono">#{{ id }}</span>
            <span>{{ groupName(id) }}</span>
            <button type="button" class="ml-1 text-emerald-500 hover:text-emerald-700 dark:hover:text-emerald-200" :aria-label="t('common.delete')" @click="removeGroup(id)">✕</button>
          </span>
        </div>
        <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.riskControl.allowedGroupsHint') }}</p>
      </div>

      <div class="flex items-center justify-end gap-3">
        <span v-if="savedAt" class="text-xs text-emerald-600 dark:text-emerald-400">{{ t('admin.riskAllowlist.saved') }}</span>
        <span v-if="saveError" class="text-xs text-red-600 dark:text-red-400">{{ saveError }}</span>
        <button type="button" class="btn btn-primary btn-sm" :disabled="saving || loading || !dirty" @click="save">
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { AdminGroup } from '@/types'

/* 风控白名单(用户 + API 分组):与审计风控中心共用同一份配置(allowed_user_ids /
   allowed_group_ids)。三条线都认:内容审核跳过、提示词审计不留存、蒸馏检测不评分;
   加入即清理该用户 / 该分组下用户已有的三类数据。本面板只改这两个字段,其它配置不动。 */

interface UserChip {
  id: number
  email: string
}

const { t } = useI18n()
const open = ref(true)
const loading = ref(false)
const loadError = ref('')
const saving = ref(false)
const saveError = ref('')
const savedAt = ref<number | null>(null)

const users = ref<UserChip[]>([])
const groupIds = ref<number[]>([])
const groups = ref<AdminGroup[]>([])
let baseline = ''

const snapshot = () => JSON.stringify({ u: users.value.map((x) => x.id), g: [...groupIds.value] })
const dirty = computed(() => snapshot() !== baseline)

const groupOptions = computed(() => groups.value.filter((g) => !groupIds.value.includes(g.id)))
const groupName = (id: number) => groups.value.find((g) => g.id === id)?.name ?? t('admin.riskControl.allowedGroupUnknown')

// —— 用户搜索(复用用量页接口) ——
const userKeyword = ref('')
const userResults = ref<Array<{ id: number; email: string }>>([])
const showUserDropdown = ref(false)
const userSearchRef = ref<HTMLElement | null>(null)
let userSearchTimer: ReturnType<typeof setTimeout> | null = null

const debounceUserSearch = () => {
  if (userSearchTimer) clearTimeout(userSearchTimer)
  userSearchTimer = setTimeout(async () => {
    const keyword = userKeyword.value.trim()
    if (!keyword) {
      userResults.value = []
      return
    }
    try {
      const results = await adminAPI.usage.searchUsers(keyword)
      const chosen = new Set(users.value.map((u) => u.id))
      userResults.value = results.filter((u) => !u.deleted && !chosen.has(u.id)).map((u) => ({ id: u.id, email: u.email }))
      showUserDropdown.value = true
    } catch {
      userResults.value = []
    }
  }, 300)
}

const addUser = (u: { id: number; email: string }) => {
  if (!users.value.some((x) => x.id === u.id)) users.value.push({ id: u.id, email: u.email })
  userKeyword.value = ''
  userResults.value = []
  showUserDropdown.value = false
}

const addUserByKeyword = async () => {
  const keyword = userKeyword.value.trim()
  if (!keyword) return
  if (/^\d+$/.test(keyword)) {
    const id = Number(keyword)
    let email = ''
    try {
      email = (await adminAPI.users.getById(id)).email ?? ''
    } catch {
      email = ''
    }
    addUser({ id, email })
    return
  }
  if (userResults.value.length > 0) addUser(userResults.value[0])
}

const removeUser = (id: number) => {
  users.value = users.value.filter((u) => u.id !== id)
}

const onPickGroup = (e: Event) => {
  const el = e.target as HTMLSelectElement
  const id = Number(el.value)
  el.value = ''
  if (!id || groupIds.value.includes(id)) return
  groupIds.value.push(id)
}

const removeGroup = (id: number) => {
  groupIds.value = groupIds.value.filter((x) => x !== id)
}

const onClickOutside = (e: MouseEvent) => {
  if (userSearchRef.value && !userSearchRef.value.contains(e.target as Node)) showUserDropdown.value = false
}

async function hydrateUsers(ids: number[]) {
  users.value = ids.map((id) => ({ id, email: '' }))
  const resolved = await Promise.all(
    ids.map(async (id) => {
      try {
        return { id, email: (await adminAPI.users.getById(id, true)).email ?? '' }
      } catch {
        return { id, email: '' }
      }
    }),
  )
  users.value = resolved
  baseline = snapshot()
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [cfg, groupItems] = await Promise.all([
      adminAPI.riskControl.getConfig(),
      adminAPI.groups.getAllIncludingInactive().catch(() => adminAPI.groups.getAll()),
    ])
    groups.value = groupItems
    groupIds.value = Array.isArray(cfg.allowed_group_ids) ? [...cfg.allowed_group_ids] : []
    await hydrateUsers(Array.isArray(cfg.allowed_user_ids) ? cfg.allowed_user_ids : [])
  } catch (err) {
    loadError.value = (err as Error)?.message || t('admin.riskAllowlist.loadFailed')
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  saveError.value = ''
  savedAt.value = null
  try {
    const cfg = await adminAPI.riskControl.updateConfig({
      allowed_user_ids: users.value.map((u) => u.id),
      allowed_group_ids: [...groupIds.value],
    })
    groupIds.value = Array.isArray(cfg.allowed_group_ids) ? [...cfg.allowed_group_ids] : []
    users.value = (cfg.allowed_user_ids ?? []).map((id) => users.value.find((u) => u.id === id) ?? { id, email: '' })
    baseline = snapshot()
    savedAt.value = Date.now()
    setTimeout(() => (savedAt.value = null), 4000)
  } catch (err) {
    saveError.value = (err as Error)?.message || t('admin.riskAllowlist.saveFailed')
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  document.addEventListener('click', onClickOutside)
  void load()
})
onUnmounted(() => document.removeEventListener('click', onClickOutside))
</script>
