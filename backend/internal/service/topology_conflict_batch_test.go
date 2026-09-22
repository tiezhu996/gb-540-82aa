package service

import (
	"errors"
	"testing"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/model"
	"cadastral-boundary-topology-resolution/backend/internal/repository"
)

func detectedBatchFixtures(t *testing.T, svc *CadastralService) (model.LandParcel, model.BoundaryProposal, model.BoundaryProposal, []model.TopologyConflict) {
	t.Helper()
	surveyor := testActor(801, constants.RoleSurveyor, "batch-parcel")
	base := createTestParcel(t, svc, "P-BATCH-BASE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), surveyor)
	createTestParcel(t, svc, "P-BATCH-NEIGHBOUR", serviceTestPolygon(`[10,0],[20,0],[20,10],[10,10],[10,0]`), testActor(802, constants.RoleSurveyor, "batch-neighbour"))
	firstProposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[11,0],[11,10],[0,10],[0,0]`), testActor(801, constants.RoleSurveyor, "batch-proposal-1"))
	first, err := svc.DetectConflicts(dto.DetectConflictRequest{ProposalID: firstProposal.ID, SnapToleranceM: 0.1}, "batch-detect-1", testActor(803, constants.RoleGISAnalyst, "batch-detect-1"))
	if err != nil {
		t.Fatalf("detect conflicts for first proposal: %v", err)
	}
	secondProposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[12,0],[12,10],[0,10],[0,0]`), testActor(801, constants.RoleSurveyor, "batch-proposal-2"))
	second, err := svc.DetectConflicts(dto.DetectConflictRequest{ProposalID: secondProposal.ID, SnapToleranceM: 0.1}, "batch-detect-2", testActor(803, constants.RoleGISAnalyst, "batch-detect-2"))
	if err != nil {
		t.Fatalf("detect conflicts for second proposal: %v", err)
	}
	return base, firstProposal, secondProposal, append(first, second...)
}

func TestBatchConfirmConflictsSucceedsAtomicallyAndAuditsEach(t *testing.T) {
	svc, store := newCadastralTestService(t)
	_, firstProposal, _, all := detectedBatchFixtures(t, svc)
	var firstIDs []uint
	for _, item := range all {
		if item.ProposalID == firstProposal.ID {
			firstIDs = append(firstIDs, item.ID)
		}
	}
	if len(firstIDs) < 1 {
		t.Fatalf("expected at least one conflict for proposal %d, got none", firstProposal.ID)
	}
	reviewer := testActor(804, constants.RoleReviewer, "batch-confirm-ok")
	// Blank ids and duplicates must be tolerated and collapsed.
	result, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: firstProposal.ID, ConflictIDs: append([]uint{0, 0}, append(firstIDs, firstIDs...)...)}, reviewer)
	if err != nil {
		t.Fatalf("BatchConfirmConflicts() error = %v", err)
	}
	if result.ProposalID != firstProposal.ID || result.Count != len(firstIDs) || len(result.ConflictIDs) != len(firstIDs) {
		t.Fatalf("batch result = %#v, want %d ids of proposal %d", result, len(firstIDs), firstProposal.ID)
	}
	for _, id := range firstIDs {
		reloaded, getErr := svc.GetConflict(id)
		if getErr != nil {
			t.Fatalf("reload conflict %d: %v", id, getErr)
		}
		if reloaded.ConflictState != constants.ConflictConfirmed {
			t.Fatalf("conflict %d state = %q, want confirmed", id, reloaded.ConflictState)
		}
	}
	var auditCount int64
	if err := store.DB.Model(&model.AuditLog{}).
		Where("resource_type = ? AND action = ? AND request_id = ?", "TopologyConflict", "conflict.state_changed", reviewer.RequestID).
		Count(&auditCount).Error; err != nil {
		t.Fatalf("count batch audits: %v", err)
	}
	if auditCount != int64(len(firstIDs)) {
		t.Fatalf("batch audit count = %d, want %d (one per conflict)", auditCount, len(firstIDs))
	}
}

func TestBatchConfirmConflictsRejectsUnknownCrossProposalAndChangedState(t *testing.T) {
	t.Run("unknown id rejects without changes", func(t *testing.T) {
		svc, store := newCadastralTestService(t)
		_, firstProposal, _, all := detectedBatchFixtures(t, svc)
		ids := []uint{all[0].ID, 999999}
		_, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: firstProposal.ID, ConflictIDs: ids}, testActor(810, constants.RoleReviewer, "batch-missing"))
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Code != CodeNotFound || appErr.Status != 404 {
			t.Fatalf("unknown id error = %v, want 404 %s", err, CodeNotFound)
		}
		if changed := countConflictsInState(store, constants.ConflictConfirmed); changed != 0 {
			t.Fatalf("confirmed conflicts after rejection = %d, want 0", changed)
		}
	})

	t.Run("cross proposal rejects without changes", func(t *testing.T) {
		svc, store := newCadastralTestService(t)
		_, firstProposal, secondProposal, all := detectedBatchFixtures(t, svc)
		other := func() uint {
			for _, item := range all {
				if item.ProposalID == secondProposal.ID {
					return item.ID
				}
			}
			return 0
		}()
		ids := []uint{all[0].ID, other}
		_, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: firstProposal.ID, ConflictIDs: ids}, testActor(811, constants.RoleReviewer, "batch-cross"))
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
			t.Fatalf("cross proposal error = %v, want 409 %s", err, CodeConflict)
		}
		if changed := countConflictsInState(store, constants.ConflictConfirmed); changed != 0 {
			t.Fatalf("confirmed conflicts after rejection = %d, want 0", changed)
		}
	})

	t.Run("state change rejects the whole batch", func(t *testing.T) {
		svc, store := newCadastralTestService(t)
		_, firstProposal, _, all := detectedBatchFixtures(t, svc)
		var ids []uint
		for _, item := range all {
			if item.ProposalID == firstProposal.ID {
				ids = append(ids, item.ID)
			}
		}
		// A concurrent reviewer confirms one conflict first.
		if _, err := svc.TransitionConflict(ids[0], dto.ConflictTransitionRequest{To: constants.ConflictConfirmed}, testActor(812, constants.RoleReviewer, "batch-race-single")); err != nil {
			t.Fatalf("pre-confirm one conflict: %v", err)
		}
		_, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: firstProposal.ID, ConflictIDs: ids}, testActor(813, constants.RoleReviewer, "batch-race"))
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
			t.Fatalf("changed-state error = %v, want 409 %s", err, CodeConflict)
		}
		// Only the independently confirmed conflict may be confirmed.
		if changed := countConflictsInState(store, constants.ConflictConfirmed); changed != 1 {
			t.Fatalf("confirmed conflicts after rejection = %d, want only the pre-confirmed one", changed)
		}
	})

	t.Run("blank and duplicated ids collapse to an empty request", func(t *testing.T) {
		svc, _ := newCadastralTestService(t)
		_, firstProposal, _, _ := detectedBatchFixtures(t, svc)
		_, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: firstProposal.ID, ConflictIDs: []uint{0, 0}}, testActor(814, constants.RoleReviewer, "batch-empty"))
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Status != 400 {
			t.Fatalf("blank ids error = %v, want 400", err)
		}
	})

	t.Run("reviewer role is required", func(t *testing.T) {
		svc, _ := newCadastralTestService(t)
		_, firstProposal, _, all := detectedBatchFixtures(t, svc)
		_, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: firstProposal.ID, ConflictIDs: []uint{all[0].ID}}, testActor(815, constants.RoleSurveyor, "batch-forbidden"))
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Code != CodeForbidden || appErr.Status != 403 {
			t.Fatalf("surveyor batch confirm error = %v, want 403 %s", err, CodeForbidden)
		}
	})
}

func countConflictsInState(store *repository.Store, state string) int64 {
	var count int64
	if err := store.DB.Model(&model.TopologyConflict{}).Where("conflict_state = ?", state).Count(&count).Error; err != nil {
		return -1
	}
	return count
}
