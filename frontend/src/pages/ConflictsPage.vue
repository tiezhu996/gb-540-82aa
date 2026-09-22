<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { TableInstance } from 'element-plus'
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
import type { BatchConfirmConflictsResult } from '@/api/topology-conflict'

const conflicts = useTopologyConflictStore()
const proposals = useBoundaryProposalStore()
const parcels = useLandParcelStore()
const auth = useAuth()
const detectOpen = ref(false)
const evidenceOpen = ref(false)
const selected = ref<TopologyConflict | null>(null)
const conflictsTableRef = ref<TableInstance>()
const checkedRows = ref<TopologyConflict[]>([])
const batchSummary = ref<BatchConfirmConflictsResult | null>(null)
const batchError = ref('')
const form = reactive({ proposal_id: 0 })

const isReviewer = computed(() => auth.hasRole('reviewer', 'admin'))
const checkedProposalID = computed(() => checkedRows.value[0]?.proposal_id ?? null)
const checkedCount = computed(() => checkedRows.value.length)

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

// Element Plus consults this before allowing a tick. Reviewers may only batch
// detected conflicts, and once the first conflict is ticked every further
// tick must belong to the same proposal, so a cross-proposal batch can never
// be assembled in the UI.
function isRowSelectable(row: TopologyConflict): boolean {
  if (!isReviewer.value || row.conflict_state !== 'detected') return false
  return checkedProposalID.value === null || row.proposal_id === checkedProposalID.value
}

function onSelectionChange(rows: TopologyConflict[]) {
  checkedRows.value = rows
  batchError.value = ''
}

async function load() {
  // Server state is authoritative: selections and a stale failure banner do
  // not survive a refresh, while the confirmed rows read back as confirmed.
  conflictsTableRef.value?.clearSelection()
  checkedRows.value = []
  batchError.value = ''
  await Promise.all([
    parcels.fetch({ page_size: 100 }),
    proposals.fetch({ page_size: 100 }),
    conflicts.fetch({ page_size: 100 }),
  ])
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

async function batchConfirm() {
  const proposalID = checkedProposalID.value
  if (proposalID === null) {
    ElMessage.warning('请先勾选同一提案下仍为 detected 的冲突')
    return
  }
  const ids = checkedRows.value.map((row) => row.id)
  try {
    await ElMessageBox.confirm(
      `将把提案 #${proposalID} 下勾选的 ${ids.length} 条冲突一次性复核为 confirmed。该操作在单个事务内完成，原始证据和地块边界不会被修改。`,
      '批量复核',
      { confirmButtonText: '一次确认', cancelButtonText: '取消', type: 'warning' },
    )
  } catch (reason) {
    if (reason === 'cancel' || reason === 'close') return
    throw reason
  }
  try {
    batchError.value = ''
    batchSummary.value = null
    const result = await conflicts.batchConfirm({ proposal_id: proposalID, conflict_ids: ids })
    batchSummary.value = result
    ElMessage.success(`提案 #${result.proposal_id} 的 ${result.confirmed_count} 条冲突已确认`)
    await load()
  } catch (reason: unknown) {
    const reasonAny = reason as { response?: { data?: { error?: { message?: string } } } }
    batchError.value = reasonAny.response?.data?.error?.message ?? '批量复核被拒绝，未修改任何冲突'
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
    <div class="toolbar"><el-button @click="load"><RefreshCw :size="15" />刷新</el-button><TopologyLegend /><span class="toolbar-spacer subtle-count">{{ conflicts.items.length }} 条冲突</span></div>

    <div v-if="isReviewer" class="batch-bar">
      <div class="batch-bar-copy">
        <strong><CheckCheck :size="15" /> 批量复核</strong>
        <span v-if="checkedProposalID === null">勾选同一提案下仍为 detected 的冲突后一次确认；编号自动去空去重，跨提案行不可勾选。</span>
        <span v-else>已锁定提案 #{{ checkedProposalID }}，已选 {{ checkedCount }} 条 detected 冲突。</span>
      </div>
      <el-button type="primary" :loading="conflicts.batchConfirming" :disabled="!checkedCount" @click="batchConfirm"><CheckCheck :size="15" />批量确认{{ checkedCount ? `（${checkedCount}）` : '' }}</el-button>
    </div>

    <el-alert
      v-if="batchSummary"
      class="batch-result"
      type="success"
      show-icon
      closable
      title="批量复核成功"
      @close="batchSummary = null"
    >
      <template #default>
        提案 #{{ batchSummary.proposal_id }} 下 {{ batchSummary.confirmed_count }} 条冲突已在单个事务内进入 confirmed 状态，并逐条写入审计：#{{ batchSummary.conflict_ids.join('、#') }}。原始证据和地块边界未改动。
      </template>
    </el-alert>
    <el-alert
      v-if="batchError"
      class="batch-result"
      type="error"
      show-icon
      closable
      title="批量复核失败，未修改任何冲突"
      @close="batchError = ''"
    >
      <template #default>{{ batchError }}</template>
    </el-alert>

    <div class="data-surface">
      <el-table
        ref="conflictsTableRef"
        v-loading="conflicts.loading"
        :data="conflicts.items"
        row-key="id"
        @selection-change="onSelectionChange"
      >
        <el-table-column v-if="isReviewer" type="selection" width="46" :selectable="isRowSelectable" />
        <el-table-column label="冲突" width="90"><template #default="scope"><strong>#{{ scope.row.id }}</strong></template></el-table-column>
        <el-table-column label="参与地块" min-width="180"><template #default="scope"><strong>{{ parcelLabelFor(scope.row) }}</strong><small class="muted">提案 #{{ scope.row.proposal_id }}</small><ProposalStateBadge :state="proposalStateFor(scope.row)" /></template></el-table-column>
        <el-table-column label="类型" width="125"><template #default="scope"><span :class="['conflict-tag', `tone-${scope.row.conflict_type}`]">{{ typeLabel(scope.row.conflict_type) }}</span></template></el-table-column>
        <el-table-column prop="severity" label="严重度" width="95" />
        <el-table-column label="量级" width="125"><template #default="scope">{{ scope.row.magnitude_square_m.toFixed(2) }} m²</template></el-table-column>
        <el-table-column prop="explanation" label="说明" min-width="220" show-overflow-tooltip />
        <el-table-column label="状态" width="145"><template #default="scope"><span class="status-pill" :class="scope.row.conflict_state">{{ scope.row.conflict_state }}</span></template></el-table-column>
        <el-table-column label="动作" width="270"><template #default="scope"><div class="conflict-actions"><el-button text @click="showEvidence(scope.row)">证据</el-button><template v-if="isReviewer"><el-button v-if="scope.row.conflict_state === 'detected'" text type="primary" @click="transition(scope.row, 'confirmed')"><Check :size="14" />确认</el-button><el-button v-if="scope.row.conflict_state === 'detected'" text type="warning" @click="transition(scope.row, 'false_positive')"><X :size="14" />误报</el-button><el-button v-if="scope.row.conflict_state === 'confirmed'" text type="primary" @click="transition(scope.row, 'resolution_proposed')"><FilePlus :size="14" />准备建议</el-button><el-button v-if="scope.row.conflict_state === 'resolution_proposed'" text type="primary" @click="applySuggestion(scope.row)"><FilePlus :size="14" />应用建议</el-button><el-button v-if="scope.row.conflict_state === 'false_positive' || scope.row.conflict_state === 'resolved'" text @click="transition(scope.row, 'closed')"><X :size="14" />关闭</el-button></template></div></template></el-table-column>
      </el-table>
      <div v-if="!conflicts.loading && !conflicts.items.length" class="empty-state"><div><strong>暂无冲突</strong><span>选择一个提案运行检测，系统会记录算法版本和输入哈希。</span></div></div>
    </div>
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
.batch-bar { display: flex; align-items: center; justify-content: space-between; gap: 14px; flex-wrap: wrap; margin: 10px 0; padding: 10px 14px; border: 1px solid var(--line-strong); background: var(--surface-raised, #fff); }
.batch-bar-copy { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 13px; color: var(--text-muted); }
.batch-bar-copy strong { display: inline-flex; align-items: center; gap: 6px; color: var(--text-strong, #1f2933); }
.batch-result { margin: 10px 0; }
</style>
