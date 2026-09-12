package dto

// CreatePlanRequest creates a named container for timetable versions.
type CreatePlanRequest struct {
	Name        string `json:"name" binding:"required,max=128"`
	Description string `json:"description" binding:"omitempty,max=512"`
	Operator    string `json:"operator" binding:"required,max=128"`
}

// PlanResponse describes a timetable plan and its currently active version.
type PlanResponse struct {
	ID               uint   `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	CurrentVersionID uint   `json:"current_version_id"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// CreateDraftRequest runs the scheduling algorithm and stores the result
// as a named draft version of a plan, without touching the live timetable.
type CreateDraftRequest struct {
	GenerateScheduleRequest
	Name     string `json:"name" binding:"required,max=128"`
	Operator string `json:"operator" binding:"required,max=128"`
	Remark   string `json:"remark" binding:"omitempty,max=512"`
}

// VersionResponse is one version with its snapshot lessons.
type VersionResponse struct {
	ID          uint           `json:"id"`
	PlanID      uint           `json:"plan_id"`
	VersionNo   int            `json:"version_no"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Semester    string         `json:"semester"`
	Weeks       int            `json:"weeks"`
	DaysPerWeek int            `json:"days_per_week"`
	LessonCount int            `json:"lesson_count"`
	CreatedBy   string         `json:"created_by"`
	CreatedAt   string         `json:"created_at"`
	PublishedBy string         `json:"published_by"`
	PublishedAt string         `json:"published_at"`
	Remark      string         `json:"remark"`
	Lessons     []LessonDetail `json:"lessons"`
}

// VersionSummary is the compact form used in version lists.
type VersionSummary struct {
	ID          uint   `json:"id"`
	PlanID      uint   `json:"plan_id"`
	VersionNo   int    `json:"version_no"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Semester    string `json:"semester"`
	Weeks       int    `json:"weeks"`
	DaysPerWeek int    `json:"days_per_week"`
	LessonCount int    `json:"lesson_count"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	PublishedBy string `json:"published_by"`
	PublishedAt string `json:"published_at"`
	Remark      string `json:"remark"`
}

// LessonDetail is a single lesson in a version snapshot, enriched with
// current master-data names when possible.
type LessonDetail struct {
	Week          uint   `json:"week"`
	DayOfWeek     int    `json:"day_of_week"`
	TimeSlotID    uint   `json:"time_slot_id"`
	TimeSlotCode  string `json:"time_slot_code"`
	ClassroomID   uint   `json:"classroom_id"`
	ClassroomName string `json:"classroom_name"`
	TeacherID     uint   `json:"teacher_id"`
	TeacherName   string `json:"teacher_name"`
	ClassID       uint   `json:"class_id"`
	ClassName     string `json:"class_name"`
	CourseID      uint   `json:"course_id"`
	CourseName    string `json:"course_name"`
}

// CompareVersionsRequest names the two versions of the same plan to diff.
type CompareVersionsRequest struct {
	FromVersionID uint   `json:"from_version_id" binding:"required"`
	ToVersionID   uint   `json:"to_version_id" binding:"required"`
	Operator      string `json:"operator" binding:"required,max=128"`
}

// VersionDiffItem describes one changed lesson between two versions.
type VersionDiffItem struct {
	Type          string        `json:"type"` // added | removed | modified
	Week          uint          `json:"week"`
	DayOfWeek     int           `json:"day_of_week"`
	TimeSlotID    uint          `json:"time_slot_id"`
	ClassID       uint          `json:"class_id"`
	CourseID      uint          `json:"course_id"`
	FromLesson    *LessonDetail `json:"from_lesson,omitempty"`
	ToLesson      *LessonDetail `json:"to_lesson,omitempty"`
	ChangedFields []string      `json:"changed_fields,omitempty"`
}

// VersionDiffResponse summarizes the differences between two versions.
type VersionDiffResponse struct {
	PlanID        uint              `json:"plan_id"`
	FromVersion   VersionSummary    `json:"from_version"`
	ToVersion     VersionSummary    `json:"to_version"`
	AddedCount    int               `json:"added_count"`
	RemovedCount  int               `json:"removed_count"`
	ModifiedCount int               `json:"modified_count"`
	Items         []VersionDiffItem `json:"items"`
}

// PublishVersionRequest publishes a draft version. Conflicts are re-checked
// against current master data before the release becomes live.
type PublishVersionRequest struct {
	Operator string `json:"operator" binding:"required,max=128"`
	Remark   string `json:"remark" binding:"omitempty,max=512"`
}

// PublishVersionResponse reports a successful release.
type PublishVersionResponse struct {
	Version     VersionSummary `json:"version"`
	Plan        PlanResponse   `json:"plan"`
	AppliedRows int64          `json:"applied_rows"`
}

// RollbackVersionRequest restores a previously published version.
type RollbackVersionRequest struct {
	Operator string `json:"operator" binding:"required,max=128"`
	Name     string `json:"name" binding:"omitempty,max=128"`
	Remark   string `json:"remark" binding:"omitempty,max=512"`
}

// RollbackVersionResponse reports a rollback: a new version carrying the
// restored snapshot is created and immediately published.
type RollbackVersionResponse struct {
	Plan            PlanResponse   `json:"plan"`
	SourceVersion   VersionSummary `json:"source_version"`
	RollbackVersion VersionSummary `json:"rollback_version"`
	AppliedRows     int64          `json:"applied_rows"`
}

// CreateDraftResponse is the result of draft generation.
type CreateDraftResponse struct {
	Plan    PlanResponse    `json:"plan"`
	Version VersionResponse `json:"version"`
	// PlacementConflicts are courses the greedy algorithm failed to place;
	// unlike post-generation conflicts they do not block storing the draft.
	PlacementConflicts []ConflictResponse `json:"placement_conflicts"`
}

// VersionOperationLogResponse is one audit entry for version operations.
type VersionOperationLogResponse struct {
	ID        uint   `json:"id"`
	PlanID    uint   `json:"plan_id"`
	VersionID uint   `json:"version_id"`
	Action    string `json:"action"`
	Operator  string `json:"operator"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}
