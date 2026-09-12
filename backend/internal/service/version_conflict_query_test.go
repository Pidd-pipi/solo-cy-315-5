package service_test

// 冲突复检的主数据查询错误不能被吞掉：发布与回滚在查询教室/教师/班级/课程/
// 时间段失败时必须返回内部错误，既不能当成“没有冲突”而覆盖在线课表，也不能
// 误报成“资料缺失”的业务冲突。版本状态、方案当前版本、在线课表和审计日志都
// 必须保持不变。
//
// 复检在 IMMEDIATE 写事务内通过 masterDataLoader 读取主数据，因此这里用故障
// 注入 loader（经 service.WithMasterDataLoader 装配）来模拟某一类查询失败。

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/gbschedule/gbschedule/internal/constants"
	"github.com/gbschedule/gbschedule/internal/dto"
	"github.com/gbschedule/gbschedule/internal/model"
	"github.com/gbschedule/gbschedule/internal/repository"
	"github.com/gbschedule/gbschedule/internal/service"
)

var errQueryFailed = errors.New("simulated master-data query failure")

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(&strings.Builder{}, nil)) }

func mustListVersions(t *testing.T, s *versionSuite, planID uint) []dto.VersionSummary {
	t.Helper()
	list, err := s.svc.ListVersions(s.ctx, planID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	return list
}

// faultyField 选择让哪一类主数据查询失败。
type faultyField int

const (
	faultClassroom faultyField = iota
	faultTeacher
	faultClass
	faultCourse
	faultTimeSlot
)

// faultyLoader 是 masterDataLoader 的故障注入实现：被选中的那一类查询返回
// 错误，其余在传入的事务连接上正常查询（保持事务内一致性读取）。
type faultyLoader struct {
	fault faultyField
}

func (l faultyLoader) LoadClasses(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Class, error) {
	var out []model.Class
	if l.fault == faultClass {
		return nil, errQueryFailed
	}
	if len(ids) > 0 {
		if err := exec.WithContext(ctx).Where("id IN ?", ids).Find(&out).Error; err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l faultyLoader) LoadClassrooms(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Classroom, error) {
	var out []model.Classroom
	if l.fault == faultClassroom {
		return nil, errQueryFailed
	}
	if len(ids) > 0 {
		if err := exec.WithContext(ctx).Where("id IN ?", ids).Find(&out).Error; err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l faultyLoader) LoadTeachers(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Teacher, error) {
	var out []model.Teacher
	if l.fault == faultTeacher {
		return nil, errQueryFailed
	}
	if len(ids) > 0 {
		if err := exec.WithContext(ctx).Where("id IN ?", ids).Find(&out).Error; err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l faultyLoader) LoadCourses(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Course, error) {
	var out []model.Course
	if l.fault == faultCourse {
		return nil, errQueryFailed
	}
	if len(ids) > 0 {
		if err := exec.WithContext(ctx).Where("id IN ?", ids).Find(&out).Error; err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l faultyLoader) LoadTimeSlots(ctx context.Context, exec *gorm.DB) ([]model.TimeSlot, error) {
	var out []model.TimeSlot
	if l.fault == faultTimeSlot {
		return nil, errQueryFailed
	}
	if err := exec.WithContext(ctx).Limit(constants.MaxPageSize).Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// svcWithFaultyPlanner 构造一个版本服务，其冲突复检所用 planner 注入了恰好
// 一类会失败的主数据加载器。
func (s *versionSuite) svcWithFaultyPlanner(fault faultyField) service.VersionService {
	logger := silentLogger()
	roomRepo := repository.NewClassroomRepository(s.db)
	teacherRepo := repository.NewTeacherRepository(s.db)
	classRepo := repository.NewClassRepository(s.db)
	courseRepo := repository.NewCourseRepository(s.db)
	slotRepo := repository.NewTimeSlotRepository(s.db)

	scheduleRepo := repository.NewScheduleRepository(s.db)
	planRepo := repository.NewPlanRepository(s.db)
	planner := service.NewPlanner(s.db, roomRepo, teacherRepo, classRepo, courseRepo, slotRepo,
		service.WithMasterDataLoader(faultyLoader{fault: fault}))
	return service.NewVersionService(planRepo, scheduleRepo, planner, logger)
}

// assertInternalError 确认 err 是内部错误（不是业务冲突/非法/未找到），并带上
// 冲突复检上下文。
func assertInternalError(t *testing.T, op string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected internal error, got nil", op)
	}
	for _, sentinel := range []error{service.ErrConflict, service.ErrInvalid, service.ErrNotFound} {
		if errors.Is(err, sentinel) {
			t.Fatalf("%s: query failure must not be reported as business error %v, got %v", op, sentinel, err)
		}
	}
	if !errors.Is(err, errQueryFailed) {
		t.Fatalf("%s: error must wrap the underlying query failure, got %v", op, err)
	}
	if !strings.Contains(err.Error(), "conflict check") {
		t.Fatalf("%s: error must mention the conflict-check stage, got %v", op, err)
	}
}

func rejectCount(s *versionSuite, planID uint) int {
	return s.auditActions(planID)[constants.ActionVersionReject]
}

// 发布：任一类主数据查询失败都必须中止，返回内部错误且不产生任何副作用。
func TestVM_PublishConflictQueryFailure(t *testing.T) {
	faults := []struct {
		name  string
		fault faultyField
	}{
		{"教室查询失败", faultClassroom},
		{"教师查询失败", faultTeacher},
		{"班级查询失败", faultClass},
		{"课程查询失败", faultCourse},
		{"时间段查询失败", faultTimeSlot},
	}

	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			s := newVersionSuite(t)
			plan := s.createPlan("查询失败发布-" + tc.name)
			draft := s.draft(plan.ID, s.buildDraftRequest("待发布", "alice", s.t1))

			liveBefore := s.liveSchedules()
			currentBefore := s.currentVersionID(plan.ID)
			rejectsBefore := rejectCount(s, plan.ID)

			faultySvc := s.svcWithFaultyPlanner(tc.fault)
			_, err := faultySvc.Publish(s.ctx, plan.ID, draft.Version.ID,
				&dto.PublishVersionRequest{Operator: "alice"})

			assertInternalError(t, "publish", err)

			// 版本仍为草稿、方案当前版本未变、在线课表未被覆盖。
			if got := s.versionStatus(plan.ID, draft.Version.ID); got != constants.VersionStatusDraft {
				t.Errorf("version status = %q, want draft (must not flip to published)", got)
			}
			if got := s.currentVersionID(plan.ID); got != currentBefore {
				t.Errorf("current version = %d, want unchanged %d", got, currentBefore)
			}
			if got := len(s.liveSchedules()); got != len(liveBefore) {
				t.Errorf("live schedules changed from %d to %d rows; must not be touched", len(liveBefore), got)
			}
			// 查询故障是系统错误而非业务拒绝，不应写 version_rejected 审计。
			if got := rejectCount(s, plan.ID); got != rejectsBefore {
				t.Errorf("rejection audit changed from %d to %d; query failure must not be audited as a business rejection",
					rejectsBefore, got)
			}
		})
	}
}

// 回滚：任一类主数据查询失败都必须中止，在线课表与当前版本保持不变。
func TestVM_RollbackConflictQueryFailure(t *testing.T) {
	faults := []struct {
		name  string
		fault faultyField
	}{
		{"教室查询失败", faultClassroom},
		{"教师查询失败", faultTeacher},
		{"班级查询失败", faultClass},
		{"课程查询失败", faultCourse},
		{"时间段查询失败", faultTimeSlot},
	}

	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			s := newVersionSuite(t)
			plan := s.createPlan("查询失败回滚-" + tc.name)
			v1 := s.draft(plan.ID, s.buildDraftRequest("已发布版本", "alice", s.t1))
			if _, err := s.publish(plan.ID, v1.Version.ID, "alice"); err != nil {
				t.Fatalf("publish v1: %v", err)
			}
			liveBefore := s.liveSchedules()
			if len(liveBefore) != 1 {
				t.Fatalf("setup: expected 1 live lesson, got %d", len(liveBefore))
			}
			currentBefore := s.currentVersionID(plan.ID)
			versionsBefore := len(mustListVersions(t, s, plan.ID))
			rejectsBefore := rejectCount(s, plan.ID)

			faultySvc := s.svcWithFaultyPlanner(tc.fault)
			_, err := faultySvc.Rollback(s.ctx, plan.ID, v1.Version.ID,
				&dto.RollbackVersionRequest{Operator: "bob"})

			assertInternalError(t, "rollback", err)

			// 已发布版本状态不变、当前版本不变、在线课表仍是原内容、不产生回滚版本。
			if got := s.versionStatus(plan.ID, v1.Version.ID); got != constants.VersionStatusPublished {
				t.Errorf("source version status = %q, want published", got)
			}
			if got := s.currentVersionID(plan.ID); got != currentBefore {
				t.Errorf("current version = %d, want unchanged %d", got, currentBefore)
			}
			liveAfter := s.liveSchedules()
			if len(liveAfter) != 1 || liveAfter[0].TeacherID != liveBefore[0].TeacherID {
				t.Errorf("live timetable changed after failed rollback: before=%+v after=%+v", liveBefore, liveAfter)
			}
			if got := len(mustListVersions(t, s, plan.ID)); got != versionsBefore {
				t.Errorf("version count changed from %d to %d; failed rollback must not create a version", versionsBefore, got)
			}
			if got := rejectCount(s, plan.ID); got != rejectsBefore {
				t.Errorf("rejection audit changed from %d to %d; query failure must not be audited as a business rejection",
					rejectsBefore, got)
			}
		})
	}
}

// 查询全部成功且确有冲突时，仍按业务冲突拒绝（确保错误传播没有把真冲突也变成 500）。
func TestVM_RealConflictStillRejectedAfterErrorFix(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("真冲突不被误报")
	v1 := s.draft(plan.ID, s.buildDraftRequest("容量变化前", "alice", s.t1))

	// 正常服务：缩小教室容量 -> 发布必须是业务冲突而非内部错误。
	s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20)
	_, err := s.publish(plan.ID, v1.Version.ID, "alice")
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("expected business ErrConflict when a real conflict exists, got %v", err)
	}
	if rejectCount(s, plan.ID) != 1 {
		t.Errorf("a genuine conflict rejection must be audited, got %d rejection logs", rejectCount(s, plan.ID))
	}

	// 恢复容量后发布成功（健康路径仍可用）。
	s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 50)
	if _, err := s.publish(plan.ID, v1.Version.ID, "alice"); err != nil {
		t.Fatalf("publish after conflict resolved: %v", err)
	}
	if got := s.versionStatus(plan.ID, v1.Version.ID); got != constants.VersionStatusPublished {
		t.Errorf("version status = %q, want published", got)
	}
	if got := len(s.liveSchedules()); got != 1 {
		t.Errorf("live schedules = %d, want 1 after successful publish", got)
	}
}
