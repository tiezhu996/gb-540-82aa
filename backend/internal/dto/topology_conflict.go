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

// BatchConfirmConflictsRequest confirms a reviewer-selected set of detected
// conflicts. IDs may contain blanks or duplicates; the service normalizes
// them. Empty elements and repeated numbers are ignored, but every remaining
// ID must exist, belong to ProposalID, and still be in the detected state.
type BatchConfirmConflictsRequest struct {
	ProposalID  uint   `json:"proposal_id" validate:"required,gt=0"`
	ConflictIDs []uint `json:"conflict_ids" validate:"required,min=1,max=500"`
}

// BatchConfirmConflictsResult reports the proposal and number of conflicts
// that moved to confirmed inside the single batch transaction.
type BatchConfirmConflictsResult struct {
	ProposalID     uint   `json:"proposal_id"`
	ConfirmedCount int    `json:"confirmed_count"`
	ConflictIDs    []uint `json:"conflict_ids"`
}

type ApplySuggestionRequest struct {
	Rationale string `json:"rationale" validate:"max=2000"`
}
