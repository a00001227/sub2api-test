import { defineStore } from 'pinia'
import { ref } from 'vue'
import { adminAPI } from '@/api/admin'

// 侧边栏「用户反馈」未处理数角标。
// 数据源复用现有列表接口:GET /admin/feedbacks?status=pending&page_size=1 的 total,
// 不新增后端接口。仅管理员登录时轮询;反馈页处理/回复后调用 refresh() 立即同步。
const POLL_MS = 60 * 1000

export const useAdminFeedbackStore = defineStore('adminFeedbacks', () => {
  const pendingCount = ref(0)
  const loaded = ref(false)

  let pollerId: ReturnType<typeof setInterval> | null = null
  let inflight: Promise<void> | null = null

  async function refresh() {
    // 合并并发调用,避免轮询与页面操作同时触发时重复请求。
    if (inflight) return inflight
    inflight = (async () => {
      try {
        const res = await adminAPI.feedbacks.list(1, 1, { status: 'pending' })
        pendingCount.value = res.total
        loaded.value = true
      } catch {
        // 静默:角标是辅助信息,失败保留上一次的值,下个周期再试。
      } finally {
        inflight = null
      }
    })()
    return inflight
  }

  function startPolling() {
    if (pollerId) return
    void refresh()
    pollerId = setInterval(() => {
      if (document.visibilityState === 'visible') void refresh()
    }, POLL_MS)
  }

  function stopPolling() {
    if (pollerId) {
      clearInterval(pollerId)
      pollerId = null
    }
  }

  return { pendingCount, loaded, refresh, startPolling, stopPolling }
})
