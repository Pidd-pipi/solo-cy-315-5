package model

import (
	"time"

	"gorm.io/gorm"
)

// SchedulePlan groups all versions of one timetable solution (e.g. one
// semester schedule). Versions inside a plan can be compared with each
// other; versions of different plans cannot.
type SchedulePlan struct {
	gorm.Model
	Name             string `gorm:"size:128;not null" json:"name"`
	Description      string `gorm:"size:512" json:"description"`
	CurrentVersionID uint   `gorm:"index" json:"current_version_id"`
}

// TableName explicitly names the table to avoid GORM's default plural form.
func (SchedulePlan) TableName() string { return "schedule_plans" }

// ScheduleVersion is one immutable named draft or release of a plan.
//
// Snapshot stores the full lesson list as JSON, so the version stays
// readable and comparable even after later generations overwrite the live
// schedules table. Status is draft or published; historical releases stay
// "published" while the plan's CurrentVersionID points at the active one.
type ScheduleVersion struct {
	gorm.Model
	PlanID      uint       `gorm:"not null;uniqueIndex:uniq_plan_version_no,priority:1" json:"plan_id"`
	VersionNo   int        `gorm:"not null;uniqueIndex:uniq_plan_version_no,priority:2" json:"version_no"`
	Name        string     `gorm:"size:128;not null" json:"name"`
	Status      string     `gorm:"size:32;index;not null" json:"status"`
	Snapshot    string     `gorm:"type:text;not null" json:"-"`
	LessonCount int        `gorm:"not null;default:0" json:"lesson_count"`
	Semester    string     `gorm:"size:128" json:"semester"`
	Weeks       int        `json:"weeks"`
	DaysPerWeek int        `json:"days_per_week"`
	PublishedBy string     `gorm:"size:128" json:"published_by"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedBy   string     `gorm:"size:128;not null" json:"created_by"`
	Remark      string     `gorm:"size:512" json:"remark"`
}

// TableName explicitly names the table to avoid GORM's default plural form.
func (ScheduleVersion) TableName() string { return "schedule_versions" }

// VersionOperationLog records who did what to a plan/version and when,
// including rejected attempts (kept in Detail for auditing).
type VersionOperationLog struct {
	gorm.Model
	PlanID    uint   `gorm:"index;not null" json:"plan_id"`
	VersionID uint   `gorm:"index" json:"version_id"`
	Action    string `gorm:"size:32;not null" json:"action"`
	Operator  string `gorm:"size:128;not null" json:"operator"`
	Detail    string `gorm:"type:text" json:"detail"`
}

// TableName explicitly names the table to avoid GORM's default plural form.
func (VersionOperationLog) TableName() string { return "version_operation_logs" }
