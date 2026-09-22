<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type TableInstance } from 'element-plus'
import { Check, CheckCheck, FilePlus, RefreshCw, ScanSearch, X } from 'lucide-vue-next'
import PageHeader from '@/components/common/PageHeader.vue'
import TopologyLegend from '@/components/common/TopologyLegend.vue'
import GeometryEvidenceDrawer from '@/components/common/GeometryEvidenceDrawer.vue'
import ProposalStateBadge from '@/components/common/ProposalStateBadge.vue'
import { useTopologyConflictStore } from '@/stores/topology-conflict'
import { useBoundaryProposalStore } from '@/stores/boundary-proposal'
import { useLandParcelStore } from '@/stores/land-parcel'
import { useAuth } from '@/hooks/useAuth'
import { conflictTypeLabel } from '@/types/enums/conflict-type'
import type { ProposalState } from '@/types/enums/proposal-state'
import type { TopologyConflict } from '@/types/topology-conflict'

const conflicts = useTopologyConflictStore()
const proposals = useBoundaryProposalStore()
const parcels = useLandParcelStore()
const auth = useAuth()
const detectOpen = ref(false)
const evidenceOpen = ref(false)
const selected = ref<TopologyConflict | null>(null)
const form = reactive({ proposal_id: 0 })
const tableRef = ref<TableInstance>()
const checkedRows = ref<TopologyConflict[]>([])
const batchBusy = ref(false)
const batchResult = reactive<{ kind: 'success' | 'error'; proposalId: number | null; count: number; reason: string }>({
  kind: 'success',
  proposalId: null,
  count: 0,
  reason: '',
})

const canReview = computed(() => auth.hasRole('reviewer', 'admin'))
const checkedDetected = computed(() => checkedRows.value.filter((row) => row.conflict_state === 'detected'))
const selectedProposalId = computed(() => checkedRows.value[0]?.proposal_id ?? null)
const selectableRows = computed(() =>
  selectedProposalId.value === null
    ? []
    : conflicts.items.filter((row) => row.proposal_id === selectedProposalId.value && row.conflict_state === 'detected'),
)
const allDetectedSelected = computed(
  () => selectableRows.value.length > 0 && selectableRows.value.every((row) => checkedRows.value.includes(row)),
)

function typeLabel(value: unknown) {
  return conflictTypeLabel[value as keyof typeof conflictTypeLabel] ?? String(value)
}

function proposalStateFor(conflict: TopologyConflict): ProposalState | null {
  return proposals.items.find((proposal) => proposal.id === conflict.proposal_id)?.proposal_state ?? null
}

function parcelLabelFor(conflict: TopologyConflict) {
  const labels = conflict.parcel_ids.map((id) => {
    const parcel = parcels.items.find((item) => item.id === id)
    return parcel ? parcel.parcel_code : `地块 #${id}`
  })
  return labels.length ? labels.join(' · ') : '未记录参与地块'
}

// Only detected conflicts are selectable, and a single batch may never mix
// proposals. Element Plus fires both select and select-all through here so the
// checkbox UI is forced back into agreement whenever the rule is violated.
function isSelectable(row: TopologyConflict) {
  return row.conflict_state === 'detected'
}

async function reconcileSelection(rows: TopologyConflict[]) {
  const valid = rows.filter((row) => isSelectable(row))
  const anchor = valid[0]?.proposal_id ?? null
  const sameProposal = anchor === null ? [] : valid.filter((row) => row.proposal_id === anchor)
  const rejectedCross = valid.length - sameProposal.length
  const rejectedState = rows.length - valid.length
  if (rejectedCross > 0 || rejectedState > 0) {
    ElMessage.warning('一次批量复核只能选择同一提案下仍为 detected 的冲突，已忽略其余勾选。')
  }
  checkedRows.value = sameProposal
  await nextTick(() => {
    tableRef.value?.clearSelection()
    sameProposal.forEach((row) => tableRef.value?.toggleRowSelection(row, true))
  })
}

function onSelectionChange(rows: TopologyConflict[]) {
  void reconcileSelection(rows)
}

async function load() {
  checkedRows.value = []
  await Promise.all([
    parcels.fetch({ page_size: 100 }),
    proposals.fetch({ page_size: 100 }),
    conflicts.fetch({ page_size: 100 }),
  ])
  await nextTick(() => tableRef.value?.clearSelection())
}

async function detect() {
  await conflicts.detect({ proposal_id: form.proposal_id })
  detectOpen.value = false
  await load()
}

async function transition(item: TopologyConflict, to: string) {
  await conflicts.transition(item.id, { to })
  await load()
}

async function applySuggestion(item: TopologyConflict) {
  try {
    await ElMessageBox.confirm(
      `将基于冲突 #${item.id} 的吸附证据创建新的草稿提案；原始提案和地块边界不会被改写。`,
      '应用建议',
      { confirmButtonText: '创建草稿', cancelButtonText: '取消', type: 'warning' },
    )
  } catch (reason) {
    if (reason === 'cancel' || reason === 'close') return
    throw reason
  }
  await conflicts.applySuggestion(item.id)
  await load()
}

function failureReason(error: unknown): string {
  const detail = (error as { response?: { data?: { error?: { message?: string } } } })?.response?.data?.error?.message
  return detail ?? '请求未完成，请稍后重试。'
}

async function batchConfirm() {
  const targets = checkedDetected.value
  const proposalId = selectedProposalId.value
  if (!proposalId || targets.length === 0) return
  try {
    await ElMessageBox.confirm(
      `将一次性把提案 #${proposalId} 下勾选的 ${targets.length} 条 detected 冲突全部复核为 confirmed。该操作在一个事务内完成，原始证据和地块边界不会被改动。`,
      '批量复核确认',
      { confirmButtonText: '确认复核', cancelButtonText: '取消', type: 'info' },
    )
  } catch (reason) {
    if (reason === 'cancel' || reason === 'close') return
    throw reason
  }
  batchBusy.value = true
  try {
    const result = await conflicts.batchConfirm(
      proposalId,
      targets.map((row) => row.id),
    )
    batchResult.kind = 'success'
    batchResult.proposalId = result.proposal_id
    batchResult.count = result.count
    batchResult.reason = ''
    ElMessage.success(`提案 #${result.proposal_id} 的 ${result.count} 条冲突已复核为 confirmed。`)
  } catch (error) {
    batchResult.kind = 'error'
    batchResult.proposalId = proposalId
    batchResult.count = 0
    batchResult.reason = failureReason(error)
  } finally {
    batchBusy.value = false
    await load()
  }
}

function showEvidence(item: TopologyConflict) {
  selected.value = item
  evidenceOpen.value = true
}

onMounted(load)
</script>

<template>
  <PageHeader title="冲突消解" eyebrow="TOPOLOGY CONFLICTS" description="查看重叠、缝隙和无效拓扑证据，所有建议都需要人工确认。">
    <el-button v-if="auth.hasRole('gis_analyst', 'admin')" type="primary" @click="detectOpen = true"><ScanSearch :size="15" />运行检测</el-button>
  </PageHeader>

  <section class="content-band">
    <div class="toolbar">
      <el-button @click="load"><RefreshCw :size="15" />刷新</el-button>
      <TopologyLegend />
      <span class="toolbar-spacer subtle-count">{{ conflicts.items.length }} 条冲突</span>
      <el-button
        v-if="canReview"
        type="primary"
        :loading="batchBusy"
        :disabled="checkedDetected.length === 0"
        @click="batchConfirm"
      >
        <CheckCheck :size="15" />批量确认（{{ checkedDetected.length }}）
      </el-button>
    </div>

    <el-alert
      v-if="batchResult.kind === 'success' && batchResult.count > 0"
      class="batch-summary"
      type="success"
      show-icon
      :closable="true"
      @close="batchResult.count = 0"
      :title="`批量复核成功：提案 #${batchResult.proposalId} 下 ${batchResult.count} 条冲突已全部进入 confirmed。`"
    />
    <el-alert
      v-else-if="batchResult.kind === 'error'"
      class="batch-summary"
      type="error"
      show-icon
      :closable="true"
      @close="batchResult.reason = ''"
      title="批量复核被拒绝，所有冲突均未改动"
      :description="batchResult.proposalId !== null ? `提案 #${batchResult.proposalId}：${batchResult.reason}` : batchResult.reason"
    />

    <div class="data-surface">
      <el-table
        ref="tableRef"
        v-loading="conflicts.loading"
        :data="conflicts.items"
        row-key="id"
        @selection-change="onSelectionChange"
      >
        <el-table-column
          v-if="canReview"
          width="46"
          :selectable="isSelectable"
          type="selection"
        />
        <el-table-column label="冲突" width="90"><template #default="scope"><strong>#{{ scope.row.id }}</strong></template></el-table-column>
        <el-table-column label="参与地块" min-width="180"><template #default="scope"><strong>{{ parcelLabelFor(scope.row) }}</strong><small class="muted">提案 #{{ scope.row.proposal_id }}</small><ProposalStateBadge :state="proposalStateFor(scope.row)" /></template></el-table-column>
        <el-table-column label="类型" width="125"><template #default="scope"><span :class="['conflict-tag', `tone-${scope.row.conflict_type}`]">{{ typeLabel(scope.row.conflict_type) }}</span></template></el-table-column>
        <el-table-column prop="severity" label="严重度" width="95" />
        <el-table-column label="量级" width="125"><template #default="scope">{{ scope.row.magnitude_square_m.toFixed(2) }} m²</template></el-table-column>
        <el-table-column prop="explanation" label="说明" min-width="220" show-overflow-tooltip />
        <el-table-column label="状态" width="145"><template #default="scope"><span class="status-pill" :class="scope.row.conflict_state">{{ scope.row.conflict_state }}</span></template></el-table-column>
        <el-table-column label="动作" width="270"><template #default="scope"><div class="conflict-actions"><el-button text @click="showEvidence(scope.row)">证据</el-button><template v-if="canReview"><el-button v-if="scope.row.conflict_state === 'detected'" text type="primary" @click="transition(scope.row, 'confirmed')"><Check :size="14" />确认</el-button><el-button v-if="scope.row.conflict_state === 'detected'" text type="warning" @click="transition(scope.row, 'false_positive')"><X :size="14" />误报</el-button><el-button v-if="scope.row.conflict_state === 'confirmed'" text type="primary" @click="transition(scope.row, 'resolution_proposed')"><FilePlus :size="14" />准备建议</el-button><el-button v-if="scope.row.conflict_state === 'resolution_proposed'" text type="primary" @click="applySuggestion(scope.row)"><FilePlus :size="14" />应用建议</el-button><el-button v-if="scope.row.conflict_state === 'false_positive' || scope.row.conflict_state === 'resolved'" text @click="transition(scope.row, 'closed')"><X :size="14" />关闭</el-button></template></div></template></el-table-column>
      </el-table>
      <div v-if="!conflicts.loading && !conflicts.items.length" class="empty-state"><div><strong>暂无冲突</strong><span>选择一个提案运行检测，系统会记录算法版本和输入哈希。</span></div></div>
    </div>
    <p v-if="canReview" class="batch-hint">
      <template v-if="selectedProposalId === null">勾选同一提案下状态为 detected 的冲突后可一次确认。</template>
      <template v-else>当前批次限定提案 #{{ selectedProposalId }}，共 {{ selectableRows.length }} 条 detected 冲突可选{{ allDetectedSelected ? '（已全选）' : '' }}。</template>
    </p>
  </section>

  <el-dialog v-model="detectOpen" title="检测提案拓扑" width="min(480px, calc(100vw - 28px))">
    <el-form label-position="top"><el-form-item label="边界提案"><el-select v-model="form.proposal_id" placeholder="选择提案" style="width: 100%"><el-option v-for="item in proposals.items" :key="item.id" :label="`提案 #${item.id} · ${item.proposal_state}`" :value="item.id" /></el-select></el-form-item><el-alert type="warning" :closable="false" title="检测是离线决策支持，不会修改原始地块边界或法定登记。" /></el-form>
    <template #footer><el-button @click="detectOpen = false">取消</el-button><el-button type="primary" :disabled="!form.proposal_id" @click="detect">开始检测</el-button></template>
  </el-dialog>

  <GeometryEvidenceDrawer v-model="evidenceOpen" title="冲突证据" :geometry="selected?.geometry_geojson" :explanation="selected?.explanation" />
</template>

<style scoped>
.conflict-tag { display: inline-flex; padding: 3px 8px; border: 1px solid var(--line-strong); font-size: 12px; font-weight: 800; }
.tone-overlap { color: #9c3028; background: #fbeceb; border-color: #e6afaa; }.tone-gap { color: #755310; background: #fff5db; border-color: #e0c16b; }.tone-self_intersection { color: #7b4c9e; background: #f4ecfa; }.tone-dangling_edge { color: #2b6f96; background: #e8f2f7; }
.muted { display: block; margin-top: 4px; color: var(--text-muted); font-size: 11px; }
.conflict-actions { display: flex; flex-wrap: wrap; gap: 2px; }
.status-pill.confirmed, .status-pill.resolution_proposed { color: #755310; background: #fff5db; }.status-pill.resolved { color: #17604e; background: #e8f4f0; }.status-pill.false_positive, .status-pill.closed { color: #4b5551; background: #e8ecea; }
.batch-summary { margin-bottom: 12px; }
.batch-hint { margin: 10px 2px 0; color: var(--text-muted); font-size: 12px; }
</style>
