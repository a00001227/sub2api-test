<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-8">
      <!-- Header -->
      <div class="mb-4 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.riskV2.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.description') }}</p>
        </div>
        <button
          data-test="refresh-btn"
          @click="refreshAll"
          :disabled="statusLoading || listLoading"
          class="btn btn-secondary"
          :aria-label="t('admin.riskV2.ariaRefresh')"
          :title="t('admin.riskV2.refresh')"
        >
          <Icon name="refresh" size="md" :class="(statusLoading || listLoading) ? 'animate-spin' : ''" />
        </button>
      </div>

      <!-- Persistent Shadow banner -->
      <div class="mb-6 flex items-start gap-3 rounded-lg border-2 border-amber-300 bg-amber-50 px-4 py-3 dark:border-amber-700 dark:bg-amber-900/20">
        <Icon name="eye" size="lg" class="mt-0.5 text-amber-600 dark:text-amber-400" />
        <div class="flex-1">
          <p class="text-sm font-bold text-amber-800 dark:text-amber-200">{{ t('admin.riskV2.shadowBannerTitle') }}</p>
          <ul class="mt-1 list-disc space-y-0.5 pl-4 text-xs text-amber-700 dark:text-amber-300">
            <li>{{ t('admin.riskV2.shadowNote1') }}</li>
            <li>{{ t('admin.riskV2.shadowNote2') }}</li>
            <li>{{ t('admin.riskV2.shadowNote3') }}</li>
            <li>{{ t('admin.riskV2.shadowNote4') }}</li>
          </ul>
        </div>
        <span class="whitespace-nowrap rounded-full bg-amber-200 px-3 py-1 text-xs font-semibold text-amber-800 dark:bg-amber-800 dark:text-amber-100">
          {{ t('admin.riskV2.shadowBadge') }}
        </span>
      </div>

      <!-- Runtime Status -->
      <div class="mb-6">
        <div v-if="statusLoading && !status" data-test="status-loading" role="status" aria-live="polite" class="flex items-center justify-center py-8">
          <Icon name="refresh" size="lg" class="animate-spin text-gray-400" />
          <span class="sr-only">{{ t('admin.riskV2.loadingLabel') }}</span>
        </div>
        <template v-else-if="status">
          <!-- Disabled: neutral info, NOT a failure -->
          <div v-if="!status.enabled" data-test="status-disabled" class="rounded-lg border border-gray-200 bg-gray-50 px-4 py-3 text-sm text-gray-600 dark:border-dark-500 dark:bg-dark-700 dark:text-gray-300">
            {{ t('admin.riskV2.statusDisabled') }}
          </div>
          <template v-else>
            <div class="mb-3 flex flex-wrap items-center gap-2">
              <span data-test="mode-label" class="rounded-full bg-gray-100 px-2.5 py-0.5 text-xs font-medium text-gray-700 dark:bg-dark-600 dark:text-gray-200">{{ modeLabel }}</span>
              <span v-if="status.worker_degraded" class="rounded-full bg-yellow-100 px-2.5 py-0.5 text-xs font-medium text-yellow-800 dark:bg-yellow-900/30 dark:text-yellow-300">
                {{ t('admin.riskV2.workerDegraded') }}
              </span>
              <span v-if="status.leader" class="rounded-full bg-primary-50 px-2.5 py-0.5 text-xs font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">
                {{ t('admin.riskV2.leader') }}
              </span>
            </div>
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
              <div v-for="c in primaryCards" :key="c.label" class="card px-3 py-2">
                <div class="text-xs text-gray-500 dark:text-gray-400">{{ c.label }}</div>
                <div class="mt-0.5 text-sm font-semibold" :class="c.cls">{{ c.value }}</div>
              </div>
            </div>
            <button
              @click="statusOpen = !statusOpen"
              :aria-expanded="statusOpen"
              class="mt-3 text-xs text-primary-600 hover:underline dark:text-primary-400"
            >
              {{ statusOpen ? t('admin.riskV2.hideDetails') : t('admin.riskV2.showDetails') }}
            </button>
            <dl v-if="statusOpen" class="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-3 lg:grid-cols-4">
              <div v-for="s in secondaryFields" :key="s.label" class="flex justify-between gap-2">
                <dt class="text-gray-500 dark:text-gray-400">{{ s.label }}</dt>
                <dd class="font-mono text-gray-800 dark:text-gray-200">{{ s.value }}</dd>
              </div>
            </dl>
          </template>
        </template>
      </div>

      <!-- Filter bar -->
      <div class="mb-4 grid grid-cols-1 gap-3 rounded-lg border border-gray-200 p-3 dark:border-dark-500 sm:grid-cols-2 lg:grid-cols-4">
        <div>
          <label class="input-label" for="rv2-tier">{{ t('admin.riskV2.filterTier') }}</label>
          <select id="rv2-tier" v-model="filters.tier" class="input w-full">
            <option value="">{{ t('admin.riskV2.allTiers') }}</option>
            <option value="HIGH">{{ tierLabel('HIGH') }}</option>
            <option value="MEDIUM">{{ tierLabel('MEDIUM') }}</option>
            <option value="WATCH">{{ tierLabel('WATCH') }}</option>
            <option value="INSUFFICIENT_DATA">{{ tierLabel('INSUFFICIENT_DATA') }}</option>
          </select>
        </div>
        <div>
          <label class="input-label" for="rv2-min">{{ t('admin.riskV2.filterMinIndex') }}</label>
          <input id="rv2-min" v-model="filters.minRiskIndex" type="number" min="0" max="100" step="1" class="input w-full" placeholder="0 - 100" />
        </div>
        <div>
          <label class="input-label" for="rv2-uid">{{ t('admin.riskV2.filterUserId') }}</label>
          <input id="rv2-uid" v-model="filters.userId" type="number" min="1" step="1" class="input w-full" />
        </div>
        <div>
          <label class="input-label" for="rv2-fresh">{{ t('admin.riskV2.filterFreshness') }}</label>
          <select id="rv2-fresh" v-model="filters.freshness" class="input w-full">
            <option value="">{{ t('admin.riskV2.freshnessAll') }}</option>
            <option value="fresh">{{ t('admin.riskV2.freshnessFresh') }}</option>
            <option value="stale">{{ t('admin.riskV2.freshnessStale') }}</option>
          </select>
        </div>
        <div>
          <label class="input-label" for="rv2-data">{{ t('admin.riskV2.filterDataSufficient') }}</label>
          <select id="rv2-data" v-model="filters.dataSufficient" class="input w-full">
            <option value="">{{ t('admin.riskV2.any') }}</option>
            <option value="true">{{ t('admin.riskV2.yes') }}</option>
            <option value="false">{{ t('admin.riskV2.no') }}</option>
          </select>
        </div>
        <div>
          <label class="input-label" for="rv2-degraded">{{ t('admin.riskV2.filterDegraded') }}</label>
          <select id="rv2-degraded" v-model="filters.degraded" class="input w-full">
            <option value="">{{ t('admin.riskV2.any') }}</option>
            <option value="true">{{ t('admin.riskV2.yes') }}</option>
            <option value="false">{{ t('admin.riskV2.no') }}</option>
          </select>
        </div>
        <div>
          <label class="input-label" for="rv2-from">{{ t('admin.riskV2.filterFrom') }}</label>
          <input id="rv2-from" v-model="filters.from" type="datetime-local" class="input w-full" />
        </div>
        <div>
          <label class="input-label" for="rv2-to">{{ t('admin.riskV2.filterTo') }}</label>
          <input id="rv2-to" v-model="filters.to" type="datetime-local" class="input w-full" />
        </div>
        <div class="flex items-end gap-2 lg:col-span-4">
          <button data-test="query-btn" @click="applyFilters" :disabled="listLoading" class="btn btn-primary btn-sm">{{ t('admin.riskV2.query') }}</button>
          <button data-test="clear-btn" @click="clearFilters" :disabled="listLoading" class="btn btn-secondary btn-sm">{{ t('admin.riskV2.clearFilters') }}</button>
          <span v-if="filterError" data-test="filter-error" role="alert" class="text-xs text-red-600 dark:text-red-400">{{ filterError }}</span>
        </div>
      </div>

      <!-- Users table -->
      <div class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-500">
        <div v-if="schemaNotReady" data-test="schema-not-ready" class="py-16 text-center">
          <p class="text-sm font-medium text-gray-600 dark:text-gray-300">{{ t('admin.riskV2.schemaNotReadyTitle') }}</p>
          <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.riskV2.schemaNotReadyDesc') }}</p>
        </div>
        <div v-else-if="listError" data-test="list-error" role="alert" class="py-16 text-center">
          <p class="text-sm font-medium text-red-600 dark:text-red-400">{{ listError }}</p>
        </div>
        <div v-else-if="listLoading" data-test="list-loading" role="status" aria-live="polite" class="flex items-center justify-center py-16">
          <Icon name="refresh" size="xl" class="animate-spin text-gray-400" />
          <span class="sr-only">{{ t('admin.riskV2.loadingLabel') }}</span>
        </div>
        <!-- Dry Run + empty: must NOT read as "no risky users" -->
        <div v-else-if="items.length === 0 && isDryRun" data-test="dry-run-empty" class="py-16 text-center">
          <p class="text-sm font-medium text-gray-600 dark:text-gray-300">{{ t('admin.riskV2.dryRunEmptyTitle') }}</p>
          <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.riskV2.dryRunEmptyDesc') }}</p>
        </div>
        <div v-else-if="items.length === 0" data-test="list-empty" class="py-16 text-center">
          <p class="text-sm font-medium text-gray-600 dark:text-gray-300">{{ t('admin.riskV2.emptyTitle') }}</p>
          <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.riskV2.emptyDesc') }}</p>
        </div>
        <div v-else class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 dark:divide-dark-500">
            <thead class="bg-gray-50 dark:bg-dark-700">
              <tr>
                <th class="th-l">{{ t('admin.riskV2.colUser') }}</th>
                <th class="th-c">{{ t('admin.riskV2.colTier') }}</th>
                <th class="th-r">{{ t('admin.riskV2.colIndex') }}</th>
                <th class="th-r">{{ t('admin.riskV2.colConfidence') }}</th>
                <th class="th-c">{{ t('admin.riskV2.colData') }}</th>
                <th class="th-c">{{ t('admin.riskV2.colPlanes') }}</th>
                <th class="th-l">{{ t('admin.riskV2.colReasons') }}</th>
                <th class="th-c">{{ t('admin.riskV2.colFlags') }}</th>
                <th class="th-c">{{ t('admin.riskV2.colAssessed') }}</th>
                <th class="th-c"><span class="sr-only">{{ t('admin.riskV2.colActions') }}</span></th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-600 dark:bg-dark-800">
              <tr
                v-for="it in items"
                :key="it.user_id"
                data-test="user-row"
                class="hover:bg-gray-50 dark:hover:bg-dark-700"
              >
                <td class="px-4 py-3">
                  <div class="text-sm font-medium text-gray-900 dark:text-white">{{ it.user.email || t('admin.riskV2.unknownUser') }}</div>
                  <div class="font-mono text-xs text-gray-400 dark:text-gray-500">#{{ it.user_id }}</div>
                </td>
                <td class="px-4 py-3 text-center">
                  <span
                    :class="tierBadgeClass(it.risk_tier)"
                    class="inline-block rounded-full px-2.5 py-0.5 text-xs font-medium"
                    :tabindex="it.risk_tier === 'HIGH' ? 0 : undefined"
                    :aria-label="it.risk_tier === 'HIGH' ? tierLabel('HIGH') + ' — ' + t('admin.riskV2.highTooltip') : undefined"
                    :title="it.risk_tier === 'HIGH' ? t('admin.riskV2.highTooltip') : undefined"
                  >
                    {{ tierLabel(it.risk_tier) }}
                  </span>
                </td>
                <td class="px-4 py-3 text-right font-mono text-sm font-semibold text-gray-900 dark:text-white">{{ it.risk_index.toFixed(1) }}</td>
                <td class="px-4 py-3 text-right font-mono text-sm text-gray-600 dark:text-gray-300">{{ it.confidence.toFixed(2) }}</td>
                <td class="px-4 py-3 text-center">
                  <span :class="it.data_sufficient ? 'text-green-600 dark:text-green-400' : 'text-gray-400'" class="text-xs">
                    {{ it.data_sufficient ? t('admin.riskV2.yes') : t('admin.riskV2.no') }}
                  </span>
                </td>
                <td class="px-4 py-3">
                  <div class="flex flex-wrap gap-x-3 text-xs text-gray-600 dark:text-gray-300">
                    <span>A:<b class="ml-0.5 font-mono">{{ planeText(it.automation) }}</b></span>
                    <span>H:<b class="ml-0.5 font-mono">{{ planeText(it.harvest) }}</b></span>
                    <span>C:<b class="ml-0.5 font-mono">{{ planeText(it.campaign) }}</b></span>
                    <span>E:<b class="ml-0.5 font-mono">{{ planeText(it.exposure) }}</b></span>
                  </div>
                </td>
                <td class="px-4 py-3">
                  <div class="flex flex-wrap gap-1">
                    <span
                      v-for="rc in it.top_reason_codes"
                      :key="rc.code"
                      class="rounded bg-gray-100 px-1.5 py-0.5 text-[11px] text-gray-600 dark:bg-dark-600 dark:text-gray-300"
                      :title="rc.code"
                    >{{ reasonName(rc.code) }}</span>
                    <span v-if="it.top_reason_codes.length === 0" class="text-xs text-gray-400">—</span>
                  </div>
                </td>
                <td class="px-4 py-3 text-center">
                  <div class="flex flex-wrap justify-center gap-1 text-[11px]">
                    <span v-if="it.degraded" class="rounded bg-orange-100 px-1.5 text-orange-700 dark:bg-orange-900/30 dark:text-orange-300">{{ t('admin.riskV2.flagDegraded') }}</span>
                    <span v-if="it.incomplete" class="rounded bg-gray-100 px-1.5 text-gray-600 dark:bg-dark-600 dark:text-gray-300">{{ t('admin.riskV2.flagIncomplete') }}</span>
                    <span v-if="!it.health_available" class="rounded bg-gray-100 px-1.5 text-gray-600 dark:bg-dark-600 dark:text-gray-300">{{ t('admin.riskV2.flagHealthNA') }}</span>
                    <span v-if="it.time_anomaly" class="rounded bg-red-100 px-1.5 text-red-700 dark:bg-red-900/30 dark:text-red-300">{{ t('admin.riskV2.flagTimeAnomaly') }}</span>
                  </div>
                </td>
                <td class="px-4 py-3 text-center">
                  <div class="text-xs text-gray-600 dark:text-gray-300">{{ fmtTime(it.assessed_at) }}</div>
                  <span :class="it.fresh ? 'text-green-600 dark:text-green-400' : 'text-gray-400'" class="text-[11px]">
                    {{ it.fresh ? t('admin.riskV2.fresh') : t('admin.riskV2.stale') }}
                  </span>
                </td>
                <td class="px-4 py-3 text-center">
                  <button
                    data-test="view-detail-btn"
                    @click="openDetail(it.user_id, $event)"
                    class="btn btn-secondary btn-xs"
                    :aria-label="t('admin.riskV2.ariaViewDetail', { user: it.user.email || ('#' + it.user_id) })"
                  >{{ t('admin.riskV2.viewDetail') }}</button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- Pagination -->
      <div v-if="!schemaNotReady && !listError && (items.length > 0 || offset > 0)" class="mt-4 flex items-center justify-between text-sm">
        <div class="flex items-center gap-2">
          <label class="text-gray-500 dark:text-gray-400" for="rv2-page-size">{{ t('admin.riskV2.pageSize') }}</label>
          <select id="rv2-page-size" v-model.number="pageSize" @change="onPageSizeChange" class="input w-24">
            <option :value="20">20</option>
            <option :value="50">50</option>
            <option :value="100">100</option>
          </select>
        </div>
        <div class="flex items-center gap-3">
          <span class="text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.pageLabel', { page: currentPage }) }}</span>
          <button data-test="prev-btn" @click="prevPage" :disabled="offset === 0 || listLoading" class="btn btn-secondary btn-sm">{{ t('admin.riskV2.prev') }}</button>
          <button data-test="next-btn" @click="nextPage" :disabled="!hasMore || listLoading" class="btn btn-secondary btn-sm">{{ t('admin.riskV2.next') }}</button>
        </div>
      </div>
    </div>

    <!-- Detail drawer -->
    <div v-if="detailOpen" class="fixed inset-0 z-50 flex justify-end bg-black/40" @click.self="closeDetail">
      <div
        ref="drawerRef"
        data-test="drawer"
        role="dialog"
        aria-modal="true"
        aria-labelledby="rv2-drawer-title"
        tabindex="-1"
        class="flex h-full w-full max-w-lg flex-col overflow-y-auto bg-white shadow-xl outline-none dark:bg-dark-800"
        @keydown="onDrawerKeydown"
      >
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-dark-500">
          <div>
            <h3 id="rv2-drawer-title" class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.riskV2.detailTitle') }}</h3>
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ detail?.user.email || ('#' + detailUserId) }}</p>
          </div>
          <button
            ref="closeBtnRef"
            data-test="drawer-close"
            @click="closeDetail"
            class="btn btn-secondary btn-xs"
            :aria-label="t('admin.riskV2.ariaClose')"
          >{{ t('admin.riskV2.close') }}</button>
        </div>

        <div v-if="detailLoading" data-test="detail-loading" role="status" aria-live="polite" class="flex items-center justify-center py-16">
          <Icon name="refresh" size="lg" class="animate-spin text-gray-400" />
          <span class="sr-only">{{ t('admin.riskV2.loadingLabel') }}</span>
        </div>
        <div v-else-if="detailNotFound" data-test="detail-not-found" role="alert" class="p-6 text-center text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.riskV2.detailNotFound') }}
        </div>
        <div v-else-if="detailError" data-test="detail-error" role="alert" class="p-6 text-center text-sm text-red-600 dark:text-red-400">
          {{ detailError }}
        </div>
        <div v-else-if="detail" class="flex-1 space-y-6 p-5">
          <div class="flex flex-wrap items-center gap-4">
            <div>
              <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.colIndex') }}</div>
              <div class="font-mono text-2xl font-bold text-gray-900 dark:text-white">{{ detail.risk_index.toFixed(1) }}</div>
            </div>
            <span :class="tierBadgeClass(detail.risk_tier)" class="rounded-full px-2.5 py-0.5 text-xs font-medium">{{ tierLabel(detail.risk_tier) }}</span>
            <span data-test="effective-action" class="rounded-full bg-gray-100 px-2 py-0.5 text-[11px] text-gray-500 dark:bg-dark-600 dark:text-gray-400">{{ t('admin.riskV2.effectiveAction') }}: {{ detail.effective_action }}</span>
          </div>

          <!-- Planes -->
          <div>
            <p class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.riskV2.planesSection') }}</p>
            <dl class="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
              <div v-for="pl in detailPlanes" :key="pl.label"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ pl.label }}</dt><dd class="font-mono text-gray-900 dark:text-white">{{ pl.text }}</dd></div>
            </dl>
          </div>

          <!-- Evidence groups -->
          <div v-if="detail.evidence_groups.length">
            <p class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.riskV2.groupsSection') }}</p>
            <div class="max-h-48 space-y-1 overflow-y-auto pr-1">
              <div v-for="g in detail.evidence_groups" :key="g.group" class="flex items-center justify-between text-xs">
                <span class="text-gray-600 dark:text-gray-300">
                  {{ groupName(g.group) }}
                  <span v-if="g.met_high" class="ml-1 text-red-600 dark:text-red-400" :aria-label="t('admin.riskV2.metHigh')">● {{ t('admin.riskV2.metHigh') }}</span>
                </span>
                <span class="font-mono text-gray-500 dark:text-gray-400">{{ g.strength.toFixed(1) }}</span>
              </div>
            </div>
          </div>

          <!-- Evidence families -->
          <div v-if="detail.evidence_families.length">
            <p class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.riskV2.familiesSection') }}</p>
            <div class="max-h-48 space-y-1 overflow-y-auto pr-1">
              <div v-for="f in detail.evidence_families" :key="f.family + f.window" class="flex items-center justify-between text-xs">
                <span class="text-gray-600 dark:text-gray-300">{{ familyName(f.family) }} <span class="text-gray-400">· {{ f.window }}</span></span>
                <span class="font-mono text-gray-500 dark:text-gray-400">{{ f.available ? f.strength.toFixed(1) : t('admin.riskV2.na') }}</span>
              </div>
            </div>
          </div>

          <!-- Reason codes (human-readable + raw code) -->
          <div v-if="detail.reason_codes.length">
            <p class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.riskV2.reasonsSection') }}</p>
            <div class="max-h-64 space-y-1.5 overflow-y-auto pr-1">
              <div v-for="rc in detail.reason_codes" :key="rc.code + rc.window" class="rounded bg-gray-50 px-2 py-1.5 text-xs dark:bg-dark-700">
                <div class="flex items-baseline justify-between gap-2">
                  <span class="font-medium text-gray-800 dark:text-gray-200">{{ reasonName(rc.code) }}</span>
                  <span class="font-mono text-[10px] text-gray-400 dark:text-gray-500">{{ rc.code }}</span>
                </div>
                <div class="mt-0.5 flex flex-wrap gap-x-3 text-[11px] text-gray-500 dark:text-gray-400">
                  <span>{{ t('admin.riskV2.reasonWindow') }}: <b class="font-mono">{{ rc.window }}</b></span>
                  <span>{{ t('admin.riskV2.reasonObserved') }}: <b class="font-mono">{{ rc.observed_value.toFixed(2) }}</b></span>
                  <span>{{ t('admin.riskV2.reasonThreshold') }}: <b class="font-mono">{{ rc.threshold.toFixed(2) }}</b></span>
                  <span v-if="rc.evidence_family">{{ t('admin.riskV2.reasonFamily') }}: <b>{{ familyName(rc.evidence_family) }}</b></span>
                  <span v-if="rc.evidence_group">{{ t('admin.riskV2.reasonGroup') }}: <b>{{ groupName(rc.evidence_group) }}</b></span>
                  <span v-if="rc.confidence_contribution">{{ t('admin.riskV2.reasonConfidence') }}: <b class="font-mono">{{ rc.confidence_contribution.toFixed(2) }}</b></span>
                </div>
              </div>
            </div>
          </div>

          <!-- Incomplete reasons -->
          <div v-if="detail.incomplete_reasons.length">
            <p class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.riskV2.incompleteSection') }}</p>
            <div class="flex flex-wrap gap-1">
              <span v-for="(ir, i) in detail.incomplete_reasons" :key="i" class="rounded bg-gray-100 px-1.5 py-0.5 text-[11px] text-gray-600 dark:bg-dark-600 dark:text-gray-300" :title="ir">{{ incompleteText(ir) }}</span>
            </div>
          </div>

          <!-- Flags + versions -->
          <div>
            <dl class="grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
              <div class="flex justify-between"><dt class="text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.colConfidence') }}</dt><dd class="font-mono">{{ detail.confidence.toFixed(2) }}</dd></div>
              <div class="flex justify-between"><dt class="text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.colData') }}</dt><dd>{{ detail.data_sufficient ? t('admin.riskV2.yes') : t('admin.riskV2.no') }}</dd></div>
              <div class="flex justify-between"><dt class="text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.flagHealthNA') }}</dt><dd>{{ detail.health_available ? t('admin.riskV2.yes') : t('admin.riskV2.no') }}</dd></div>
              <div class="flex justify-between"><dt class="text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.fresh') }}</dt><dd>{{ detail.fresh ? t('admin.riskV2.yes') : t('admin.riskV2.no') }}</dd></div>
              <div class="flex justify-between"><dt class="text-gray-500 dark:text-gray-400">{{ t('admin.riskV2.colAssessed') }}</dt><dd class="font-mono">{{ fmtTime(detail.assessed_at) }}</dd></div>
              <div v-if="detail.time_anomaly" class="flex justify-between text-red-600 dark:text-red-400"><dt>{{ t('admin.riskV2.flagTimeAnomaly') }}</dt><dd>!</dd></div>
              <div class="flex justify-between"><dt class="text-gray-500 dark:text-gray-400">feature</dt><dd class="font-mono">{{ detail.feature_version }}</dd></div>
              <div class="flex justify-between"><dt class="text-gray-500 dark:text-gray-400">policy</dt><dd class="font-mono">{{ detail.policy_version }}</dd></div>
            </dl>
          </div>

          <!-- Live windows (manual, on-demand only) -->
          <div class="border-t border-gray-100 pt-4 dark:border-dark-600">
            <div class="mb-2 flex items-center justify-between">
              <p class="text-sm font-semibold text-gray-700 dark:text-gray-300">{{ t('admin.riskV2.liveSection') }}</p>
              <button
                data-test="live-load-btn"
                @click="loadWindows"
                :disabled="windowsLoading || cooldownRemaining > 0"
                class="btn btn-secondary btn-xs"
                :aria-label="t('admin.riskV2.ariaLoadLive')"
              >
                <span v-if="windowsLoading">{{ t('admin.riskV2.liveLoadingLabel') }}</span>
                <span v-else-if="cooldownRemaining > 0">{{ t('admin.riskV2.liveCooldown', { sec: cooldownRemaining }) }}</span>
                <span v-else>{{ windowsLoaded ? t('admin.riskV2.liveReload') : t('admin.riskV2.liveLoadBtn') }}</span>
              </button>
            </div>
            <p class="mb-2 text-[11px] text-gray-400 dark:text-gray-500">{{ t('admin.riskV2.liveHint') }}</p>

            <div v-if="windowsLoading" data-test="live-loading" role="status" aria-live="polite" class="flex items-center justify-center py-6">
              <Icon name="refresh" size="md" class="animate-spin text-gray-400" />
              <span class="sr-only">{{ t('admin.riskV2.loadingLabel') }}</span>
            </div>
            <div v-else-if="windowsError" data-test="live-error" role="alert" class="rounded bg-gray-50 px-3 py-2 text-xs text-gray-500 dark:bg-dark-700 dark:text-gray-400">{{ windowsError }}</div>
            <div v-else-if="windows && !windows.data_available" data-test="live-no-data" class="rounded bg-gray-50 px-3 py-2 text-xs text-gray-500 dark:bg-dark-700 dark:text-gray-400">{{ t('admin.riskV2.liveNoData') }}</div>
            <div v-else-if="windows" data-test="live-region">
              <table class="min-w-full text-xs">
                <thead><tr class="text-gray-500 dark:text-gray-400"><th class="py-1 text-left"></th><th class="py-1 text-right">5m</th><th class="py-1 text-right">1h</th><th class="py-1 text-right">24h</th></tr></thead>
                <tbody class="font-mono text-gray-800 dark:text-gray-200">
                  <tr><td class="py-0.5 font-sans text-gray-500">{{ t('admin.riskV2.winRequests') }}</td><td class="text-right">{{ wCell(windows.w_5m, 'request_count') }}</td><td class="text-right">{{ wCell(windows.w_1h, 'request_count') }}</td><td class="text-right">{{ wCell(windows.w_24h, 'request_count') }}</td></tr>
                  <tr><td class="py-0.5 font-sans text-gray-500">{{ t('admin.riskV2.winActiveMin') }}</td><td class="text-right">{{ wCell(windows.w_5m, 'active_minutes') }}</td><td class="text-right">{{ wCell(windows.w_1h, 'active_minutes') }}</td><td class="text-right">{{ wCell(windows.w_24h, 'active_minutes') }}</td></tr>
                  <tr><td class="py-0.5 font-sans text-gray-500">{{ t('admin.riskV2.winOutTokens') }}</td><td class="text-right">{{ wCell(windows.w_5m, 'output_tokens') }}</td><td class="text-right">{{ wCell(windows.w_1h, 'output_tokens') }}</td><td class="text-right">{{ wCell(windows.w_24h, 'output_tokens') }}</td></tr>
                  <tr><td class="py-0.5 font-sans text-gray-500">{{ t('admin.riskV2.winDistinctScaffold') }}</td><td class="text-right">{{ wCell(windows.w_5m, 'distinct_full_scaffold_estimate') }}</td><td class="text-right">{{ wCell(windows.w_1h, 'distinct_full_scaffold_estimate') }}</td><td class="text-right">{{ wCell(windows.w_24h, 'distinct_full_scaffold_estimate') }}</td></tr>
                </tbody>
              </table>
              <div class="mt-2 flex flex-wrap gap-x-4 gap-y-0.5 text-[11px] text-gray-500 dark:text-gray-400">
                <span>{{ t('admin.riskV2.mkActiveKeys') }}: <b class="font-mono">{{ mkCell('active_api_key_count_24h') }}</b></span>
                <span>{{ t('admin.riskV2.mkSync') }}: <b class="font-mono">{{ mkCell('synchronized_multi_key_minutes_1h') }}</b></span>
                <span>{{ t('admin.riskV2.mkOverlap') }}: <b class="font-mono">{{ windows.multi_key.cross_key_full_scaffold_overlap_available_1h ? windows.multi_key.cross_key_full_scaffold_overlap_estimate_1h : t('admin.riskV2.na') }}</b></span>
                <span>{{ t('admin.riskV2.mkHandoff') }}: <b class="font-mono">{{ mkCell('key_handoff_count_1h') }}</b></span>
              </div>
            </div>

            <!-- Explicit shadow limitations, always shown -->
            <ul class="mt-3 list-disc space-y-0.5 pl-4 text-[11px] text-gray-400 dark:text-gray-500">
              <li>{{ t('admin.riskV2.liveCacheNote') }}</li>
              <li>{{ t('admin.riskV2.liveNearDupNote') }}</li>
            </ul>
          </div>

          <!-- 疑似蒸馏取证：请求原文捕获（管理员定向） -->
          <EvidenceCapturePanel :user-id="detailUserId" />
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import EvidenceCapturePanel from '@/components/admin/EvidenceCapturePanel.vue'
import Icon from '@/components/icons/Icon.vue'
import {
  riskV2API,
  type RiskV2Tier,
  type RiskV2RuntimeStatus,
  type RiskV2AssessmentListItem,
  type RiskV2AssessmentDetail,
  type RiskV2WindowSummary,
  type RiskV2Window,
  type RiskV2MultiKey,
  type RiskV2Plane,
  type ListRiskV2UsersParams,
} from '@/api/admin/riskV2'

const { t, te } = useI18n()

const LIVE_COOLDOWN_SECONDS = 8

// —— Runtime status ——
const status = ref<RiskV2RuntimeStatus | null>(null)
const statusLoading = ref(false)
const statusOpen = ref(false)

// —— List ——
const items = ref<RiskV2AssessmentListItem[]>([])
const listLoading = ref(false)
const listError = ref('')
const schemaNotReady = ref(false)
const hasMore = ref(false)
const offset = ref(0)
const pageSize = ref(20)
const currentPage = computed(() => Math.floor(offset.value / pageSize.value) + 1)

// Dry Run = enabled + aggregating + scoring, but NOT persisting.
const isDryRun = computed(() => {
  const s = status.value
  return !!(s && s.enabled && s.aggregation_enabled && s.scoring_enabled && !s.persist_enabled)
})

// —— Filters (raw string inputs; validated before query) ——
const filters = reactive({
  tier: '' as '' | RiskV2Tier,
  minRiskIndex: '' as string | number,
  userId: '' as string | number,
  freshness: '' as '' | 'fresh' | 'stale',
  dataSufficient: '' as '' | 'true' | 'false',
  degraded: '' as '' | 'true' | 'false',
  from: '',
  to: '',
})
const filterError = ref('')

// —— Detail drawer ——
const detailOpen = ref(false)
const detailUserId = ref(0)
const detail = ref<RiskV2AssessmentDetail | null>(null)
const detailLoading = ref(false)
const detailError = ref('')
const detailNotFound = ref(false)
const drawerRef = ref<HTMLElement | null>(null)
const closeBtnRef = ref<HTMLElement | null>(null)
let lastFocused: HTMLElement | null = null

// —— Live windows (manual load only) ——
const windows = ref<RiskV2WindowSummary | null>(null)
const windowsLoading = ref(false)
const windowsError = ref('')
const windowsLoaded = ref(false)
const cooldownRemaining = ref(0)
let cooldownTimer: ReturnType<typeof setInterval> | null = null

// —— Abort + stale-response guards ——
let statusAbort: AbortController | null = null
let listAbort: AbortController | null = null
let detailAbort: AbortController | null = null
let windowsAbort: AbortController | null = null
let listSeq = 0
let detailSeq = 0
let windowsSeq = 0

// A cancelled request must never surface as a business error.
function isCancel(e: any): boolean {
  return e?.code === 'ERR_CANCELED' || e?.name === 'CanceledError' || e?.name === 'AbortError'
}

// —— i18n code → human label, with raw-code fallback for unknown enums ——
function labelFor(kind: string, code: string): string {
  if (!code) return ''
  const key = `admin.riskV2.${kind}.${code}`
  return te(key) ? t(key) : code
}
function reasonName(code: string): string {
  return labelFor('reason', code)
}
function familyName(family: string): string {
  return labelFor('family', family)
}
function groupName(group: string): string {
  return labelFor('group', group)
}
function incompleteText(reason: string): string {
  // Windowed reasons carry a ":<window>" suffix — map the prefix, keep the suffix raw.
  const idx = reason.indexOf(':')
  const prefix = idx >= 0 ? reason.slice(0, idx) : reason
  const suffix = idx >= 0 ? reason.slice(idx + 1) : ''
  const key = `admin.riskV2.incomplete.${prefix}`
  const base = te(key) ? t(key) : reason
  return suffix ? `${base} · ${suffix}` : base
}
// Map an API error object to a user-facing message (code first, then status).
function apiErrorText(e: any, fallbackKey: string): string {
  const code = e?.code as string | undefined
  if (code) {
    const key = `admin.riskV2.apiError.${code}`
    if (te(key)) return t(key)
  }
  switch (e?.status) {
    case 401:
      return t('admin.riskV2.errUnauthorized')
    case 403:
      return t('admin.riskV2.errForbidden')
    case 429:
      return t('admin.riskV2.errRateLimited')
    case 500:
      return t('admin.riskV2.errServer')
    case 503:
      return t('admin.riskV2.errUnavailable')
    default:
      return e?.message && typeof e.message === 'string' ? e.message : t(fallbackKey)
  }
}

const modeLabel = computed(() => {
  const s = status.value
  if (!s || !s.enabled) return ''
  if (!s.aggregation_enabled) return t('admin.riskV2.modeEnabledOnly')
  if (!s.scoring_enabled) return t('admin.riskV2.modeAggOnly')
  if (!s.persist_enabled) return t('admin.riskV2.modeDryRun')
  return t('admin.riskV2.modePersist')
})

const primaryCards = computed(() => {
  const s = status.value
  if (!s) return []
  const yn = (b: boolean) => (b ? t('admin.riskV2.yes') : t('admin.riskV2.no'))
  const okCls = (b: boolean) => (b ? 'text-green-600 dark:text-green-400' : 'text-gray-400')
  return [
    { label: t('admin.riskV2.stWorkerReady'), value: yn(s.worker_ready), cls: okCls(s.worker_ready) },
    { label: t('admin.riskV2.stSchemaReady'), value: yn(s.schema_ready), cls: okCls(s.schema_ready) },
    { label: t('admin.riskV2.stHealthAvail'), value: yn(s.health_available), cls: okCls(s.health_available) },
    { label: t('admin.riskV2.stDropRatio'), value: (s.observation_drop_ratio * 100).toFixed(1) + '%', cls: 'text-gray-900 dark:text-white' },
    { label: t('admin.riskV2.stQueueDepth'), value: String(s.queue_depth), cls: 'text-gray-900 dark:text-white' },
    { label: t('admin.riskV2.stCoverage'), value: (s.health_coverage_ratio * 100).toFixed(0) + '%', cls: 'text-gray-900 dark:text-white' },
  ]
})

const secondaryFields = computed(() => {
  const s = status.value
  if (!s) return []
  return [
    { label: t('admin.riskV2.stAggregation'), value: s.aggregation_enabled ? t('admin.riskV2.yes') : t('admin.riskV2.no') },
    { label: t('admin.riskV2.stScoring'), value: s.scoring_enabled ? t('admin.riskV2.yes') : t('admin.riskV2.no') },
    { label: t('admin.riskV2.stPersist'), value: s.persist_enabled ? t('admin.riskV2.yes') : t('admin.riskV2.no') },
    { label: t('admin.riskV2.stDispatcher'), value: s.dispatcher_ready ? t('admin.riskV2.yes') : t('admin.riskV2.no') },
    { label: t('admin.riskV2.stReporter'), value: s.reporter_ready ? t('admin.riskV2.yes') : t('admin.riskV2.no') },
    { label: t('admin.riskV2.stCurrentCycle'), value: fmtTime(s.current_cycle) },
    { label: t('admin.riskV2.stLastCycle'), value: fmtTime(s.last_completed_cycle) },
    { label: t('admin.riskV2.stLastCycleStatus'), value: s.last_cycle_status || '—' },
    { label: t('admin.riskV2.stLastError'), value: s.last_error_code || '—' },
    { label: t('admin.riskV2.stUpdated'), value: fmtTime(s.last_updated_at) },
  ]
})

const detailPlanes = computed(() => {
  const d = detail.value
  if (!d) return []
  return [
    { label: t('admin.riskV2.planeAutomation'), text: planeText(d.automation) },
    { label: t('admin.riskV2.planeHarvest'), text: planeText(d.harvest) },
    { label: t('admin.riskV2.planeCampaign'), text: planeText(d.campaign) },
    { label: t('admin.riskV2.planeExposure'), text: planeText(d.exposure) },
  ]
})

function planeText(p: RiskV2Plane): string {
  return p.available && p.score != null ? p.score.toFixed(1) : t('admin.riskV2.na')
}

// Window / multi-key cells: unavailable → N/A, never 0.
function wCell(w: RiskV2Window, key: keyof RiskV2Window): string {
  if (!w || !w.available) return t('admin.riskV2.na')
  return String(w[key])
}
function mkCell(key: keyof RiskV2MultiKey): string {
  const mk = windows.value?.multi_key
  if (!mk || !mk.multi_key_available) return t('admin.riskV2.na')
  return String(mk[key])
}

function tierBadgeClass(tier: RiskV2Tier): string {
  switch (tier) {
    case 'HIGH':
      return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'MEDIUM':
      return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-300'
    case 'WATCH':
      return 'bg-blue-50 text-blue-600 dark:bg-blue-900/20 dark:text-blue-300'
    default:
      return 'bg-gray-100 text-gray-500 dark:bg-dark-600 dark:text-gray-400'
  }
}

function tierLabel(tier: RiskV2Tier): string {
  return t(`admin.riskV2.tier.${tier}`)
}

function fmtTime(unix: number): string {
  if (!unix || unix <= 0) return '—'
  return new Date(unix * 1000).toLocaleString()
}

// —— Filter validation: never send NaN/Inf/empty; from<=to; positive user_id ——
function buildParams(): ListRiskV2UsersParams | null {
  filterError.value = ''
  const p: ListRiskV2UsersParams = {}
  if (filters.tier) p.tier = filters.tier
  if (filters.freshness) p.freshness = filters.freshness
  if (filters.dataSufficient) p.data_sufficient = filters.dataSufficient === 'true'
  if (filters.degraded) p.degraded = filters.degraded === 'true'

  if (filters.minRiskIndex !== '' && filters.minRiskIndex !== null) {
    const n = Number(filters.minRiskIndex)
    if (!isFinite(n) || n < 0 || n > 100) {
      filterError.value = t('admin.riskV2.errIndexRange')
      return null
    }
    p.min_risk_index = n
  }
  if (filters.userId !== '' && filters.userId !== null) {
    const n = Number(filters.userId)
    if (!Number.isInteger(n) || n <= 0) {
      filterError.value = t('admin.riskV2.errUserId')
      return null
    }
    p.user_id = n
  }
  let fromU = 0
  let toU = 0
  if (filters.from) {
    fromU = Math.floor(new Date(filters.from).getTime() / 1000)
    if (!isFinite(fromU)) {
      filterError.value = t('admin.riskV2.errTime')
      return null
    }
    p.assessed_from = fromU
  }
  if (filters.to) {
    toU = Math.floor(new Date(filters.to).getTime() / 1000)
    if (!isFinite(toU)) {
      filterError.value = t('admin.riskV2.errTime')
      return null
    }
    p.assessed_to = toU
  }
  if (fromU && toU && fromU > toU) {
    filterError.value = t('admin.riskV2.errFromAfterTo')
    return null
  }
  return p
}

async function loadStatus() {
  statusAbort?.abort()
  statusAbort = new AbortController()
  statusLoading.value = true
  try {
    status.value = await riskV2API.getStatus({ signal: statusAbort.signal })
  } catch (e: any) {
    if (isCancel(e)) return
    // Runtime status failing is not fatal — the list area stays usable.
    status.value = null
  } finally {
    statusLoading.value = false
  }
}

async function loadUsers() {
  const params = buildParams()
  if (params === null) return // invalid input → do NOT hit the API
  listAbort?.abort()
  listAbort = new AbortController()
  const seq = ++listSeq
  listLoading.value = true
  schemaNotReady.value = false
  listError.value = ''
  try {
    const resp = await riskV2API.listUsers(
      { ...params, limit: pageSize.value, offset: offset.value },
      { signal: listAbort.signal },
    )
    if (seq !== listSeq) return // a newer request superseded this one
    items.value = resp.items ?? []
    hasMore.value = resp.has_more
  } catch (e: any) {
    if (isCancel(e)) return // ignore cancellations entirely
    if (seq !== listSeq) return
    if (e?.code === 'RISK_V2_SCHEMA_NOT_READY') {
      schemaNotReady.value = true
      items.value = []
    } else {
      listError.value = apiErrorText(e, 'admin.riskV2.loadFail')
      items.value = []
    }
  } finally {
    if (seq === listSeq) listLoading.value = false
  }
}

function applyFilters() {
  offset.value = 0
  loadUsers()
}
function clearFilters() {
  filters.tier = ''
  filters.minRiskIndex = ''
  filters.userId = ''
  filters.freshness = ''
  filters.dataSufficient = ''
  filters.degraded = ''
  filters.from = ''
  filters.to = ''
  filterError.value = ''
  offset.value = 0
  loadUsers()
}
function onPageSizeChange() {
  offset.value = 0
  loadUsers()
}
function prevPage() {
  if (offset.value <= 0) return
  offset.value = Math.max(0, offset.value - pageSize.value)
  loadUsers()
}
function nextPage() {
  if (!hasMore.value) return
  offset.value += pageSize.value
  loadUsers()
}

// —— Detail drawer ——
async function openDetail(userId: number, ev?: Event) {
  // Remember the trigger so focus can be restored on close.
  lastFocused = (ev?.currentTarget as HTMLElement) || (document.activeElement as HTMLElement) || null
  // Switching users: cancel the previous detail + any live-window request and
  // reset all per-user state so a late response can never overwrite the new user.
  detailAbort?.abort()
  detailAbort = new AbortController()
  resetLiveWindows()
  const seq = ++detailSeq
  detailOpen.value = true
  detailUserId.value = userId
  detail.value = null
  detailError.value = ''
  detailNotFound.value = false
  detailLoading.value = true
  await nextTick()
  focusDrawer()
  try {
    const d = await riskV2API.getUser(userId, { signal: detailAbort.signal })
    if (seq !== detailSeq) return
    detail.value = d
  } catch (e: any) {
    if (isCancel(e)) return
    if (seq !== detailSeq) return
    if (e?.code === 'RISK_V2_ASSESSMENT_NOT_FOUND' || e?.status === 404) {
      detailNotFound.value = true
    } else {
      detailError.value = apiErrorText(e, 'admin.riskV2.detailFail')
    }
    // NOTE: never touch items.value here — a detail failure must not clear the list.
  } finally {
    if (seq === detailSeq) detailLoading.value = false
  }
  // Live windows are NOT fetched here — they load only on explicit user action.
}

function closeDetail() {
  detailAbort?.abort()
  detailAbort = null
  detailSeq++ // invalidate any in-flight detail response
  resetLiveWindows()
  detailOpen.value = false
  detail.value = null
  detailError.value = ''
  detailNotFound.value = false
  detailUserId.value = 0
  restoreFocus()
}

// —— Live windows: manual load + cooldown ——
function resetLiveWindows() {
  windowsAbort?.abort()
  windowsAbort = null
  windowsSeq++
  windows.value = null
  windowsError.value = ''
  windowsLoaded.value = false
  windowsLoading.value = false
  clearCooldown()
}

async function loadWindows() {
  if (!detailUserId.value) return
  if (windowsLoading.value) return // no double-submit
  if (cooldownRemaining.value > 0) return // cooldown active
  windowsAbort?.abort()
  windowsAbort = new AbortController()
  const seq = ++windowsSeq
  windowsLoading.value = true
  windowsError.value = ''
  try {
    const w = await riskV2API.getWindows(detailUserId.value, { signal: windowsAbort.signal })
    if (seq !== windowsSeq) return
    windows.value = w
    windowsLoaded.value = true
  } catch (e: any) {
    if (isCancel(e)) return
    if (seq !== windowsSeq) return
    windows.value = null
    windowsLoaded.value = true
    if (e?.code === 'RISK_V2_LIVE_METRICS_RATE_LIMITED') {
      windowsError.value = t('admin.riskV2.liveRateLimited')
    } else if (e?.code === 'RISK_V2_LIVE_METRICS_BUSY') {
      windowsError.value = t('admin.riskV2.liveBusy')
    } else {
      windowsError.value = t('admin.riskV2.liveUnavailable')
    }
    // A live-window failure must never affect the loaded assessment detail.
  } finally {
    if (seq === windowsSeq) {
      windowsLoading.value = false
      startCooldown()
    }
  }
}

function startCooldown() {
  clearCooldown()
  cooldownRemaining.value = LIVE_COOLDOWN_SECONDS
  cooldownTimer = setInterval(() => {
    cooldownRemaining.value -= 1
    if (cooldownRemaining.value <= 0) clearCooldown()
  }, 1000)
}
function clearCooldown() {
  if (cooldownTimer) {
    clearInterval(cooldownTimer)
    cooldownTimer = null
  }
  cooldownRemaining.value = 0
}

// —— Focus management ——
function focusDrawer() {
  const el = closeBtnRef.value || drawerRef.value
  el?.focus?.()
}
function restoreFocus() {
  const el = lastFocused
  lastFocused = null
  el?.focus?.()
}
function onDrawerKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    e.stopPropagation()
    closeDetail()
    return
  }
  if (e.key !== 'Tab' || !drawerRef.value) return
  // Minimal focus trap: keep Tab focus inside the drawer.
  const focusable = Array.from(
    drawerRef.value.querySelectorAll<HTMLElement>(
      'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])',
    ),
  ).filter((el) => el.offsetParent !== null || el === document.activeElement)
  if (focusable.length === 0) {
    e.preventDefault()
    drawerRef.value.focus()
    return
  }
  const first = focusable[0]
  const last = focusable[focusable.length - 1]
  const active = document.activeElement as HTMLElement
  if (e.shiftKey && (active === first || active === drawerRef.value)) {
    e.preventDefault()
    last.focus()
  } else if (!e.shiftKey && active === last) {
    e.preventDefault()
    first.focus()
  }
}

function refreshAll() {
  loadStatus()
  loadUsers()
}

onMounted(() => {
  loadStatus()
  loadUsers()
})

onUnmounted(() => {
  // Cancel every in-flight Risk V2 request and clear timers.
  statusAbort?.abort()
  listAbort?.abort()
  detailAbort?.abort()
  windowsAbort?.abort()
  clearCooldown()
})

// Minimal test seam: the list controls disable themselves while a request is in
// flight, so a UI-driven concurrent list load cannot happen — the race protection
// (abort + sequence guard) is exercised by invoking loadUsers directly.
defineExpose({ loadUsers, openDetail })
</script>

<style scoped>
.th-l {
  @apply px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400;
}
.th-c {
  @apply px-4 py-3 text-center text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400;
}
.th-r {
  @apply px-4 py-3 text-right text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400;
}
</style>
