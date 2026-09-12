package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gbschedule/gbschedule/internal/constants"
	"github.com/gbschedule/gbschedule/internal/dto"
	"github.com/gbschedule/gbschedule/internal/model"
	"github.com/gbschedule/gbschedule/internal/repository"
	"gorm.io/gorm"
)

// VersionService manages named timetable drafts/releases, their comparison,
// publication (with pre-release conflict re-checking) and rollback.
type VersionService interface {
	CreatePlan(ctx context.Context, req *dto.CreatePlanRequest) (*dto.PlanResponse, error)
	ListPlans(ctx context.Context, page, pageSize int) ([]dto.PlanResponse, int64, error)
	GetPlan(ctx context.Context, id uint) (*dto.PlanResponse, error)
	CreateDraft(ctx context.Context, planID uint, req *dto.CreateDraftRequest) (*dto.CreateDraftResponse, error)
	ListVersions(ctx context.Context, planID uint) ([]dto.VersionSummary, error)
	GetVersion(ctx context.Context, planID, versionID uint) (*dto.VersionResponse, error)
	Compare(ctx context.Context, planID uint, req *dto.CompareVersionsRequest) (*dto.VersionDiffResponse, error)
	Publish(ctx context.Context, planID, versionID uint, req *dto.PublishVersionRequest) (*dto.PublishVersionResponse, error)
	Rollback(ctx context.Context, planID, versionID uint, req *dto.RollbackVersionRequest) (*dto.RollbackVersionResponse, error)
	ListOperationLogs(ctx context.Context, planID uint, page, pageSize int) ([]dto.VersionOperationLogResponse, int64, error)
}

type versionService struct {
	plans     repository.PlanRepository
	schedules repository.ScheduleRepository
	planner   Planner
	logger    *slog.Logger
}

// NewVersionService constructs a version-management service.
func NewVersionService(
	plans repository.PlanRepository,
	schedules repository.ScheduleRepository,
	planner Planner,
	logger *slog.Logger,
) VersionService {
	return &versionService{plans: plans, schedules: schedules, planner: planner, logger: logger}
}

// snapshotLesson is the immutable content of one lesson in a version.
type snapshotLesson struct {
	Week        uint `json:"week"`
	DayOfWeek   int  `json:"day_of_week"`
	TimeSlotID  uint `json:"time_slot_id"`
	ClassroomID uint `json:"classroom_id"`
	TeacherID   uint `json:"teacher_id"`
	ClassID     uint `json:"class_id"`
	CourseID    uint `json:"course_id"`
}

// lessonIdentity pairs a lesson with its class+course, the logical key of
// "what the class studies". A lesson moving slot/teacher/room is a
// modification of the same identity; appearing/disappearing is add/remove.
type lessonIdentity struct {
	Week     uint
	ClassID  uint
	CourseID uint
}

const versionTimeLayout = "2006-01-02 15:04:05"

func (s *versionService) CreatePlan(ctx context.Context, req *dto.CreatePlanRequest) (*dto.PlanResponse, error) {
	plan := &model.SchedulePlan{Name: req.Name, Description: req.Description}
	if err := s.plans.CreatePlan(ctx, plan); err != nil {
		return nil, fmt.Errorf("create plan: %w", err)
	}
	if err := s.recordLog(ctx, plan.ID, 0, constants.ActionPlanCreate, req.Operator,
		map[string]any{"name": plan.Name, "description": plan.Description}); err != nil {
		return nil, err
	}
	resp := planResponse(plan)
	return &resp, nil
}

func (s *versionService) ListPlans(ctx context.Context, page, pageSize int) ([]dto.PlanResponse, int64, error) {
	items, total, err := s.plans.ListPlans(ctx, page, pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list plans: %w", err)
	}
	out := make([]dto.PlanResponse, 0, len(items))
	for i := range items {
		out = append(out, planResponse(&items[i]))
	}
	return out, total, nil
}

func (s *versionService) GetPlan(ctx context.Context, id uint) (*dto.PlanResponse, error) {
	plan, err := s.plans.GetPlan(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get plan: %w", err)
	}
	resp := planResponse(plan)
	return &resp, nil
}

func (s *versionService) CreateDraft(ctx context.Context, planID uint, req *dto.CreateDraftRequest) (*dto.CreateDraftResponse, error) {
	plan, err := s.plans.GetPlan(ctx, planID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get plan: %w", err)
	}

	// The planner only computes in memory; the live schedules table is never
	// touched by draft generation.
	items, placementConflicts, _, err := s.planner.Plan(ctx, &req.GenerateScheduleRequest)
	if err != nil {
		return nil, err
	}

	lessons := toSnapshotLessons(items)
	snapshot, err := encodeSnapshot(lessons)
	if err != nil {
		return nil, err
	}

	version := &model.ScheduleVersion{
		PlanID:      plan.ID,
		Name:        req.Name,
		Status:      constants.VersionStatusDraft,
		Snapshot:    snapshot,
		LessonCount: len(lessons),
		Semester:    req.Semester,
		Weeks:       req.Weeks,
		DaysPerWeek: req.DaysPerWeek,
		CreatedBy:   req.Operator,
		Remark:      req.Remark,
	}
	if err := s.retryOnTxContention(ctx, func(tx *gorm.DB) error {
		count, err := s.plans.CountVersionsTx(ctx, tx, plan.ID)
		if err != nil {
			return err
		}
		// Reset the auto-populated primary key so a retried attempt gets a new
		// row identity; the previous failed insert rolled back its ID.
		version.ID = 0
		version.VersionNo = int(count) + 1
		if err := s.plans.CreateVersionTx(ctx, tx, version); err != nil {
			return err
		}
		return s.plans.CreateOperationLogTx(ctx, tx, &model.VersionOperationLog{
			PlanID: plan.ID, VersionID: version.ID, Action: constants.ActionVersionDraft, Operator: req.Operator,
			Detail: mustJSON(map[string]any{"version_no": version.VersionNo, "name": version.Name, "lesson_count": version.LessonCount}),
		})
	}); err != nil {
		return nil, mapWriteError("create draft version", err)
	}

	detail, err := s.versionDetail(ctx, version, true)
	if err != nil {
		return nil, err
	}
	return &dto.CreateDraftResponse{Plan: planResponse(plan), Version: *detail, PlacementConflicts: placementConflicts}, nil
}

func (s *versionService) ListVersions(ctx context.Context, planID uint) ([]dto.VersionSummary, error) {
	if _, err := s.requirePlan(ctx, planID); err != nil {
		return nil, err
	}
	items, err := s.plans.ListVersions(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	out := make([]dto.VersionSummary, 0, len(items))
	for i := range items {
		out = append(out, versionSummary(&items[i]))
	}
	return out, nil
}

func (s *versionService) GetVersion(ctx context.Context, planID, versionID uint) (*dto.VersionResponse, error) {
	version, err := s.requireVersionOfPlan(ctx, planID, versionID)
	if err != nil {
		return nil, err
	}
	return s.versionDetail(ctx, version, true)
}

func (s *versionService) Compare(ctx context.Context, planID uint, req *dto.CompareVersionsRequest) (*dto.VersionDiffResponse, error) {
	from, err := s.requireVersionOfPlan(ctx, planID, req.FromVersionID)
	if err != nil {
		return nil, err
	}
	to, err := s.requireVersionOfPlan(ctx, planID, req.ToVersionID)
	if err != nil {
		return nil, err
	}

	fromLessons, err := decodeSnapshot(from.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("decode from snapshot: %w", err)
	}
	toLessons, err := decodeSnapshot(to.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("decode to snapshot: %w", err)
	}

	items := diffLessons(fromLessons, toLessons)
	added, removed, modified := 0, 0, 0
	for _, item := range items {
		switch item.Type {
		case "added":
			added++
		case "removed":
			removed++
		case "modified":
			modified++
		}
	}

	if err := s.recordLog(ctx, planID, 0, constants.ActionVersionCompare, req.Operator,
		map[string]any{"from_version_id": from.ID, "to_version_id": to.ID, "added": added, "removed": removed, "modified": modified}); err != nil {
		return nil, err
	}

	return &dto.VersionDiffResponse{
		PlanID:        planID,
		FromVersion:   versionSummary(from),
		ToVersion:     versionSummary(to),
		AddedCount:    added,
		RemovedCount:  removed,
		ModifiedCount: modified,
		Items:         items,
	}, nil
}

func (s *versionService) Publish(ctx context.Context, planID, versionID uint, req *dto.PublishVersionRequest) (*dto.PublishVersionResponse, error) {
	plan, err := s.requirePlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	version, err := s.requireVersionOfPlan(ctx, planID, versionID)
	if err != nil {
		return nil, err
	}

	// Only drafts can be published. An already published version is rejected.
	if version.Status != constants.VersionStatusDraft {
		err := ConflictWithData(fmt.Sprintf("version %d is already %s and cannot be published again; only draft versions can be published", version.ID, version.Status), nil)
		_ = s.recordLog(ctx, planID, version.ID, constants.ActionVersionReject, req.Operator,
			map[string]any{"operation": "publish", "reason": err.Error()})
		return nil, err
	}

	lessons, err := decodeSnapshot(version.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}

	// Re-check conflicts against current master data before publishing.
	// A master-data lookup failure aborts with an internal error before any
	// write; only successful lookups with real conflicts reject the release.
	conflicts, err := s.planner.DetectConflicts(ctx, toModelSchedules(lessons))
	if err != nil {
		return nil, fmt.Errorf("re-check conflicts before publish: %w", err)
	}
	if len(conflicts) > 0 {
		err := ConflictWithData(fmt.Sprintf("publish rejected: %d conflict(s) found in version %d (%s), resolve them before publishing", len(conflicts), version.ID, version.Name),
			map[string]any{"conflict_count": len(conflicts), "conflicts": conflicts})
		_ = s.recordLog(ctx, planID, version.ID, constants.ActionVersionReject, req.Operator,
			map[string]any{"operation": "publish", "reason": err.Error(), "conflict_count": len(conflicts)})
		return nil, err
	}

	var appliedRows int64
	if err := s.plans.Transaction(ctx, func(tx *gorm.DB) error {
		// Conditional UPDATE ... WHERE status='draft' makes the publish
		// decision and state flip atomic under concurrency: only one racing
		// request matches a row, so a version can never be published twice.
		current, err := s.plans.PublishVersionIfDraftTx(ctx, tx, version.ID, req.Operator, req.Remark, time.Now())
		if err != nil {
			return err
		}

		activePlan, err := s.plans.GetPlanTx(ctx, tx, plan.ID)
		if err != nil {
			return err
		}
		activePlan.CurrentVersionID = current.ID
		if err := s.plans.UpdatePlanTx(ctx, tx, activePlan); err != nil {
			return err
		}

		rows, err := s.schedules.ReplaceAllTx(ctx, tx, toModelSchedules(lessons))
		if err != nil {
			return err
		}
		appliedRows = rows
		return s.plans.CreateOperationLogTx(ctx, tx, &model.VersionOperationLog{
			PlanID: plan.ID, VersionID: current.ID, Action: constants.ActionVersionPublish, Operator: req.Operator,
			Detail: mustJSON(map[string]any{"applied_rows": rows, "remark": req.Remark}),
		})
	}); err != nil {
		if errors.Is(err, repository.ErrConcurrentUpdate) {
			bizErr := ConflictWithData("version was already published by another request", nil)
			_ = s.recordLog(ctx, planID, version.ID, constants.ActionVersionReject, req.Operator,
				map[string]any{"operation": "publish", "reason": bizErr.Error()})
			return nil, bizErr
		}
		return nil, fmt.Errorf("publish version: %w", err)
	}

	refreshed, err := s.plans.GetVersion(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("reload published version: %w", err)
	}
	refreshedPlan, err := s.plans.GetPlan(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("reload plan: %w", err)
	}
	return &dto.PublishVersionResponse{Version: versionSummary(refreshed), Plan: planResponse(refreshedPlan), AppliedRows: appliedRows}, nil
}

func (s *versionService) Rollback(ctx context.Context, planID, versionID uint, req *dto.RollbackVersionRequest) (*dto.RollbackVersionResponse, error) {
	plan, err := s.requirePlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	source, err := s.requireVersionOfPlan(ctx, planID, versionID)
	if err != nil {
		return nil, err
	}

	// Rollback targets must be previously published versions — never drafts.
	if source.Status != constants.VersionStatusPublished {
		err := InvalidWithData(fmt.Sprintf("rollback rejected: version %d is a draft; only published versions can be rollback targets", source.ID), nil)
		_ = s.recordLog(ctx, planID, source.ID, constants.ActionVersionReject, req.Operator,
			map[string]any{"operation": "rollback", "reason": err.Error()})
		return nil, err
	}

	lessons, err := decodeSnapshot(source.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}

	// Re-check conflicts against current master data before the rollback can
	// overwrite the live timetable. A previously valid release may have become
	// unusable after classroom capacity changes, teacher preference updates or
	// deletion of referenced master data — in that case the rollback is
	// rejected with the concrete conflicts, exactly like publishing a draft.
	// A master-data lookup failure aborts with an internal error before any
	// write, so a query failure can never masquerade as "missing reference".
	conflicts, err := s.planner.DetectConflicts(ctx, toModelSchedules(lessons))
	if err != nil {
		return nil, fmt.Errorf("re-check conflicts before rollback: %w", err)
	}
	if len(conflicts) > 0 {
		err := ConflictWithData(fmt.Sprintf("rollback rejected: %d conflict(s) found in published version %d (%s) against current master data, resolve them before rolling back", len(conflicts), source.ID, source.Name),
			map[string]any{"conflict_count": len(conflicts), "conflicts": conflicts})
		_ = s.recordLog(ctx, planID, source.ID, constants.ActionVersionReject, req.Operator,
			map[string]any{"operation": "rollback", "reason": err.Error(), "conflict_count": len(conflicts)})
		return nil, err
	}

	rollbackName := req.Name
	if strings.TrimSpace(rollbackName) == "" {
		rollbackName = fmt.Sprintf("回滚至 v%d - %s", source.VersionNo, source.Name)
	}

	rollbackVersion := &model.ScheduleVersion{
		PlanID:      plan.ID,
		Name:        rollbackName,
		Status:      constants.VersionStatusPublished,
		Snapshot:    source.Snapshot,
		LessonCount: source.LessonCount,
		Semester:    source.Semester,
		Weeks:       source.Weeks,
		DaysPerWeek: source.DaysPerWeek,
		CreatedBy:   req.Operator,
		PublishedBy: req.Operator,
		Remark:      req.Remark,
	}

	var appliedRows int64
	if err := s.retryOnTxContention(ctx, func(tx *gorm.DB) error {
		count, err := s.plans.CountVersionsTx(ctx, tx, plan.ID)
		if err != nil {
			return err
		}
		rollbackVersion.ID = 0
		rollbackVersion.VersionNo = int(count) + 1
		now := time.Now()
		rollbackVersion.PublishedAt = &now
		if err := s.plans.CreateVersionTx(ctx, tx, rollbackVersion); err != nil {
			return err
		}

		activePlan, err := s.plans.GetPlanTx(ctx, tx, plan.ID)
		if err != nil {
			return err
		}
		activePlan.CurrentVersionID = rollbackVersion.ID
		if err := s.plans.UpdatePlanTx(ctx, tx, activePlan); err != nil {
			return err
		}

		rows, err := s.schedules.ReplaceAllTx(ctx, tx, toModelSchedules(lessons))
		if err != nil {
			return err
		}
		appliedRows = rows
		return s.plans.CreateOperationLogTx(ctx, tx, &model.VersionOperationLog{
			PlanID: plan.ID, VersionID: rollbackVersion.ID, Action: constants.ActionVersionRollback, Operator: req.Operator,
			Detail: mustJSON(map[string]any{"source_version_id": source.ID, "source_version_no": source.VersionNo, "new_version_id": rollbackVersion.ID, "applied_rows": rows}),
		})
	}); err != nil {
		return nil, mapWriteError("rollback version", err)
	}

	refreshedPlan, _ := s.plans.GetPlan(ctx, planID)
	rollbackVersion.PlanID = plan.ID
	return &dto.RollbackVersionResponse{
		Plan:            planResponse(refreshedPlan),
		SourceVersion:   versionSummary(source),
		RollbackVersion: versionSummary(rollbackVersion),
		AppliedRows:     appliedRows,
	}, nil
}

func (s *versionService) ListOperationLogs(ctx context.Context, planID uint, page, pageSize int) ([]dto.VersionOperationLogResponse, int64, error) {
	if _, err := s.requirePlan(ctx, planID); err != nil {
		return nil, 0, err
	}
	items, total, err := s.plans.ListOperationLogs(ctx, planID, page, pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list operation logs: %w", err)
	}
	out := make([]dto.VersionOperationLogResponse, 0, len(items))
	for i := range items {
		out = append(out, dto.VersionOperationLogResponse{
			ID:        items[i].ID,
			PlanID:    items[i].PlanID,
			VersionID: items[i].VersionID,
			Action:    items[i].Action,
			Operator:  items[i].Operator,
			Detail:    items[i].Detail,
			CreatedAt: items[i].CreatedAt.Format(versionTimeLayout),
		})
	}
	return out, total, nil
}

// ---- helpers ----

func (s *versionService) requirePlan(ctx context.Context, planID uint) (*model.SchedulePlan, error) {
	plan, err := s.plans.GetPlan(ctx, planID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get plan: %w", err)
	}
	return plan, nil
}

// requireVersionOfPlan loads a version and rejects it when it does not belong
// to the given plan — this is the cross-plan guard shared by all version ops.
func (s *versionService) requireVersionOfPlan(ctx context.Context, planID, versionID uint) (*model.ScheduleVersion, error) {
	version, err := s.plans.GetVersion(ctx, versionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get version: %w", err)
	}
	if version.PlanID != planID {
		return nil, InvalidWithData(fmt.Sprintf("version %d belongs to plan %d, not plan %d; cross-plan access is not allowed", versionID, version.PlanID, planID), nil)
	}
	return version, nil
}

func (s *versionService) versionDetail(ctx context.Context, version *model.ScheduleVersion, enrich bool) (*dto.VersionResponse, error) {
	lessons, err := decodeSnapshot(version.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	resp := &dto.VersionResponse{
		ID:          version.ID,
		PlanID:      version.PlanID,
		VersionNo:   version.VersionNo,
		Name:        version.Name,
		Status:      version.Status,
		Semester:    version.Semester,
		Weeks:       version.Weeks,
		DaysPerWeek: version.DaysPerWeek,
		LessonCount: version.LessonCount,
		CreatedBy:   version.CreatedBy,
		CreatedAt:   version.CreatedAt.Format(versionTimeLayout),
		PublishedBy: version.PublishedBy,
		PublishedAt: formatTimePtr(version.PublishedAt),
		Remark:      version.Remark,
		Lessons:     make([]dto.LessonDetail, 0, len(lessons)),
	}

	details := make([]dto.LessonDetail, 0, len(lessons))
	if enrich {
		enriched, err := s.planner.Enrich(ctx, toModelSchedules(lessons))
		if err != nil {
			return nil, fmt.Errorf("enrich snapshot lessons: %w", err)
		}
		for i, l := range lessons {
			d := dto.LessonDetail{
				Week: l.Week, DayOfWeek: l.DayOfWeek, TimeSlotID: l.TimeSlotID,
				ClassroomID: l.ClassroomID, TeacherID: l.TeacherID, ClassID: l.ClassID, CourseID: l.CourseID,
			}
			if i < len(enriched) {
				e := enriched[i]
				d.TimeSlotCode = e.TimeSlotCode
				d.ClassroomName = e.ClassroomName
				d.TeacherName = e.TeacherName
				d.ClassName = e.ClassName
				d.CourseName = e.CourseName
			}
			details = append(details, d)
		}
	} else {
		for _, l := range lessons {
			details = append(details, dto.LessonDetail{
				Week: l.Week, DayOfWeek: l.DayOfWeek, TimeSlotID: l.TimeSlotID,
				ClassroomID: l.ClassroomID, TeacherID: l.TeacherID, ClassID: l.ClassID, CourseID: l.CourseID,
			})
		}
	}
	resp.Lessons = details
	return resp, nil
}

func (s *versionService) recordLog(ctx context.Context, planID, versionID uint, action, operator string, detail any) error {
	return s.plans.CreateOperationLog(ctx, &model.VersionOperationLog{
		PlanID:    planID,
		VersionID: versionID,
		Action:    action,
		Operator:  operator,
		Detail:    mustJSON(detail),
	})
}

func planResponse(plan *model.SchedulePlan) dto.PlanResponse {
	return dto.PlanResponse{
		ID:               plan.ID,
		Name:             plan.Name,
		Description:      plan.Description,
		CurrentVersionID: plan.CurrentVersionID,
		CreatedAt:        plan.CreatedAt.Format(versionTimeLayout),
		UpdatedAt:        plan.UpdatedAt.Format(versionTimeLayout),
	}
}

func versionSummary(v *model.ScheduleVersion) dto.VersionSummary {
	return dto.VersionSummary{
		ID:          v.ID,
		PlanID:      v.PlanID,
		VersionNo:   v.VersionNo,
		Name:        v.Name,
		Status:      v.Status,
		Semester:    v.Semester,
		Weeks:       v.Weeks,
		DaysPerWeek: v.DaysPerWeek,
		LessonCount: v.LessonCount,
		CreatedBy:   v.CreatedBy,
		CreatedAt:   v.CreatedAt.Format(versionTimeLayout),
		PublishedBy: v.PublishedBy,
		PublishedAt: formatTimePtr(v.PublishedAt),
		Remark:      v.Remark,
	}
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(versionTimeLayout)
}

func toSnapshotLessons(items []model.Schedule) []snapshotLesson {
	out := make([]snapshotLesson, 0, len(items))
	for _, item := range items {
		out = append(out, snapshotLesson{
			Week:        item.Week,
			DayOfWeek:   item.DayOfWeek,
			TimeSlotID:  item.TimeSlotID,
			ClassroomID: item.ClassroomID,
			TeacherID:   item.TeacherID,
			ClassID:     item.ClassID,
			CourseID:    item.CourseID,
		})
	}
	return out
}

func toModelSchedules(lessons []snapshotLesson) []model.Schedule {
	out := make([]model.Schedule, 0, len(lessons))
	for _, l := range lessons {
		out = append(out, model.Schedule{
			Week: l.Week, DayOfWeek: l.DayOfWeek, TimeSlotID: l.TimeSlotID,
			ClassroomID: l.ClassroomID, TeacherID: l.TeacherID, ClassID: l.ClassID, CourseID: l.CourseID,
		})
	}
	return out
}

func encodeSnapshot(lessons []snapshotLesson) (string, error) {
	data, err := json.Marshal(lessons)
	if err != nil {
		return "", fmt.Errorf("encode snapshot: %w", err)
	}
	return string(data), nil
}

func decodeSnapshot(raw string) ([]snapshotLesson, error) {
	var lessons []snapshotLesson
	if err := json.Unmarshal([]byte(raw), &lessons); err != nil {
		return nil, err
	}
	return lessons, nil
}

func identityOf(l snapshotLesson) lessonIdentity {
	return lessonIdentity{Week: l.Week, ClassID: l.ClassID, CourseID: l.CourseID}
}

// diffLessons compares two snapshots by (week, class, course): same key with
// changed slot/teacher/classroom is "modified"; only in `to` is "added";
// only in `from` is "removed".
func diffLessons(from, to []snapshotLesson) []dto.VersionDiffItem {
	fromMap := map[lessonIdentity]snapshotLesson{}
	for _, l := range from {
		fromMap[identityOf(l)] = l
	}
	toMap := map[lessonIdentity]snapshotLesson{}
	for _, l := range to {
		toMap[identityOf(l)] = l
	}

	items := make([]dto.VersionDiffItem, 0)
	for key, toLesson := range toMap {
		fromLesson, ok := fromMap[key]
		if !ok {
			l := toLesson
			items = append(items, dto.VersionDiffItem{
				Type: "added", Week: key.Week, DayOfWeek: l.DayOfWeek, TimeSlotID: l.TimeSlotID,
				ClassID: key.ClassID, CourseID: key.CourseID, ToLesson: lessonDetailPtr(l),
			})
			continue
		}
		if changed := changedFields(fromLesson, toLesson); len(changed) > 0 {
			fromCopy, toCopy := fromLesson, toLesson
			items = append(items, dto.VersionDiffItem{
				Type: "modified", Week: key.Week, DayOfWeek: toCopy.DayOfWeek, TimeSlotID: toCopy.TimeSlotID,
				ClassID: key.ClassID, CourseID: key.CourseID,
				FromLesson: lessonDetailPtr(fromCopy), ToLesson: lessonDetailPtr(toCopy), ChangedFields: changed,
			})
		}
	}
	for key, fromLesson := range fromMap {
		if _, ok := toMap[key]; ok {
			continue
		}
		l := fromLesson
		items = append(items, dto.VersionDiffItem{
			Type: "removed", Week: key.Week, DayOfWeek: l.DayOfWeek, TimeSlotID: l.TimeSlotID,
			ClassID: key.ClassID, CourseID: key.CourseID, FromLesson: lessonDetailPtr(l),
		})
	}

	order := map[string]int{"added": 0, "modified": 1, "removed": 2}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return order[items[i].Type] < order[items[j].Type]
		}
		if items[i].Week != items[j].Week {
			return items[i].Week < items[j].Week
		}
		if items[i].ClassID != items[j].ClassID {
			return items[i].ClassID < items[j].ClassID
		}
		return items[i].CourseID < items[j].CourseID
	})
	return items
}

func changedFields(from, to snapshotLesson) []string {
	changed := make([]string, 0, 4)
	if from.DayOfWeek != to.DayOfWeek {
		changed = append(changed, "day_of_week")
	}
	if from.TimeSlotID != to.TimeSlotID {
		changed = append(changed, "time_slot_id")
	}
	if from.TeacherID != to.TeacherID {
		changed = append(changed, "teacher_id")
	}
	if from.ClassroomID != to.ClassroomID {
		changed = append(changed, "classroom_id")
	}
	return changed
}

func lessonDetailPtr(l snapshotLesson) *dto.LessonDetail {
	return &dto.LessonDetail{
		Week: l.Week, DayOfWeek: l.DayOfWeek, TimeSlotID: l.TimeSlotID,
		ClassroomID: l.ClassroomID, TeacherID: l.TeacherID, ClassID: l.ClassID, CourseID: l.CourseID,
	}
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
