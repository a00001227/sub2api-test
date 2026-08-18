import { describe, expect, it, vi } from 'vitest'

// Keep the router import light: stub the navigation-side composables the guards
// pull in at module load, exactly like guards.spec.ts does.
vi.mock('@/composables/useNavigationLoading', () => ({
  useNavigationLoadingState: () => ({ startNavigation: vi.fn(), endNavigation: vi.fn(), isLoading: { value: false } }),
  useNavigationLoading: () => ({ startNavigation: vi.fn(), endNavigation: vi.fn(), isLoading: { value: false } }),
}))
vi.mock('@/composables/useRoutePrefetch', () => ({
  useRoutePrefetch: () => ({ triggerPrefetch: vi.fn(), cancelPendingPrefetch: vi.fn(), resetPrefetchState: vi.fn() }),
}))

describe('Risk V2 Shadow route is admin-guarded', () => {
  it('requires auth + admin in its route meta (server AdminGuard is the real boundary)', async () => {
    const { default: router } = await import('@/router')
    const route = router.getRoutes().find((r) => r.name === 'AdminRiskV2Shadow')
    expect(route, 'AdminRiskV2Shadow route missing').toBeTruthy()
    expect(route!.meta.requiresAuth).toBe(true)
    expect(route!.meta.requiresAdmin).toBe(true)
    expect(route!.path).toBe('/admin/risk-v2')
  })
})
