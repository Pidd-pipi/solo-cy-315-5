package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/gbschedule/gbschedule/internal/constants"
	"github.com/gbschedule/gbschedule/internal/dto"
	"github.com/gbschedule/gbschedule/internal/model"
	"github.com/gbschedule/gbschedule/internal/repository"
	"github.com/gbschedule/gbschedule/internal/service"
)

type versionFixture struct {
	ctx      context.Context
	db       *gorm.DB
	svc      service.VersionService
	slot     *model.TimeSlot
	room     *model.Classroom
	teacher  *model.Teacher
	teacher2 *model.Teacher
	class    *model.Class
	course   *model.Course
}

func newVersionFixture(t *testing.T) *versionFixture {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.Classroom{}, &model.Teacher{}, &model.Class{}, &model.Course{},
		&model.TimeSlot{}, &model.Schedule{}, &model.AdjustmentLog{},
		&model.SchedulePlan{}, &model.ScheduleVersion{}, &model.VersionOperationLog{},
	); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	f := &versionFixture{ctx: context.Background(), db: db}
	f.slot = &model.TimeSlot{Code: "S1", Name: "第一节", StartTime: "08:00", EndTime: "08:45"}
	f.room = &model.Classroom{Code: "R301", Name: "301教室", Capacity: 50}
	f.teacher = &model.Teacher{Name: "张老师", EmployeeNo: "T001", Subjects: []string{"数学"}}
	f.teacher2 = &model.Teacher{Name: "李老师", EmployeeNo: "T002", Subjects: []string{"数学"}}
	f.class = &model.Class{Name: "一班", StudentCount: 40, Grade: "高一"}
	f.course = &model.Course{Name: "数学", Code: "MATH", Duration: 1}
	for _, v := range []any{f.slot, f.room, f.teacher, f.teacher2, f.class, f.course} {
		if err := db.Create(v).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	roomRepo := repository.NewClassroomRepository(db)
	teacherRepo := repository.NewTeacherRepository(db)
	classRepo := repository.NewClassRepository(db)
	courseRepo := repository.NewCourseRepository(db)
	slotRepo := repository.NewTimeSlotRepository(db)
	scheduleRepo := repository.NewScheduleRepository(db)
	planRepo := repository.NewPlanRepository(db)
	planner := service.NewPlanner(roomRepo, teacherRepo, classRepo, courseRepo, slotRepo)
	f.svc = service.NewVersionService(planRepo, scheduleRepo, planner, logger)
	return f
}

func (f *versionFixture) createPlan(t *testing.T, name string) *dto.PlanResponse {
	t.Helper()
	plan, err := f.svc.CreatePlan(f.ctx, &dto.CreatePlanRequest{Name: name, Description: name + "描述", Operator: "alice"})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return plan
}

func (f *versionFixture) draftReq(name, operator string, weeks int, teacherID uint) *dto.CreateDraftRequest {
	req := &dto.CreateDraftRequest{
		Name:     name,
		Operator: operator,
	}
	req.Semester = "2026-Spring"
	req.Weeks = weeks
	req.DaysPerWeek = 1
	req.PeriodsPerDay = 1
	req.Courses = []dto.CourseRequirement{
		{CourseID: f.course.ID, WeeklyPeriods: 1, ClassID: f.class.ID, TeacherID: teacherID},
	}
	return req
}

func (f *versionFixture) createDraft(t *testing.T, planID uint, req *dto.CreateDraftRequest) *dto.CreateDraftResponse {
	t.Helper()
	resp, err := f.svc.CreateDraft(f.ctx, planID, req)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return resp
}

func liveScheduleCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.Schedule{}).Count(&n).Error; err != nil {
		t.Fatalf("count schedules: %v", err)
	}
	return n
}

// 1. 草稿生成：运行排课算法、生成带名称草稿、自增版本号、记录执行人/时间，且不污染线上课表。
func TestVersionDraftGeneration(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "2026春季课表")

	if got := liveScheduleCount(t, f.db); got != 0 {
		t.Fatalf("live schedules should be empty before draft, got %d", got)
	}

	d1 := f.createDraft(t, plan.ID, f.draftReq("初稿", "alice", 1, f.teacher.ID))
	v1 := d1.Version
	if v1.Status != constants.VersionStatusDraft {
		t.Fatalf("new version status = %s, want draft", v1.Status)
	}
	if v1.VersionNo != 1 || v1.Name != "初稿" {
		t.Fatalf("unexpected version meta: %+v", v1)
	}
	if v1.LessonCount != 1 || len(v1.Lessons) != 1 {
		t.Fatalf("expected 1 lesson, got count=%d len=%d", v1.LessonCount, len(v1.Lessons))
	}
	if v1.Lessons[0].TeacherName != "张老师" || v1.Lessons[0].ClassName != "一班" || v1.Lessons[0].CourseName != "数学" {
		t.Fatalf("lesson enrichment wrong: %+v", v1.Lessons[0])
	}
	if v1.CreatedBy != "alice" || v1.CreatedAt == "" {
		t.Fatalf("draft must record creator and time: created_by=%s created_at=%s", v1.CreatedBy, v1.CreatedAt)
	}
	if got := liveScheduleCount(t, f.db); got != 0 {
		t.Fatalf("draft generation must not touch live schedules, got %d rows", got)
	}

	d2 := f.createDraft(t, plan.ID, f.draftReq("第二版草稿", "bob", 2, f.teacher.ID))
	if d2.Version.VersionNo != 2 {
		t.Fatalf("second draft version_no = %d, want 2", d2.Version.VersionNo)
	}
	if d2.Version.LessonCount != 2 {
		t.Fatalf("2-week draft should have 2 lessons, got %d", d2.Version.LessonCount)
	}

	list, err := f.svc.ListVersions(f.ctx, plan.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(list) != 2 || list[0].VersionNo != 1 || list[1].VersionNo != 2 {
		t.Fatalf("version list wrong: %+v", list)
	}
	detail, err := f.svc.GetVersion(f.ctx, plan.ID, d1.Version.ID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if detail.ID != d1.Version.ID || len(detail.Lessons) != 1 {
		t.Fatalf("version detail wrong: %+v", detail)
	}
}

// 1b. 算法无法排下的需求在生成时给出 placement_conflicts，但草稿仍会保存。
func TestVersionDraftWithPlacementConflicts(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "排不下的课表")
	course2 := &model.Course{Name: "物理", Code: "PHY", Duration: 1}
	if err := f.db.Create(course2).Error; err != nil {
		t.Fatalf("create course2: %v", err)
	}
	req := f.draftReq("超容初稿", "alice", 1, f.teacher.ID)
	// 数学每周 1 节可以排下；物理同一班级每周 2 节，但每天只有 1 个时段，排不下。
	req.Courses = append(req.Courses, dto.CourseRequirement{CourseID: course2.ID, WeeklyPeriods: 2, ClassID: f.class.ID, TeacherID: f.teacher.ID})
	resp, err := f.svc.CreateDraft(f.ctx, plan.ID, req)
	if err != nil {
		t.Fatalf("draft with placement conflicts should still be stored: %v", err)
	}
	if len(resp.PlacementConflicts) == 0 {
		t.Fatalf("expected placement conflicts for unsatisfiable requirement")
	}
	if resp.Version.LessonCount != 1 || resp.Version.Status != constants.VersionStatusDraft {
		t.Fatalf("unexpected draft: %+v", resp.Version)
	}
}

// 2. 差异比较：同方案两个版本按"班级+课程+周次"为逻辑键给出 新增/删除/修改。
func TestVersionCompare(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "比较方案")

	v1 := f.createDraft(t, plan.ID, f.draftReq("v1-单周-张老师", "alice", 1, f.teacher.ID))
	v2 := f.createDraft(t, plan.ID, f.draftReq("v2-双周-张老师", "alice", 2, f.teacher.ID))

	// v1 -> v2：多出第 2 周一节课（added）
	diff, err := f.svc.Compare(f.ctx, plan.ID, &dto.CompareVersionsRequest{
		FromVersionID: v1.Version.ID, ToVersionID: v2.Version.ID, Operator: "alice",
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if diff.AddedCount != 1 || diff.RemovedCount != 0 || diff.ModifiedCount != 0 {
		t.Fatalf("v1->v2 diff counts wrong: +%d -%d ~%d", diff.AddedCount, diff.RemovedCount, diff.ModifiedCount)
	}
	if diff.Items[0].Type != "added" || diff.Items[0].ToLesson == nil || diff.Items[0].Week != 2 {
		t.Fatalf("added item wrong: %+v", diff.Items[0])
	}

	// 反向比较：v2 -> v1 为 removed
	rdiff, err := f.svc.Compare(f.ctx, plan.ID, &dto.CompareVersionsRequest{
		FromVersionID: v2.Version.ID, ToVersionID: v1.Version.ID, Operator: "alice",
	})
	if err != nil {
		t.Fatalf("reverse compare: %v", err)
	}
	if rdiff.AddedCount != 0 || rdiff.RemovedCount != 1 {
		t.Fatalf("reverse diff counts wrong: +%d -%d", rdiff.AddedCount, rdiff.RemovedCount)
	}

	// 同周同班同课改换教师 -> modified(teacher_id)
	v3 := f.createDraft(t, plan.ID, f.draftReq("v3-换老师", "bob", 1, f.teacher2.ID))
	mdiff, err := f.svc.Compare(f.ctx, plan.ID, &dto.CompareVersionsRequest{
		FromVersionID: v1.Version.ID, ToVersionID: v3.Version.ID, Operator: "alice",
	})
	if err != nil {
		t.Fatalf("compare teacher swap: %v", err)
	}
	if mdiff.ModifiedCount != 1 {
		t.Fatalf("expected 1 modified, got %d (%+v)", mdiff.ModifiedCount, mdiff.Items)
	}
	item := mdiff.Items[0]
	if item.FromLesson.TeacherID != f.teacher.ID || item.ToLesson.TeacherID != f.teacher2.ID {
		t.Fatalf("modified lesson teachers wrong: %+v", item)
	}
	changed := map[string]bool{}
	for _, c := range item.ChangedFields {
		changed[c] = true
	}
	if !changed["teacher_id"] {
		t.Fatalf("changed_fields should include teacher_id: %v", item.ChangedFields)
	}
}

// 3. 冲突发布拒绝：发布前基于当前主数据复检；存在冲突时返回 409 和冲突明细，草稿不生效。
func TestVersionPublishRejectsConflicts(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "冲突方案")
	draft := f.createDraft(t, plan.ID, f.draftReq("容量变化前的草稿", "alice", 1, f.teacher.ID))

	// 主数据变更：教室容量从 50 降到 20，班级 40 人 -> 容量冲突（草稿快照已过时）。
	if err := f.db.Model(&model.Classroom{}).Where("id = ?", f.room.ID).Update("capacity", 20).Error; err != nil {
		t.Fatalf("shrink room: %v", err)
	}

	_, err := f.svc.Publish(f.ctx, plan.ID, draft.Version.ID, &dto.PublishVersionRequest{Operator: "alice"})
	if err == nil {
		t.Fatal("publish with conflicts must be rejected")
	}
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("error should wrap ErrConflict, got %v", err)
	}
	var bizErr *service.BusinessError
	if !errors.As(err, &bizErr) || !strings.Contains(bizErr.Error(), "conflict") {
		t.Fatalf("error should explain the rejection reason, got %v", err)
	}
	data, _ := bizErr.Data.(map[string]any)
	if n, _ := data["conflict_count"].(int); n < 1 {
		t.Fatalf("rejection payload must carry conflict_count, got %#v", bizErr.Data)
	}
	rawConflicts, _ := data["conflicts"].([]dto.ConflictResponse)
	found := false
	for _, c := range rawConflicts {
		if c.Type == constants.ConflictClassroomCap {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejection payload must list the capacity conflict: %#v", data["conflicts"])
	}

	// 草稿状态不变、方案没有当前版本、线上课表仍为空。
	detail, err := f.svc.GetVersion(f.ctx, plan.ID, draft.Version.ID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if detail.Status != constants.VersionStatusDraft {
		t.Fatalf("rejected version must stay draft, got %s", detail.Status)
	}
	again, _ := f.svc.GetPlan(f.ctx, plan.ID)
	if again.CurrentVersionID != 0 {
		t.Fatalf("plan current version must remain unset, got %d", again.CurrentVersionID)
	}
	if got := liveScheduleCount(t, f.db); got != 0 {
		t.Fatalf("rejected publish must not touch live schedules, got %d", got)
	}
}

// 4. 正常发布：草稿转为已发布、记录发布人/时间、快照应用到线上课表、方案指向当前版本。
func TestVersionPublishSuccess(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "正常方案")
	draft := f.createDraft(t, plan.ID, f.draftReq("待发布", "alice", 1, f.teacher.ID))

	resp, err := f.svc.Publish(f.ctx, plan.ID, draft.Version.ID, &dto.PublishVersionRequest{Operator: "reviewer-bob"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if resp.Version.Status != constants.VersionStatusPublished {
		t.Fatalf("published status = %s", resp.Version.Status)
	}
	if resp.Version.PublishedBy != "reviewer-bob" || resp.Version.PublishedAt == "" {
		t.Fatalf("publish must record publisher and time: %+v", resp.Version)
	}
	if resp.Plan.CurrentVersionID != draft.Version.ID {
		t.Fatalf("plan should point at published version, got current=%d want=%d", resp.Plan.CurrentVersionID, draft.Version.ID)
	}
	if resp.AppliedRows != 1 {
		t.Fatalf("applied rows = %d, want 1", resp.AppliedRows)
	}
	if got := liveScheduleCount(t, f.db); got != 1 {
		t.Fatalf("live schedules after publish = %d, want 1", got)
	}
}

// 5. 回滚：只能回滚到已发布版本；回滚生成新的已发布版本并恢复其课表，回滚草稿被拒绝。
func TestVersionRollback(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "回滚方案")

	v1 := f.createDraft(t, plan.ID, f.draftReq("第一版", "alice", 1, f.teacher.ID))
	if _, err := f.svc.Publish(f.ctx, plan.ID, v1.Version.ID, &dto.PublishVersionRequest{Operator: "alice"}); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	v2 := f.createDraft(t, plan.ID, f.draftReq("第二版", "alice", 2, f.teacher.ID))
	if _, err := f.svc.Publish(f.ctx, plan.ID, v2.Version.ID, &dto.PublishVersionRequest{Operator: "alice"}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if got := liveScheduleCount(t, f.db); got != 2 {
		t.Fatalf("live should reflect v2 (2 lessons), got %d", got)
	}

	// 新建一个未发布草稿：回滚到草稿必须拒绝。
	draft3 := f.createDraft(t, plan.ID, f.draftReq("未发布草稿", "alice", 1, f.teacher.ID))
	if _, err := f.svc.Rollback(f.ctx, plan.ID, draft3.Version.ID, &dto.RollbackVersionRequest{Operator: "bob"}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("rollback to draft must be ErrInvalid, got %v", err)
	}

	// 回滚到已发布的 v1：生成新已发布版本，课表恢复为 1 节。
	rb, err := f.svc.Rollback(f.ctx, plan.ID, v1.Version.ID, &dto.RollbackVersionRequest{Operator: "bob", Name: "紧急回滚"})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rb.SourceVersion.ID != v1.Version.ID {
		t.Fatalf("rollback source wrong: %+v", rb.SourceVersion)
	}
	if rb.RollbackVersion.Status != constants.VersionStatusPublished {
		t.Fatalf("rollback-created version must be published, got %s", rb.RollbackVersion.Status)
	}
	if rb.RollbackVersion.Name != "紧急回滚" || rb.RollbackVersion.CreatedBy != "bob" || rb.RollbackVersion.PublishedBy != "bob" {
		t.Fatalf("rollback version meta wrong: %+v", rb.RollbackVersion)
	}
	if rb.Plan.CurrentVersionID != rb.RollbackVersion.ID {
		t.Fatalf("plan current must point at rollback version")
	}
	if rb.AppliedRows != 1 {
		t.Fatalf("applied rows after rollback = %d, want 1", rb.AppliedRows)
	}
	if got := liveScheduleCount(t, f.db); got != 1 {
		t.Fatalf("live schedules after rollback = %d, want 1", got)
	}
	var live model.Schedule
	if err := f.db.First(&live).Error; err != nil {
		t.Fatalf("load live schedule: %v", err)
	}
	if live.Week != 1 || live.TeacherID != f.teacher.ID || live.CourseID != f.course.ID || live.ClassID != f.class.ID {
		t.Fatalf("live schedule does not match v1 snapshot: %+v", live)
	}
	// 被回滚的历史版本仍然保留 published 状态。
	src, _ := f.svc.GetVersion(f.ctx, plan.ID, v1.Version.ID)
	if src.Status != constants.VersionStatusPublished {
		t.Fatalf("source published version must remain published, got %s", src.Status)
	}
}

// setupTwoPublishedVersions publishes v1 (1 周) 然后 v2 (2 周)，在线课表为 2 节，
// 供回滚冲突门禁测试使用。
func setupTwoPublishedVersions(t *testing.T) (*versionFixture, *dto.PlanResponse, uint, uint) {
	t.Helper()
	f := newVersionFixture(t)
	plan := f.createPlan(t, "门禁方案")
	v1 := f.createDraft(t, plan.ID, f.draftReq("第一版", "alice", 1, f.teacher.ID))
	if _, err := f.svc.Publish(f.ctx, plan.ID, v1.Version.ID, &dto.PublishVersionRequest{Operator: "alice"}); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	v2 := f.createDraft(t, plan.ID, f.draftReq("第二版", "alice", 2, f.teacher.ID))
	if _, err := f.svc.Publish(f.ctx, plan.ID, v2.Version.ID, &dto.PublishVersionRequest{Operator: "alice"}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	return f, plan, v1.Version.ID, v2.Version.ID
}

func assertConflictRejection(t *testing.T, err error, wantType string) *service.BusinessError {
	t.Helper()
	if err == nil {
		t.Fatal("rollback must be rejected, got nil")
	}
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("rollback rejection must wrap ErrConflict, got %v", err)
	}
	var bizErr *service.BusinessError
	if !errors.As(err, &bizErr) {
		t.Fatalf("rejection must carry conflict data, got %v", err)
	}
	if !strings.Contains(bizErr.Error(), "rollback rejected") {
		t.Fatalf("rejection message must explain rollback context: %v", err)
	}
	data, _ := bizErr.Data.(map[string]any)
	rawConflicts, _ := data["conflicts"].([]dto.ConflictResponse)
	found := false
	for _, c := range rawConflicts {
		if c.Type == wantType {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected conflict type %s in rejection payload, got %#v", wantType, data["conflicts"])
	}
	return bizErr
}

// 5b. 回滚冲突门禁：已发布版本在容量/教师偏好/基础资料变化后不再可用时，
// 回滚必须在覆盖在线课表前被拒绝并给出具体原因。
func TestVersionRollbackRejectsConflicts(t *testing.T) {
	t.Run("教室容量缩小", func(t *testing.T) {
		f, plan, v1, v2 := setupTwoPublishedVersions(t)
		if err := f.db.Model(&model.Classroom{}).Where("id = ?", f.room.ID).Update("capacity", 20).Error; err != nil {
			t.Fatalf("shrink room: %v", err)
		}
		_, rbErr := f.svc.Rollback(f.ctx, plan.ID, v1, &dto.RollbackVersionRequest{Operator: "bob"})
		assertConflictRejection(t, rbErr, constants.ConflictClassroomCap)
		assertRollbackRejectedSideEffects(t, f, plan, v2)

		// 恢复容量后回滚应成功，在线课表恢复为 v1 的 1 节。
		if err := f.db.Model(&model.Classroom{}).Where("id = ?", f.room.ID).Update("capacity", 50).Error; err != nil {
			t.Fatalf("restore room: %v", err)
		}
		rb, err := f.svc.Rollback(f.ctx, plan.ID, v1, &dto.RollbackVersionRequest{Operator: "bob", Name: "恢复回滚"})
		if err != nil {
			t.Fatalf("rollback after restoring capacity: %v", err)
		}
		if got := liveScheduleCount(t, f.db); got != 1 {
			t.Fatalf("live schedules after rollback = %d, want 1", got)
		}
		if rb.RollbackVersion.Status != constants.VersionStatusPublished || rb.Plan.CurrentVersionID != rb.RollbackVersion.ID {
			t.Fatalf("rollback result wrong: %+v", rb)
		}
		logs, _, _ := f.svc.ListOperationLogs(f.ctx, plan.ID, 1, 50)
		var rejected, rolledBack bool
		for _, l := range logs {
			if l.Action == constants.ActionVersionReject && l.Operator == "bob" {
				rejected = true
			}
			if l.Action == constants.ActionVersionRollback && l.Operator == "bob" {
				rolledBack = true
			}
		}
		if !rejected || !rolledBack {
			t.Fatalf("both rejection and successful rollback must be audited: rejected=%v rolledBack=%v", rejected, rolledBack)
		}
	})

	t.Run("教师偏好变更", func(t *testing.T) {
		f, plan, v1, v2 := setupTwoPublishedVersions(t)
		f.teacher.UnavailableSlots = []string{f.slot.Code}
		if err := f.db.Save(f.teacher).Error; err != nil {
			t.Fatalf("update teacher preference: %v", err)
		}
		_, err := f.svc.Rollback(f.ctx, plan.ID, v1, &dto.RollbackVersionRequest{Operator: "bob"})
		assertConflictRejection(t, err, constants.ConflictTeacherPref)
		assertRollbackRejectedSideEffects(t, f, plan, v2)
	})

	t.Run("引用教室被删除", func(t *testing.T) {
		f, plan, v1, v2 := setupTwoPublishedVersions(t)
		if err := f.db.Delete(&model.Classroom{}, f.room.ID).Error; err != nil {
			t.Fatalf("delete classroom: %v", err)
		}
		_, err := f.svc.Rollback(f.ctx, plan.ID, v1, &dto.RollbackVersionRequest{Operator: "bob"})
		assertConflictRejection(t, err, constants.ConflictReferenceMissing)
		assertRollbackRejectedSideEffects(t, f, plan, v2)
	})

	t.Run("引用教师被删除", func(t *testing.T) {
		f, plan, v1, v2 := setupTwoPublishedVersions(t)
		if err := f.db.Delete(&model.Teacher{}, f.teacher.ID).Error; err != nil {
			t.Fatalf("delete teacher: %v", err)
		}
		_, err := f.svc.Rollback(f.ctx, plan.ID, v1, &dto.RollbackVersionRequest{Operator: "bob"})
		assertConflictRejection(t, err, constants.ConflictReferenceMissing)
		assertRollbackRejectedSideEffects(t, f, plan, v2)
	})
}

// assertRollbackRejectedSideEffects 验证回滚被拒后：在线课表未被覆盖、
// 方案当前版本不变、没有生成新回滚版本、拒绝操作已审计。
func assertRollbackRejectedSideEffects(t *testing.T, f *versionFixture, plan *dto.PlanResponse, currentVersionID uint) {
	t.Helper()
	if got := liveScheduleCount(t, f.db); got != 2 {
		t.Fatalf("rejected rollback must not touch live schedules, got %d rows (want 2)", got)
	}
	gotPlan, err := f.svc.GetPlan(f.ctx, plan.ID)
	if err != nil {
		t.Fatalf("reload plan: %v", err)
	}
	if gotPlan.CurrentVersionID != currentVersionID {
		t.Fatalf("plan current version must stay %d, got %d", currentVersionID, gotPlan.CurrentVersionID)
	}
	versions, err := f.svc.ListVersions(f.ctx, plan.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("rejected rollback must not create a version, got %d versions", len(versions))
	}
	logs, _, err := f.svc.ListOperationLogs(f.ctx, plan.ID, 1, 50)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	for _, l := range logs {
		if l.Action == constants.ActionVersionReject {
			return
		}
	}
	t.Fatal("rejected rollback must be recorded in operation logs")
}

// 6a. 重复发布拒绝：已发布版本不能再次发布。
func TestVersionDuplicatePublishRejected(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "重复发布方案")
	draft := f.createDraft(t, plan.ID, f.draftReq("唯一草稿", "alice", 1, f.teacher.ID))
	if _, err := f.svc.Publish(f.ctx, plan.ID, draft.Version.ID, &dto.PublishVersionRequest{Operator: "alice"}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, err := f.svc.Publish(f.ctx, plan.ID, draft.Version.ID, &dto.PublishVersionRequest{Operator: "alice"})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("duplicate publish must be ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "already") {
		t.Fatalf("error must explain why: %v", err)
	}
}

// 6b. 跨方案操作拒绝：比较、发布、回滚、查看其他方案的版本都不允许。
func TestVersionCrossPlanRejected(t *testing.T) {
	f := newVersionFixture(t)
	planA := f.createPlan(t, "方案A")
	planB := f.createPlan(t, "方案B")
	aDraft := f.createDraft(t, planA.ID, f.draftReq("A的草稿", "alice", 1, f.teacher.ID))

	// 跨方案查看详情
	if _, err := f.svc.GetVersion(f.ctx, planB.ID, aDraft.Version.ID); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("cross-plan get must be ErrInvalid, got %v", err)
	}
	// 跨方案比较
	if _, err := f.svc.Compare(f.ctx, planB.ID, &dto.CompareVersionsRequest{
		FromVersionID: aDraft.Version.ID, ToVersionID: aDraft.Version.ID, Operator: "alice",
	}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("cross-plan compare must be ErrInvalid, got %v", err)
	}
	// 跨方案发布
	if _, err := f.svc.Publish(f.ctx, planB.ID, aDraft.Version.ID, &dto.PublishVersionRequest{Operator: "alice"}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("cross-plan publish must be ErrInvalid, got %v", err)
	}
	// 先把 A 草稿发布，再尝试从 B 回滚它
	if _, err := f.svc.Publish(f.ctx, planA.ID, aDraft.Version.ID, &dto.PublishVersionRequest{Operator: "alice"}); err != nil {
		t.Fatalf("publish on plan A: %v", err)
	}
	if _, err := f.svc.Rollback(f.ctx, planB.ID, aDraft.Version.ID, &dto.RollbackVersionRequest{Operator: "alice"}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("cross-plan rollback must be ErrInvalid, got %v", err)
	}
}

// 6c. 不存在的方案/版本返回 not found。
func TestVersionNotFound(t *testing.T) {
	f := newVersionFixture(t)
	if _, err := f.svc.CreateDraft(f.ctx, 99999, f.draftReq("x", "alice", 1, f.teacher.ID)); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("draft into missing plan must be ErrNotFound, got %v", err)
	}
	plan := f.createPlan(t, "存在的方案")
	if _, err := f.svc.GetVersion(f.ctx, plan.ID, 99999); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("missing version must be ErrNotFound, got %v", err)
	}
}

// 7. 审计：每次操作（含被拒绝的操作）都记录动作、执行人和时间。
func TestVersionOperationAuditLog(t *testing.T) {
	f := newVersionFixture(t)
	plan := f.createPlan(t, "审计方案")
	draft := f.createDraft(t, plan.ID, f.draftReq("审计草稿", "alice", 1, f.teacher.ID))

	// 制造一次冲突拒绝发布
	if err := f.db.Model(&model.Classroom{}).Where("id = ?", f.room.ID).Update("capacity", 20).Error; err != nil {
		t.Fatalf("shrink room: %v", err)
	}
	if _, err := f.svc.Publish(f.ctx, plan.ID, draft.Version.ID, &dto.PublishVersionRequest{Operator: "charlie"}); err == nil {
		t.Fatal("expected publish rejection")
	}

	logs, total, err := f.svc.ListOperationLogs(f.ctx, plan.ID, 1, 50)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if total < 3 {
		t.Fatalf("expected at least plan_create + draft + rejected logs, got total=%d", total)
	}
	seen := map[string]string{} // action -> operator
	for _, l := range logs {
		if l.CreatedAt == "" || l.Operator == "" {
			t.Fatalf("audit entry must carry operator and time: %+v", l)
		}
		seen[l.Action] = l.Operator
	}
	if seen[constants.ActionPlanCreate] != "alice" {
		t.Fatalf("missing plan_create by alice: %+v", seen)
	}
	if seen[constants.ActionVersionDraft] != "alice" {
		t.Fatalf("missing version_draft by alice: %+v", seen)
	}
	if seen[constants.ActionVersionReject] != "charlie" {
		t.Fatalf("rejected publish must be audited with operator charlie: %+v", seen)
	}

	// 恢复容量后正常发布，再核对发布审计。
	if err := f.db.Model(&model.Classroom{}).Where("id = ?", f.room.ID).Update("capacity", 50).Error; err != nil {
		t.Fatalf("restore room: %v", err)
	}
	if _, err := f.svc.Publish(f.ctx, plan.ID, draft.Version.ID, &dto.PublishVersionRequest{Operator: "dora"}); err != nil {
		t.Fatalf("publish after fix: %v", err)
	}
	if _, err := f.svc.Rollback(f.ctx, plan.ID, draft.Version.ID, &dto.RollbackVersionRequest{Operator: "eric"}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	logs, _, _ = f.svc.ListOperationLogs(f.ctx, plan.ID, 1, 50)
	actions := map[string]bool{}
	for _, l := range logs {
		actions[l.Action] = true
		if l.Action == constants.ActionVersionRollback && l.Operator != "eric" {
			t.Fatalf("rollback audit operator wrong: %+v", l)
		}
	}
	if !actions[constants.ActionVersionPublish] || !actions[constants.ActionVersionRollback] {
		t.Fatalf("audit actions incomplete: %+v", actions)
	}
}
