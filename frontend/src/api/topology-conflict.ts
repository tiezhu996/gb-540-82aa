import { api } from './client'
import type { ApiEnvelope } from '@/types/api'
import { normalizeTopologyConflict, type TopologyConflictWire } from '@/types/topology-conflict'
import type { BoundaryProposal } from '@/types/boundary-proposal'

export interface ConflictQuery {
  proposal_id?: number
  state?: string
  conflict_type?: string
  page?: number
  page_size?: number
}

export interface ConflictDetectInput {
  proposal_id: number
  snap_tolerance_m?: number
}

export interface ConflictTransitionInput {
  to: string
}

export interface ApplySuggestionInput {
  rationale?: string
}

export interface BatchConfirmInput {
  proposal_id: number
  conflict_ids: number[]
}

export interface BatchConfirmResult {
  proposal_id: number
  count: number
  conflict_ids: number[]
}

function newIdempotencyKey() {
  return crypto.randomUUID()
}

export const topologyConflictApi = {
  async list(params?: ConflictQuery) {
    const response = await api.get<ApiEnvelope<TopologyConflictWire[]>>('/conflicts', { params })
    return { ...response, data: { ...response.data, data: response.data.data.map(normalizeTopologyConflict) } }
  },
  async detail(id: number) {
    const response = await api.get<ApiEnvelope<TopologyConflictWire>>(`/conflicts/${id}`)
    return { ...response, data: { ...response.data, data: normalizeTopologyConflict(response.data.data) } }
  },
  async detect(body: ConflictDetectInput, idempotencyKey: string = newIdempotencyKey()) {
    const response = await api.post<ApiEnvelope<TopologyConflictWire[]>>('/conflicts/detect', body, {
      headers: { 'Idempotency-Key': idempotencyKey },
    })
    return { ...response, data: { ...response.data, data: response.data.data.map(normalizeTopologyConflict) } }
  },
  async transition(id: number, body: ConflictTransitionInput) {
    const response = await api.post<ApiEnvelope<TopologyConflictWire>>(`/conflicts/${id}/transition`, body)
    return { ...response, data: { ...response.data, data: normalizeTopologyConflict(response.data.data) } }
  },
  async batchConfirm(body: BatchConfirmInput) {
    const response = await api.post<ApiEnvelope<BatchConfirmResult>>('/conflicts/batch-confirm', body)
    return response
  },
  applySuggestion: (id: number, body: ApplySuggestionInput = {}) =>
    api.post<ApiEnvelope<BoundaryProposal>>(`/conflicts/${id}/apply-suggestion`, body),
}
