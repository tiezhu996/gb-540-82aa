package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/model"
	"cadastral-boundary-topology-resolution/backend/internal/repository"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func serviceTestPolygon(points string) string {
	return `{"type":"Polygon","coordinates":[[` + points + `]]}`
}

func newCadastralTestService(t *testing.T) (*CadastralService, *repository.Store) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.AuditLog{}, &model.LandParcel{}, &model.SurveyObservation{}, &model.BoundaryProposal{}, &model.TopologyConflict{}, &model.TopologyDetectionRun{}); err != nil {
		t.Fatalf("migrate SQLite: %v", err)
	}
	store := repository.NewStore(db)
	return NewCadastralService(store), store
}

func testActor(id uint, role, requestID string) Actor {
	return Actor{ID: id, Username: fmt.Sprintf("user-%d", id), Role: role, RequestID: requestID}
}

func createTestParcel(t *testing.T, svc *CadastralService, code, boundary string, actor Actor) model.LandParcel {
	t.Helper()
	parcel, err := svc.CreateParcel(dto.CreateParcelRequest{
		ParcelCode: code, Name: code, BoundaryGeoJSON: boundary,
		CoordinateSystem: "EPSG:3857", OwnerOrg: "test survey office",
	}, actor)
	if err != nil {
		t.Fatalf("CreateParcel(%s) error = %v", code, err)
	}
	return parcel
}

func createTestProposal(t *testing.T, svc *CadastralService, parcel model.LandParcel, boundary string, actor Actor) model.BoundaryProposal {
	t.Helper()
	proposal, err := svc.CreateProposal(dto.CreateProposalRequest{
		ParcelID: parcel.ID, BaseVersion: parcel.BoundaryVersion, ProposedGeoJSON: boundary,
		SnapToleranceM: 0.1, Rationale: "survey evidence supports the adjusted boundary",
	}, actor)
	if err != nil {
		t.Fatalf("CreateProposal() error = %v", err)
	}
	return proposal
}

func TestDetectConflictsIsIdempotentAndRejectsRequestKeyReuse(t *testing.T) {
	svc, store := newCadastralTestService(t)
	surveyor := testActor(101, constants.RoleSurveyor, "parcel-create")
	base := createTestParcel(t, svc, "P-BASE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), surveyor)
	createTestParcel(t, svc, "P-NEIGHBOR", serviceTestPolygon(`[10,0],[20,0],[20,10],[10,10],[10,0]`), testActor(102, constants.RoleSurveyor, "neighbour-create"))
	proposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[11,0],[11,10],[0,10],[0,0]`), testActor(101, constants.RoleSurveyor, "proposal-create"))

	detectionActor := testActor(101, constants.RoleGISAnalyst, "detect-first")
	request := dto.DetectConflictRequest{ProposalID: proposal.ID, SnapToleranceM: 0.1}
	first, err := svc.DetectConflicts(request, "detection-idempotency-key", detectionActor)
	if err != nil {
		t.Fatalf("first DetectConflicts() error = %v", err)
	}
	if len(first) == 0 || first[0].ConflictType != constants.ConflictOverlap {
		t.Fatalf("first DetectConflicts() = %#v, want an overlap", first)
	}
	second, err := svc.DetectConflicts(request, "detection-idempotency-key", testActor(101, constants.RoleGISAnalyst, "detect-replay"))
	if err != nil {
		t.Fatalf("replayed DetectConflicts() error = %v", err)
	}
	if len(second) != len(first) || second[0].ID != first[0].ID {
		t.Fatalf("replayed result = %#v, first result = %#v", second, first)
	}
	var runCount int64
	if err := store.DB.Model(&model.TopologyDetectionRun{}).Count(&runCount).Error; err != nil {
		t.Fatalf("count detection runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("detection run count = %d, want 1", runCount)
	}
	_, err = svc.DetectConflicts(dto.DetectConflictRequest{ProposalID: proposal.ID, SnapToleranceM: 0.2}, "detection-idempotency-key", detectionActor)
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
		t.Fatalf("changed request with same key error = %v, want 409 %s", err, CodeConflict)
	}
}

func TestProposalReviewRequiresIndependentReviewer(t *testing.T) {
	svc, _ := newCadastralTestService(t)
	author := testActor(201, constants.RoleSurveyor, "author-create")
	parcel := createTestParcel(t, svc, "P-REVIEW", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), author)
	proposal := createTestProposal(t, svc, parcel, serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), author)

	validated, err := svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalValidated), Version: proposal.Version}, testActor(201, constants.RoleSurveyor, "author-validate"))
	if err != nil {
		t.Fatalf("validate proposal: %v", err)
	}
	_, err = svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalSubmitted), Version: validated.Version}, testActor(202, constants.RoleSurveyor, "other-author-submit"))
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Code != CodeForbidden || appErr.Status != 403 {
		t.Fatalf("non-author authoring transition error = %v, want 403 %s", err, CodeForbidden)
	}
	submitted, err := svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalSubmitted), Version: validated.Version}, testActor(201, constants.RoleSurveyor, "author-submit"))
	if err != nil {
		t.Fatalf("submit proposal: %v", err)
	}
	_, err = svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalReviewed), Version: submitted.Version}, testActor(201, constants.RoleReviewer, "self-review"))
	if !errors.As(err, &appErr) || appErr.Code != CodeForbidden || appErr.Status != 403 {
		t.Fatalf("author self-review error = %v, want 403 %s", err, CodeForbidden)
	}
	_, err = svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalReviewed), Version: submitted.Version}, testActor(201, constants.RoleAdmin, "admin-self-review"))
	if !errors.As(err, &appErr) || appErr.Code != CodeForbidden || appErr.Status != 403 {
		t.Fatalf("administrator self-review error = %v, want 403 %s", err, CodeForbidden)
	}
	_, err = svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalReviewed), Version: submitted.Version}, testActor(202, constants.RoleSurveyor, "unprivileged-review"))
	if !errors.As(err, &appErr) || appErr.Code != CodeForbidden || appErr.Status != 403 {
		t.Fatalf("non-reviewer review error = %v, want 403 %s", err, CodeForbidden)
	}
	reviewed, err := svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalReviewed), Version: submitted.Version}, testActor(203, constants.RoleReviewer, "independent-review"))
	if err != nil {
		t.Fatalf("independent reviewer transition: %v", err)
	}
	if reviewed.ProposalState != constants.ProposalReviewed || reviewed.ReviewedBy == nil || *reviewed.ReviewedBy != 203 {
		t.Fatalf("reviewed proposal = %#v, want reviewer 203", reviewed)
	}
	accepted, err := svc.TransitionProposal(proposal.ID, dto.ProposalTransitionRequest{To: string(constants.ProposalAccepted), Version: reviewed.Version}, testActor(203, constants.RoleReviewer, "independent-accept"))
	if err != nil {
		t.Fatalf("independent reviewer acceptance: %v", err)
	}
	if accepted.ProposalState != constants.ProposalAccepted || accepted.Version != reviewed.Version+1 {
		t.Fatalf("accepted proposal = %#v, want accepted state and incremented version", accepted)
	}
}

func TestCreateParcelRejectsSelfIntersectingGeometryWith422(t *testing.T) {
	svc, _ := newCadastralTestService(t)
	_, err := svc.CreateParcel(dto.CreateParcelRequest{
		ParcelCode:       "P-BOWTIE",
		Name:             "self intersecting fixture",
		BoundaryGeoJSON:  serviceTestPolygon(`[0,0],[10,10],[0,10],[10,0],[0,0]`),
		CoordinateSystem: "EPSG:3857",
	}, testActor(401, constants.RoleSurveyor, "invalid-geometry"))
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Code != CodeInvalidInput || appErr.Status != 422 {
		t.Fatalf("CreateParcel(self-intersecting) error = %v, want 422 %s", err, CodeInvalidInput)
	}
}

func TestSupersedingObservationRecordsReplacement(t *testing.T) {
	svc, _ := newCadastralTestService(t)
	actor := testActor(501, constants.RoleSurveyor, "observation-import")
	parcel := createTestParcel(t, svc, "P-OBS", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), actor)

	first, err := svc.ImportObservation(dto.ImportObservationRequest{
		ParcelID: parcel.ID, ObservationCode: "OBS-ORIGINAL", PointGeoJSON: `{"type":"Point","coordinates":[1,1]}`,
		ObservedAt: time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC), Method: "total_station", HorizontalAccuracyM: 0.02, SourceChecksum: "checksum-original",
	}, actor)
	if err != nil {
		t.Fatalf("import original observation: %v", err)
	}
	replacement, err := svc.ImportObservation(dto.ImportObservationRequest{
		ParcelID: parcel.ID, ObservationCode: "OBS-REPLACEMENT", PointGeoJSON: `{"type":"Point","coordinates":[1.02,1]}`,
		ObservedAt: time.Date(2026, 8, 22, 9, 1, 0, 0, time.UTC), Method: "total_station", HorizontalAccuracyM: 0.01, SourceChecksum: "checksum-replacement",
	}, actor)
	if err != nil {
		t.Fatalf("import replacement observation: %v", err)
	}

	superseded, err := svc.TransitionObservation(first.ID, dto.ObservationTransitionRequest{
		To: "superseded", Version: first.Version, ReplacementObservationID: &replacement.ID, QualityNote: "superseded by a more accurate repeat observation",
	}, testActor(501, constants.RoleSurveyor, "observation-supersede"))
	if err != nil {
		t.Fatalf("supersede observation: %v", err)
	}
	if superseded.ObservationState != "superseded" || superseded.ReplacedBy == nil || *superseded.ReplacedBy != replacement.ID {
		t.Fatalf("superseded observation = %#v, want replacement %d", superseded, replacement.ID)
	}
	persisted, err := svc.GetObservation(first.ID)
	if err != nil {
		t.Fatalf("reload superseded observation: %v", err)
	}
	if persisted.Version != first.Version+1 || persisted.ReplacedBy == nil || *persisted.ReplacedBy != replacement.ID {
		t.Fatalf("persisted observation = %#v, want incremented version and replacement", persisted)
	}
}

func createTestConflicts(t *testing.T, svc *CadastralService, store *repository.Store, proposal model.BoundaryProposal, count int, state string) []model.TopologyConflict {
	t.Helper()
	items := make([]model.TopologyConflict, 0, count)
	for index := 0; index < count; index++ {
		item := model.TopologyConflict{
			ProposalID: proposal.ID, ParcelIDs: "[]", ConflictType: constants.ConflictOverlap, MagnitudeSquareM: float64(index + 1),
			Severity: "medium", AlgorithmVersion: "batch-test", InputHash: fmt.Sprintf("batch-hash-%d", index), ConflictState: state,
			SuggestedResolutionJSON: "{}", Explanation: "batch review fixture", DetectedAt: time.Now().UTC(),
		}
		if err := store.Conflicts.Create(&item); err != nil {
			t.Fatalf("create test conflict %d: %v", index, err)
		}
		items = append(items, item)
	}
	return items
}

func TestBatchConfirmConflictsConfirmsAllAndAuditsEach(t *testing.T) {
	svc, store := newCadastralTestService(t)
	base := createTestParcel(t, svc, "P-BATCH-BASE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), testActor(602, constants.RoleSurveyor, "batch-base-create"))
	proposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[11,0],[11,10],[0,10],[0,0]`), testActor(602, constants.RoleSurveyor, "batch-proposal-create"))
	found := createTestConflicts(t, svc, store, proposal, 3, constants.ConflictDetected)
	reviewer := testActor(610, constants.RoleReviewer, "batch-confirm-ok")
	// IDs intentionally unsorted, duplicated and padded with zero entries.
	requestIDs := []uint{found[2].ID, 0, found[0].ID, found[1].ID, found[2].ID, 0}
	result, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: proposal.ID, ConflictIDs: requestIDs}, reviewer)
	if err != nil {
		t.Fatalf("BatchConfirmConflicts() error = %v", err)
	}
	if result.ProposalID != proposal.ID || result.ConfirmedCount != 3 {
		t.Fatalf("batch result = %#v, want proposal %d and 3 confirmations", result, proposal.ID)
	}
	if result.ConflictIDs[0] >= result.ConflictIDs[1] || result.ConflictIDs[1] >= result.ConflictIDs[2] {
		t.Fatalf("returned conflict ids = %v, want sorted unique ids", result.ConflictIDs)
	}
	for _, item := range found {
		reloaded, getErr := svc.GetConflict(item.ID)
		if getErr != nil {
			t.Fatalf("reload conflict %d: %v", item.ID, getErr)
		}
		if reloaded.ConflictState != constants.ConflictConfirmed {
			t.Fatalf("conflict %d state = %s, want confirmed", item.ID, reloaded.ConflictState)
		}
	}
	var auditCount int64
	if err := store.DB.Model(&model.AuditLog{}).Where("action = ? AND request_id = ?", "conflict.batch_confirmed", reviewer.RequestID).Count(&auditCount).Error; err != nil {
		t.Fatalf("count batch audits: %v", err)
	}
	if auditCount != int64(len(result.ConflictIDs)) {
		t.Fatalf("batch audit count = %d, want one per confirmed conflict (%d)", auditCount, len(result.ConflictIDs))
	}
	var parcelUpdates int64
	if err := store.DB.Model(&model.LandParcel{}).Where("boundary_version <> ?", 1).Count(&parcelUpdates).Error; err != nil {
		t.Fatalf("count parcel versions: %v", err)
	}
	if parcelUpdates != 0 {
		t.Fatalf("batch review changed %d parcel versions; evidence boundaries must stay untouched", parcelUpdates)
	}
}

func TestBatchConfirmConflictsRejectsMissingCrossProposalAndChangedState(t *testing.T) {
	svc, store := newCadastralTestService(t)
	base := createTestParcel(t, svc, "P-BATCH-BASE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), testActor(602, constants.RoleSurveyor, "batch-base-create-2"))
	proposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[11,0],[11,10],[0,10],[0,0]`), testActor(602, constants.RoleSurveyor, "batch-proposal-create-2"))
	found := createTestConflicts(t, svc, store, proposal, 2, constants.ConflictDetected)
	reviewer := testActor(620, constants.RoleReviewer, "batch-confirm-reject")

	// A nonexistent conflict number rejects the whole batch without writes.
	missingIDs := []uint{found[0].ID, uint(1_000_000)}
	_, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: proposal.ID, ConflictIDs: missingIDs}, reviewer)
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Code != CodeNotFound || appErr.Status != 404 {
		t.Fatalf("missing id error = %v, want 404 %s", err, CodeNotFound)
	}

	// A conflict from another proposal cannot be mixed into the batch.
	otherBase := createTestParcel(t, svc, "P-BATCH-OTHER", serviceTestPolygon(`[30,0],[40,0],[40,10],[30,10],[30,0]`), testActor(621, constants.RoleSurveyor, "batch-other-base"))
	otherProposal := createTestProposal(t, svc, otherBase, serviceTestPolygon(`[30,0],[40,0],[40,10],[30,10],[30,0]`), testActor(621, constants.RoleSurveyor, "batch-other-proposal"))
	otherConflict := createTestConflicts(t, svc, store, otherProposal, 1, constants.ConflictDetected)[0]
	_, err = svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: proposal.ID, ConflictIDs: []uint{found[0].ID, otherConflict.ID}}, reviewer)
	if !errors.As(err, &appErr) || appErr.Code != CodeInvalidInput || appErr.Status != 400 {
		t.Fatalf("cross proposal error = %v, want 400 %s", err, CodeInvalidInput)
	}

	// A state change between selection and submission rejects the batch.
	if _, err := svc.TransitionConflict(found[1].ID, dto.ConflictTransitionRequest{To: constants.ConflictFalsePositive}, reviewer); err != nil {
		t.Fatalf("mark conflict false positive: %v", err)
	}
	_, err = svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: proposal.ID, ConflictIDs: []uint{found[0].ID, found[1].ID}}, reviewer)
	if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
		t.Fatalf("changed state error = %v, want 409 %s", err, CodeConflict)
	}

	// None of the rejected batches may have moved still-detected conflicts.
	reloaded, err := svc.GetConflict(found[0].ID)
	if err != nil {
		t.Fatalf("reload untouched conflict: %v", err)
	}
	if reloaded.ConflictState != constants.ConflictDetected {
		t.Fatalf("conflict %d state = %s after rejected batches, want detected unchanged", found[0].ID, reloaded.ConflictState)
	}
	var auditCount int64
	if err := store.DB.Model(&model.AuditLog{}).Where("action = ?", "conflict.batch_confirmed").Count(&auditCount).Error; err != nil {
		t.Fatalf("count batch audits after rejections: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("rejected batches wrote %d batch audits, want none", auditCount)
	}
}

func TestBatchConfirmConflictsRequiresReviewerRole(t *testing.T) {
	svc, store := newCadastralTestService(t)
	base := createTestParcel(t, svc, "P-BATCH-ROLE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), testActor(631, constants.RoleSurveyor, "batch-role-parcel"))
	proposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[11,0],[11,10],[0,10],[0,0]`), testActor(631, constants.RoleSurveyor, "batch-role-proposal"))
	found := createTestConflicts(t, svc, store, proposal, 1, constants.ConflictDetected)
	_, err := svc.BatchConfirmConflicts(dto.BatchConfirmConflictsRequest{ProposalID: proposal.ID, ConflictIDs: []uint{found[0].ID}}, testActor(630, constants.RoleGISAnalyst, "batch-confirm-forbidden"))
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Code != CodeForbidden || appErr.Status != 403 {
		t.Fatalf("analyst batch confirm error = %v, want 403 %s", err, CodeForbidden)
	}
}
