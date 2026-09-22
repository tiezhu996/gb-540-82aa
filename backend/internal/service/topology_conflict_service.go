package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/geometry"
	"cadastral-boundary-topology-resolution/backend/internal/model"
	"cadastral-boundary-topology-resolution/backend/internal/repository"
)

func (s *CadastralService) DetectConflicts(req dto.DetectConflictRequest, idempotencyKey string, actor Actor) ([]model.TopologyConflict, error) {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || len(key) > 128 {
		return nil, invalid("Idempotency-Key must contain between 1 and 128 characters", nil)
	}
	proposal, err := s.store.Proposals.Get(req.ProposalID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, notFound("proposal")
	}
	if err != nil {
		return nil, internal("load proposal failed", err)
	}
	tolerance := req.SnapToleranceM
	if tolerance == 0 {
		tolerance = proposal.SnapToleranceM
	}
	requestHash := geometry.Hash(strconv.FormatUint(uint64(req.ProposalID), 10), fmt.Sprintf("%.6f", tolerance))
	if existing, findErr := s.store.DetectionRuns.GetByActorKey(actor.ID, key); findErr == nil {
		return s.replayDetectionRun(existing, requestHash)
	} else if !errors.Is(findErr, repository.ErrNotFound) {
		return nil, internal("check conflict idempotency failed", findErr)
	}

	parcel, err := s.store.Parcels.Get(proposal.ParcelID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, notFound("parcel")
	}
	if err != nil {
		return nil, internal("load parcel failed", err)
	}
	base, baseErr := geometry.ParsePolygon(parcel.BoundaryGeoJSON)
	if baseErr != nil {
		return nil, geoInvalid(baseErr)
	}
	proposed, proposedErr := geometry.ParsePolygon(proposal.ProposedGeoJSON)
	if proposedErr != nil {
		return nil, geoInvalid(proposedErr)
	}
	neighbourModels, neighbourErr := s.store.Parcels.ListActiveByCoordinateSystem(parcel.CoordinateSystem, parcel.ID)
	if neighbourErr != nil {
		return nil, internal("load neighbouring parcels failed", neighbourErr)
	}
	neighbours := make([]geometry.ParcelReference, 0, len(neighbourModels))
	references := []geometry.Polygon{base}
	hashParts := []string{parcel.BoundaryGeoJSON, proposal.ProposedGeoJSON, parcel.CoordinateSystem, fmt.Sprintf("%.6f", tolerance), geometry.AlgorithmVersion}
	for _, neighbour := range neighbourModels {
		polygon, parseErr := geometry.ParsePolygon(neighbour.BoundaryGeoJSON)
		if parseErr != nil {
			return nil, internal(fmt.Sprintf("stored geometry for neighbouring parcel %d is invalid", neighbour.ID), parseErr)
		}
		neighbours = append(neighbours, geometry.ParcelReference{ID: neighbour.ID, Polygon: polygon})
		references = append(references, polygon)
		hashParts = append(hashParts, strconv.FormatUint(uint64(neighbour.ID), 10), neighbour.BoundaryGeoJSON)
	}
	sort.Slice(neighbours, func(i, j int) bool { return neighbours[i].ID < neighbours[j].ID })
	snapped, snapErr := geometry.SnapToReferences(proposed, references, tolerance)
	if snapErr != nil {
		return nil, geoInvalid(snapErr)
	}
	findings, detectErr := geometry.DetectTopology(base, snapped.Polygon, neighbours, tolerance)
	if detectErr != nil {
		return nil, internal("detect topology conflicts failed", detectErr)
	}
	inputHash := geometry.Hash(hashParts...)
	suggestedJSON, marshalErr := json.Marshal(map[string]any{
		"action": "review_snapped_boundary", "snapped_geojson": json.RawMessage(snapped.SuggestedGeoJSON), "snap_changes": snapped.Changes,
		"tolerance_m": tolerance, "coordinate_system": parcel.CoordinateSystem, "algorithm_version": geometry.AlgorithmVersion, "topology_input_hash": inputHash,
	})
	if marshalErr != nil {
		return nil, internal("encode topology suggestion failed", marshalErr)
	}
	items := make([]model.TopologyConflict, 0, len(findings))
	now := time.Now().UTC()
	for _, finding := range findings {
		participantIDs := uniqueSortedIDs(append([]uint{parcel.ID}, finding.ParcelIDs...))
		parcelIDs, encodeErr := json.Marshal(participantIDs)
		if encodeErr != nil {
			return nil, internal("encode topology participants failed", encodeErr)
		}
		items = append(items, model.TopologyConflict{
			ProposalID: proposal.ID, ParcelIDs: string(parcelIDs), ConflictType: constants.ConflictType(finding.ConflictType), GeometryGeoJSON: finding.Geometry,
			MagnitudeSquareM: finding.Magnitude, Severity: conflictSeverity(finding.ConflictType, finding.Magnitude, tolerance), AlgorithmVersion: geometry.AlgorithmVersion,
			InputHash: inputHash, ConflictState: constants.ConflictDetected, SuggestedResolutionJSON: string(suggestedJSON), Explanation: finding.Explanation, DetectedAt: now,
		})
	}
	err = s.store.Transaction(func(tx *repository.Store) error {
		for index := range items {
			if createErr := tx.Conflicts.Create(&items[index]); createErr != nil {
				return createErr
			}
			if auditErr := tx.Audits.Create(audit(actor, "conflict.detected", "TopologyConflict", items[index].ID, &parcel.ID, "{}", snapshot(items[index]))); auditErr != nil {
				return auditErr
			}
		}
		resultIDs := make([]uint, 0, len(items))
		for _, item := range items {
			resultIDs = append(resultIDs, item.ID)
		}
		encodedIDs, encodeErr := json.Marshal(resultIDs)
		if encodeErr != nil {
			return encodeErr
		}
		run := model.TopologyDetectionRun{ProposalID: proposal.ID, ActorID: actor.ID, IdempotencyKey: key, RequestHash: requestHash, InputHash: inputHash, ResultIDs: string(encodedIDs)}
		if createErr := tx.DetectionRuns.Create(&run); createErr != nil {
			return createErr
		}
		return tx.Audits.Create(audit(actor, "conflict.detection_completed", "TopologyDetectionRun", run.ID, &proposal.ID, "{}", snapshot(run)))
	})
	if err != nil {
		if existing, findErr := s.store.DetectionRuns.GetByActorKey(actor.ID, key); findErr == nil {
			return s.replayDetectionRun(existing, requestHash)
		}
		return nil, wrapCadastral(err, "detect conflicts failed")
	}
	return items, nil
}

func (s *CadastralService) ListConflicts(q dto.ConflictQuery) ([]model.TopologyConflict, dto.Pagination, error) {
	normalizePage(&q.Page, &q.PageSize)
	items, total, err := s.store.Conflicts.List(q)
	if err != nil {
		return nil, dto.Pagination{}, internal("list conflicts failed", err)
	}
	return items, dto.Pagination{Page: q.Page, PageSize: q.PageSize, Total: total}, nil
}

func (s *CadastralService) GetConflict(id uint) (model.TopologyConflict, error) {
	item, err := s.store.Conflicts.Get(id)
	if errors.Is(err, repository.ErrNotFound) {
		return item, notFound("conflict")
	}
	if err != nil {
		return item, internal("get conflict failed", err)
	}
	return item, nil
}

func (s *CadastralService) TransitionConflict(id uint, req dto.ConflictTransitionRequest, actor Actor) (model.TopologyConflict, error) {
	item, err := s.store.Conflicts.Get(id)
	if errors.Is(err, repository.ErrNotFound) {
		return item, notFound("conflict")
	}
	if err != nil {
		return item, internal("get conflict failed", err)
	}
	if !constants.CanConflictTransition(item.ConflictState, req.To) {
		return item, conflict("conflict state transition is not allowed", nil)
	}
	var resolved *uint
	if req.To == constants.ConflictResolved || req.To == constants.ConflictClosed {
		resolved = &actor.ID
	}
	err = s.store.Transaction(func(tx *repository.Store) error {
		if transitionErr := tx.Conflicts.Transition(id, item.ConflictState, req.To, resolved); transitionErr != nil {
			return transitionErr
		}
		return tx.Audits.Create(audit(actor, "conflict.state_changed", "TopologyConflict", id, nil, snapshot(item), snapshot(map[string]any{"state": req.To})))
	})
	if err != nil {
		return item, conflict("conflict changed while transitioning", err)
	}
	item.ConflictState = req.To
	item.ResolvedBy = resolved
	return item, nil
}

// BatchConfirmConflicts moves a set of detected conflicts of one proposal to
// confirmed in a single transaction. Anything abnormal (unknown id, a conflict
// belonging to another proposal, or a state that already moved on) rejects the
// whole operation and leaves every conflict untouched. Original evidence
// geometries and parcel boundaries are never read or written here.
func (s *CadastralService) BatchConfirmConflicts(req dto.BatchConfirmConflictsRequest, actor Actor) (dto.BatchConfirmConflictsResult, error) {
	if err := requireAnyRole(actor, constants.RoleReviewer, constants.RoleAdmin); err != nil {
		return dto.BatchConfirmConflictsResult{}, err
	}
	ids := normalizeConflictIDs(req.ConflictIDs)
	if len(ids) == 0 {
		return dto.BatchConfirmConflictsResult{}, invalid("conflict_ids must contain at least one positive id", nil)
	}
	if _, err := s.store.Proposals.Get(req.ProposalID); errors.Is(err, repository.ErrNotFound) {
		return dto.BatchConfirmConflictsResult{}, notFound("proposal")
	} else if err != nil {
		return dto.BatchConfirmConflictsResult{}, internal("load proposal failed", err)
	}
	existing, err := s.store.Conflicts.FindExistingIDs(ids)
	if err != nil {
		return dto.BatchConfirmConflictsResult{}, internal("load conflicts failed", err)
	}
	if missing := missingConflictIDs(ids, existing); len(missing) > 0 {
		return dto.BatchConfirmConflictsResult{}, notFound(fmt.Sprintf("conflict %d", missing[0]))
	}
	items, err := s.store.Conflicts.ListByIDs(ids)
	if err != nil {
		return dto.BatchConfirmConflictsResult{}, internal("load conflicts failed", err)
	}
	for _, item := range items {
		if item.ProposalID != req.ProposalID {
			return dto.BatchConfirmConflictsResult{}, conflict(fmt.Sprintf("conflict %d does not belong to proposal %d", item.ID, req.ProposalID), nil)
		}
		if item.ConflictState != constants.ConflictDetected {
			return dto.BatchConfirmConflictsResult{}, conflict(fmt.Sprintf("conflict %d is no longer in detected state (current state: %s)", item.ID, item.ConflictState), nil)
		}
	}
	err = s.store.Transaction(func(tx *repository.Store) error {
		affected, transitionErr := tx.Conflicts.BatchConfirm(ids)
		if transitionErr != nil {
			return transitionErr
		}
		if affected != int64(len(ids)) {
			return conflict(fmt.Sprintf("expected %d conflicts to be confirmed, %d changed", len(ids), affected), nil)
		}
		for _, item := range items {
			before := item
			after := item
			after.ConflictState = constants.ConflictConfirmed
			if auditErr := tx.Audits.Create(audit(actor, "conflict.state_changed", "TopologyConflict", item.ID, &req.ProposalID, snapshot(before), snapshot(after))); auditErr != nil {
				return auditErr
			}
		}
		return nil
	})
	if err != nil {
		var appErr *AppError
		if errors.As(err, &appErr) {
			return dto.BatchConfirmConflictsResult{}, err
		}
		return dto.BatchConfirmConflictsResult{}, internal("batch confirm conflicts failed", err)
	}
	return dto.BatchConfirmConflictsResult{ProposalID: req.ProposalID, Count: len(ids), ConflictIDs: ids}, nil
}

func (s *CadastralService) ApplyConflictSuggestion(id uint, req dto.ApplySuggestionRequest, actor Actor) (model.BoundaryProposal, error) {
	if err := requireAnyRole(actor, constants.RoleReviewer, constants.RoleAdmin); err != nil {
		return model.BoundaryProposal{}, err
	}
	item, err := s.store.Conflicts.Get(id)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, notFound("conflict")
	}
	if err != nil {
		return model.BoundaryProposal{}, internal("get conflict failed", err)
	}
	if item.ConflictState != constants.ConflictResolutionProposed {
		return model.BoundaryProposal{}, conflict("a suggestion can only be applied from resolution_proposed", nil)
	}
	proposal, err := s.store.Proposals.Get(item.ProposalID)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, notFound("proposal")
	}
	if err != nil {
		return model.BoundaryProposal{}, internal("load proposal failed", err)
	}
	var suggestion struct {
		SnappedGeoJSON json.RawMessage `json:"snapped_geojson"`
	}
	if err := json.Unmarshal([]byte(item.SuggestedResolutionJSON), &suggestion); err != nil || len(suggestion.SnappedGeoJSON) == 0 {
		return model.BoundaryProposal{}, conflict("the conflict has no usable snapped-boundary suggestion", err)
	}
	polygon, parseErr := geometry.ParsePolygon(string(suggestion.SnappedGeoJSON))
	if parseErr != nil {
		return model.BoundaryProposal{}, internal("stored suggested geometry is invalid", parseErr)
	}
	parcel, parcelErr := s.store.Parcels.Get(proposal.ParcelID)
	if errors.Is(parcelErr, repository.ErrNotFound) {
		return model.BoundaryProposal{}, notFound("parcel")
	}
	if parcelErr != nil {
		return model.BoundaryProposal{}, internal("load parcel failed", parcelErr)
	}
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		rationale = fmt.Sprintf("Applied deterministic suggestion from topology conflict %d.", item.ID)
	}
	derived := model.BoundaryProposal{
		ParcelID: proposal.ParcelID, BaseVersion: parcel.BoundaryVersion, ProposedGeoJSON: string(suggestion.SnappedGeoJSON), ObservationIDs: proposal.ObservationIDs,
		SnapToleranceM: proposal.SnapToleranceM, AreaDeltaSquareM: polygon.Area - parcel.AreaSquareM, ProposalState: constants.ProposalDraft,
		Rationale: rationale, Version: proposal.Version + 1, CreatedBy: actor.ID,
	}
	resolvedBy := actor.ID
	err = s.store.Transaction(func(tx *repository.Store) error {
		if createErr := tx.Proposals.Create(&derived); createErr != nil {
			return createErr
		}
		if transitionErr := tx.Conflicts.Transition(item.ID, item.ConflictState, constants.ConflictResolved, &resolvedBy); transitionErr != nil {
			return transitionErr
		}
		if auditErr := tx.Audits.Create(audit(actor, "proposal.created_from_conflict", "BoundaryProposal", derived.ID, &derived.ParcelID, "{}", snapshot(derived))); auditErr != nil {
			return auditErr
		}
		return tx.Audits.Create(audit(actor, "conflict.suggestion_applied", "TopologyConflict", item.ID, &derived.ID, snapshot(item), snapshot(map[string]any{"conflict_state": constants.ConflictResolved, "proposal_id": derived.ID})))
	})
	if err != nil {
		return model.BoundaryProposal{}, wrapCadastral(err, "apply conflict suggestion failed")
	}
	return derived, nil
}

func (s *CadastralService) replayDetectionRun(run model.TopologyDetectionRun, requestHash string) ([]model.TopologyConflict, error) {
	if run.RequestHash != requestHash {
		return nil, conflict("Idempotency-Key has already been used with a different request", nil)
	}
	var resultIDs []uint
	if err := json.Unmarshal([]byte(run.ResultIDs), &resultIDs); err != nil {
		return nil, internal("stored idempotency result is invalid", err)
	}
	items, err := s.store.Conflicts.ListByIDs(resultIDs)
	if err != nil {
		return nil, internal("load idempotent detection result failed", err)
	}
	return items, nil
}

func canTransitionObservation(from, to string) bool {
	return (from == "accepted" && (to == "rejected" || to == "superseded")) || (from == "rejected" && to == "accepted")
}

func requireAnyRole(actor Actor, roles ...string) error {
	for _, role := range roles {
		if actor.Role == role {
			return nil
		}
	}
	return &AppError{CodeForbidden, http.StatusForbidden, "role is not permitted for this operation", nil}
}

func conflictSeverity(kind string, magnitude, tolerance float64) string {
	if kind == string(constants.ConflictOverlap) && magnitude > 100 {
		return "high"
	}
	if kind == string(constants.ConflictGap) && magnitude > tolerance*10 {
		return "high"
	}
	return "medium"
}

func uniqueSortedIDs(ids []uint) []uint {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	output := ids[:0]
	for _, id := range ids {
		if len(output) == 0 || output[len(output)-1] != id {
			output = append(output, id)
		}
	}
	return output
}

// normalizeConflictIDs drops blank (zero) entries and collapses duplicates so a
// careless client cannot inflate the batch or double-count one conflict.
func normalizeConflictIDs(ids []uint) []uint {
	seen := make(map[uint]struct{}, len(ids))
	normalized := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	return normalized
}

func missingConflictIDs(requested, existing []uint) []uint {
	present := make(map[uint]struct{}, len(existing))
	for _, id := range existing {
		present[id] = struct{}{}
	}
	missing := []uint{}
	for _, id := range requested {
		if _, ok := present[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

func geoInvalid(err error) error {
	return &AppError{CodeInvalidInput, http.StatusUnprocessableEntity, "geometry or coordinate system is invalid: " + err.Error(), err}
}

func wrapCadastral(err error, message string) error {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return err
	}
	return internal(message, err)
}
