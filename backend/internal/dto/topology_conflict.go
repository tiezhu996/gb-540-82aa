package dto

type ConflictQuery struct {
	ProposalID *uint
	State      string
	Type       string
	Page       int
	PageSize   int
}

type DetectConflictRequest struct {
	ProposalID     uint    `json:"proposal_id" validate:"required,gt=0"`
	SnapToleranceM float64 `json:"snap_tolerance_m" validate:"omitempty,gt=0,lte=1000"`
}

type ConflictTransitionRequest struct {
	To string `json:"to" validate:"required"`
}

// BatchConfirmConflictsRequest lets a reviewer confirm several still-detected
// conflicts of a single proposal in one operation. Blank/zero entries are
// dropped and duplicates collapsed by the service before validation.
type BatchConfirmConflictsRequest struct {
	ProposalID  uint   `json:"proposal_id" validate:"required,gt=0"`
	ConflictIDs []uint `json:"conflict_ids" validate:"required,min=1,max=500"`
}

// BatchConfirmConflictsResult reports what a successful bulk confirmation did.
type BatchConfirmConflictsResult struct {
	ProposalID  uint   `json:"proposal_id"`
	Count       int    `json:"count"`
	ConflictIDs []uint `json:"conflict_ids"`
}

type ApplySuggestionRequest struct {
	Rationale string `json:"rationale" validate:"max=2000"`
}
