package service_test

// 可重复运行的排课方案版本管理测试套。
//
// 每个顶层测试都能单独运行，例如：
//
//	go test ./internal/service/ -run TestVM_ConcurrentDraftVersionNumbers -v
//
// 覆盖：草稿生成、版本比较、发布、回滚、冲突拒绝、审计、并发创建/发布、
// 失败回滚对在线课表与当前版本的不变性、基础资料变化后的拒绝路径。
// 断言不放宽任何业务约束。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/gbschedule/gbschedule/internal/constants"
	"github.com/gbschedule/gbschedule/internal/database"
	"github.com/gbschedule/gbschedule/internal/dto"
	"github.com/gbschedule/gbschedule/internal/model"
	"github.com/gbschedule/gbschedule/internal/repository"
	"github.com/gbschedule/gbschedule/internal/service"
)

// suiteSeq 保证同一进程内每次套件实例使用不同的内存库名。
var suiteSeq atomic.Int64

// versionSuite 持有一个完全隔离的内存数据库与全套仓储/服务。
type versionSuite struct {
	t          *testing.T
	ctx        context.Context
	db         *gorm.DB
	dsn        string
	fileBacked bool
	svc        service.VersionService
	slotID     uint
	roomID     uint
	t1         uint
	t2         uint
	class      *model.Class
	course     *model.Course
}

func newVersionSuite(t *testing.T) *versionSuite {
	t.Helper()
	return newVersionSuiteFile(t, false)
}

// newVersionSuiteFile builds an isolated suite. With fileBacked=true it uses a
// temporary file-backed SQLite DB (production WAL + busy_timeout behavior),
// letting a blocked writer wait for the write lock and commit strictly after
// the publish/rollback transaction instead of failing with SQLITE_LOCKED as
// it does on a shared-cache in-memory DB.
func newVersionSuiteFile(t *testing.T, fileBacked bool) *versionSuite {
	t.Helper()
	seq := suiteSeq.Add(1)
	var dsn string
	var db *gorm.DB
	var err error
	if fileBacked {
		dsn = filepath.Join(t.TempDir(), fmt.Sprintf("vm_%d.db", seq))
		db, err = database.Open(dsn)
	} else {
		// 每个套件实例使用独立命名内存库（多连接共享）。进程内自增序号确保同一
		// 测试在循环中重复调用时拿到的也是全新数据库，而不是 cache=shared 复用。
		memName := fmt.Sprintf("file:vm_%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), seq)
		dsn = memName
		db, err = openSuiteDB(memName)
	}
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

	s := &versionSuite{t: t, ctx: context.Background(), db: db, dsn: dsn, fileBacked: fileBacked}
	slot := &model.TimeSlot{Code: "S1", Name: "第一节", StartTime: "08:00", EndTime: "08:45"}
	room := &model.Classroom{Code: "R301", Name: "301教室", Capacity: 50}
	teacher1 := &model.Teacher{Name: "张老师", EmployeeNo: "T001", Subjects: []string{"数学"}}
	teacher2 := &model.Teacher{Name: "李老师", EmployeeNo: "T002", Subjects: []string{"数学"}}
	s.class = &model.Class{Name: "一班", StudentCount: 40, Grade: "高一"}
	s.course = &model.Course{Name: "数学", Code: "MATH", Duration: 1}
	for _, v := range []any{slot, room, teacher1, teacher2, s.class, s.course} {
		if err := db.Create(v).Error; err != nil {
			t.Fatalf("seed master data: %v", err)
		}
	}
	s.slotID, s.roomID, s.t1, s.t2 = slot.ID, room.ID, teacher1.ID, teacher2.ID

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	roomRepo := repository.NewClassroomRepository(db)
	teacherRepo := repository.NewTeacherRepository(db)
	classRepo := repository.NewClassRepository(db)
	courseRepo := repository.NewCourseRepository(db)
	slotRepo := repository.NewTimeSlotRepository(db)
	scheduleRepo := repository.NewScheduleRepository(db)
	planRepo := repository.NewPlanRepository(db)
	planner := service.NewPlanner(s.db, roomRepo, teacherRepo, classRepo, courseRepo, slotRepo)
	s.svc = service.NewVersionService(planRepo, scheduleRepo, planner, logger)
	return s
}

// extraConn opens another connection to the same suite database.
func (s *versionSuite) extraConn() *gorm.DB {
	s.t.Helper()
	var (
		db  *gorm.DB
		err error
	)
	if s.fileBacked {
		db, err = database.Open(s.dsn)
	} else {
		db, err = openSuiteDB(s.dsn)
	}
	if err != nil {
		s.t.Fatalf("open extra connection: %v", err)
	}
	return db
}

func (s *versionSuite) createPlan(name string) *dto.PlanResponse {
	s.t.Helper()
	plan, err := s.svc.CreatePlan(s.ctx, &dto.CreatePlanRequest{Name: name, Description: name + "描述", Operator: "alice"})
	if err != nil {
		s.t.Fatalf("create plan %q: %v", name, err)
	}
	return plan
}

// buildDraftRequest 构造一个 1 周 1 节的排课草稿请求。
func (s *versionSuite) buildDraftRequest(name, operator string, teacherID uint) *dto.CreateDraftRequest {
	req := &dto.CreateDraftRequest{Name: name, Operator: operator}
	req.Semester = "2026-Spring"
	req.Weeks = 1
	req.DaysPerWeek = 1
	req.PeriodsPerDay = 1
	req.Courses = []dto.CourseRequirement{
		{CourseID: s.course.ID, WeeklyPeriods: 1, ClassID: s.class.ID, TeacherID: teacherID},
	}
	return req
}

func (s *versionSuite) draft(planID uint, req *dto.CreateDraftRequest) *dto.CreateDraftResponse {
	s.t.Helper()
	resp, err := s.svc.CreateDraft(s.ctx, planID, req)
	if err != nil {
		s.t.Fatalf("create draft %q: %v", req.Name, err)
	}
	return resp
}

func (s *versionSuite) publish(planID, versionID uint, operator string) (*dto.PublishVersionResponse, error) {
	return s.svc.Publish(s.ctx, planID, versionID, &dto.PublishVersionRequest{Operator: operator})
}

func (s *versionSuite) rollback(planID, versionID uint, operator, name string) (*dto.RollbackVersionResponse, error) {
	return s.svc.Rollback(s.ctx, planID, versionID, &dto.RollbackVersionRequest{Operator: operator, Name: name})
}

func (s *versionSuite) liveSchedules() []model.Schedule {
	s.t.Helper()
	var items []model.Schedule
	if err := s.db.Order("week asc, id asc").Find(&items).Error; err != nil {
		s.t.Fatalf("load live schedules: %v", err)
	}
	return items
}

func (s *versionSuite) currentVersionID(planID uint) uint {
	s.t.Helper()
	plan, err := s.svc.GetPlan(s.ctx, planID)
	if err != nil {
		s.t.Fatalf("reload plan: %v", err)
	}
	return plan.CurrentVersionID
}

func (s *versionSuite) versionStatus(planID, versionID uint) string {
	s.t.Helper()
	v, err := s.svc.GetVersion(s.ctx, planID, versionID)
	if err != nil {
		s.t.Fatalf("get version %d: %v", versionID, err)
	}
	return v.Status
}

func (s *versionSuite) auditActions(planID uint) map[string]int {
	s.t.Helper()
	logs, _, err := s.svc.ListOperationLogs(s.ctx, planID, 1, 200)
	if err != nil {
		s.t.Fatalf("list logs: %v", err)
	}
	counts := map[string]int{}
	for _, l := range logs {
		counts[l.Action]++
	}
	return counts
}

func conflictTypes(err error) []string {
	var bizErr *service.BusinessError
	if !errors.As(err, &bizErr) {
		return nil
	}
	data, _ := bizErr.Data.(map[string]any)
	raw, _ := data["conflicts"].([]dto.ConflictResponse)
	types := make([]string, 0, len(raw))
	for _, c := range raw {
		types = append(types, c.Type)
	}
	return types
}

func hasConflictType(types []string, want string) bool {
	for _, ty := range types {
		if ty == want {
			return true
		}
	}
	return false
}

// ---- 1. 草稿生成 ----

func TestVM_DraftGeneration(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("草稿方案")

	before := s.liveSchedules()
	if len(before) != 0 {
		t.Fatalf("baseline live schedules must be empty, got %d", len(before))
	}

	d1 := s.draft(plan.ID, s.buildDraftRequest("初稿", "alice", s.t1))
	v1 := d1.Version
	if v1.Status != constants.VersionStatusDraft {
		t.Errorf("new version status = %q, want %q", v1.Status, constants.VersionStatusDraft)
	}
	if v1.VersionNo != 1 {
		t.Errorf("first version_no = %d, want 1", v1.VersionNo)
	}
	if v1.CreatedBy != "alice" {
		t.Errorf("created_by = %q, want alice", v1.CreatedBy)
	}
	if v1.CreatedAt == "" {
		t.Error("created_at must be recorded")
	}
	if v1.LessonCount != 1 || len(v1.Lessons) != 1 {
		t.Fatalf("lesson_count=%d lessons=%d, want both 1", v1.LessonCount, len(v1.Lessons))
	}
	if got := v1.Lessons[0].TeacherName; got != "张老师" {
		t.Errorf("lesson teacher_name = %q, want 张老师", got)
	}
	if got := len(s.liveSchedules()); got != 0 {
		t.Errorf("draft generation must not touch live schedules, got %d rows", got)
	}

	d2 := s.draft(plan.ID, s.buildDraftRequest("第二版", "bob", s.t1))
	if d2.Version.VersionNo != 2 {
		t.Errorf("second version_no = %d, want 2", d2.Version.VersionNo)
	}

	list, err := s.svc.ListVersions(s.ctx, plan.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(list) != 2 || list[0].VersionNo != 1 || list[1].VersionNo != 2 {
		t.Fatalf("version list = %+v, want 2 versions ordered 1,2", list)
	}
	detail, err := s.svc.GetVersion(s.ctx, plan.ID, v1.ID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if detail.ID != v1.ID || len(detail.Lessons) != 1 {
		t.Errorf("version detail wrong: id=%d lessons=%d", detail.ID, len(detail.Lessons))
	}
}

// ---- 2. 版本比较 ----

func TestVM_CompareVersions(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("比较方案")
	v1 := s.draft(plan.ID, s.buildDraftRequest("v1", "alice", s.t1))

	// v2 换教师：同周同班同课改教师 -> modified(teacher_id)
	v2 := s.draft(plan.ID, s.buildDraftRequest("v2-换老师", "alice", s.t2))

	diff, err := s.svc.Compare(s.ctx, plan.ID, &dto.CompareVersionsRequest{
		FromVersionID: v1.Version.ID, ToVersionID: v2.Version.ID, Operator: "alice",
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if diff.AddedCount != 0 || diff.RemovedCount != 0 || diff.ModifiedCount != 1 {
		t.Fatalf("diff counts +%d -%d ~%d, want 0/0/1", diff.AddedCount, diff.RemovedCount, diff.ModifiedCount)
	}
	item := diff.Items[0]
	if item.FromLesson == nil || item.ToLesson == nil {
		t.Fatalf("modified item must carry both lessons: %+v", item)
	}
	if item.FromLesson.TeacherID != s.t1 || item.ToLesson.TeacherID != s.t2 {
		t.Errorf("teacher change wrong: %d -> %d, want %d -> %d",
			item.FromLesson.TeacherID, item.ToLesson.TeacherID, s.t1, s.t2)
	}
	if len(item.ChangedFields) != 1 || item.ChangedFields[0] != "teacher_id" {
		t.Errorf("changed_fields = %v, want [teacher_id]", item.ChangedFields)
	}

	// 跨方案比较必须拒绝。
	planB := s.createPlan("方案B")
	_, err = s.svc.Compare(s.ctx, planB.ID, &dto.CompareVersionsRequest{
		FromVersionID: v1.Version.ID, ToVersionID: v2.Version.ID, Operator: "alice",
	})
	if !errors.Is(err, service.ErrInvalid) {
		t.Errorf("cross-plan compare err = %v, want ErrInvalid", err)
	}
}

// ---- 3. 发布与回滚 ----

func TestVM_PublishAndRollback(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("发布回滚方案")
	v1 := s.draft(plan.ID, s.buildDraftRequest("第一版", "alice", s.t1))

	pub1, err := s.publish(plan.ID, v1.Version.ID, "reviewer-bob")
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	if pub1.Version.Status != constants.VersionStatusPublished {
		t.Errorf("v1 status = %q, want published", pub1.Version.Status)
	}
	if pub1.Version.PublishedBy != "reviewer-bob" || pub1.Version.PublishedAt == "" {
		t.Errorf("publish audit fields wrong: by=%q at=%q", pub1.Version.PublishedBy, pub1.Version.PublishedAt)
	}
	if pub1.Plan.CurrentVersionID != v1.Version.ID {
		t.Errorf("plan current = %d, want %d", pub1.Plan.CurrentVersionID, v1.Version.ID)
	}
	live := s.liveSchedules()
	if len(live) != 1 || live[0].TeacherID != s.t1 {
		t.Fatalf("live after publish = %+v, want 1 lesson by t1", live)
	}

	// 回滚到当前已发布版本自身（无主数据变化，应允许：产生一个内容相同的新版本）。
	rb, err := s.rollback(plan.ID, v1.Version.ID, "carol", "重放回滚")
	if err != nil {
		t.Fatalf("rollback to current published version: %v", err)
	}
	if rb.RollbackVersion.Status != constants.VersionStatusPublished {
		t.Errorf("rollback version status = %q, want published", rb.RollbackVersion.Status)
	}
	if rb.RollbackVersion.CreatedBy != "carol" || rb.RollbackVersion.PublishedBy != "carol" {
		t.Errorf("rollback operator not recorded: %+v", rb.RollbackVersion)
	}
	if rb.Plan.CurrentVersionID != rb.RollbackVersion.ID {
		t.Errorf("plan current = %d, want rollback version %d", rb.Plan.CurrentVersionID, rb.RollbackVersion.ID)
	}
	if rb.AppliedRows != 1 {
		t.Errorf("applied_rows = %d, want 1", rb.AppliedRows)
	}
	if live := s.liveSchedules(); len(live) != 1 {
		t.Errorf("live after rollback = %d rows, want 1", len(live))
	}

	// 回滚草稿必须拒绝。
	draft := s.draft(plan.ID, s.buildDraftRequest("未发布", "alice", s.t1))
	if _, err := s.rollback(plan.ID, draft.Version.ID, "carol", ""); !errors.Is(err, service.ErrInvalid) {
		t.Errorf("rollback to draft err = %v, want ErrInvalid", err)
	}
}

// ---- 4. 冲突拒绝：发布与回滚 ----

func TestVM_PublishRejectsConflicts(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("冲突发布方案")
	v1 := s.draft(plan.ID, s.buildDraftRequest("容量变化前", "alice", s.t1))

	// 教室容量缩小到小于班级人数。
	if err := s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20).Error; err != nil {
		t.Fatalf("shrink room: %v", err)
	}

	_, err := s.publish(plan.ID, v1.Version.ID, "alice")
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("conflict publish err = %v, want ErrConflict", err)
	}
	if !hasConflictType(conflictTypes(err), constants.ConflictClassroomCap) {
		t.Errorf("conflict types = %v, want to include %s", conflictTypes(err), constants.ConflictClassroomCap)
	}
	if got := s.versionStatus(plan.ID, v1.Version.ID); got != constants.VersionStatusDraft {
		t.Errorf("rejected version status = %q, want draft", got)
	}
	if got := s.currentVersionID(plan.ID); got != 0 {
		t.Errorf("plan current = %d, want 0 (unset) after rejection", got)
	}
	if got := len(s.liveSchedules()); got != 0 {
		t.Errorf("rejected publish changed live schedules, got %d rows", got)
	}
}

func TestVM_RollbackRejectsAfterMasterDataChange(t *testing.T) {
	cases := []struct {
		name       string
		conflictTy string
		mutate     func(s *versionSuite)
		restore    func(s *versionSuite)
	}{
		{
			name:       "教室容量缩小",
			conflictTy: constants.ConflictClassroomCap,
			mutate: func(s *versionSuite) {
				s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20)
			},
			restore: func(s *versionSuite) {
				s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 50)
			},
		},
		{
			name:       "教师偏好变更",
			conflictTy: constants.ConflictTeacherPref,
			mutate: func(s *versionSuite) {
				var teacher model.Teacher
				s.db.First(&teacher, s.t1)
				teacher.UnavailableSlots = []string{"S1"}
				s.db.Save(&teacher)
			},
			restore: func(s *versionSuite) {
				var teacher model.Teacher
				s.db.First(&teacher, s.t1)
				teacher.UnavailableSlots = []string{}
				s.db.Save(&teacher)
			},
		},
		{
			name:       "引用教室被删除",
			conflictTy: constants.ConflictReferenceMissing,
			mutate:     func(s *versionSuite) { s.db.Delete(&model.Classroom{}, s.roomID) },
			restore: func(s *versionSuite) {
				// 软删除恢复
				s.db.Unscoped().Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("deleted_at", nil)
			},
		},
		{
			name:       "引用教师被删除",
			conflictTy: constants.ConflictReferenceMissing,
			mutate:     func(s *versionSuite) { s.db.Delete(&model.Teacher{}, s.t1) },
			restore: func(s *versionSuite) {
				s.db.Unscoped().Model(&model.Teacher{}).Where("id = ?", s.t1).Update("deleted_at", nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newVersionSuite(t)
			plan := s.createPlan("门禁-" + tc.name)
			v1 := s.draft(plan.ID, s.buildDraftRequest("第一版", "alice", s.t1))
			if _, err := s.publish(plan.ID, v1.Version.ID, "alice"); err != nil {
				t.Fatalf("publish v1: %v", err)
			}
			// 当前在线课表为 1 节、当前版本 v1。
			tc.mutate(s)

			_, err := s.rollback(plan.ID, v1.Version.ID, "bob", "回滚尝试")
			if !errors.Is(err, service.ErrConflict) {
				t.Fatalf("rollback after %s err = %v, want ErrConflict", tc.name, err)
			}
			if !hasConflictType(conflictTypes(err), tc.conflictTy) {
				t.Errorf("conflict types = %v, want include %s", conflictTypes(err), tc.conflictTy)
			}
			if !strings.Contains(err.Error(), "rollback rejected") {
				t.Errorf("error message must explain rollback rejection: %v", err)
			}
			// 失败回滚不改变在线课表与当前版本，也不产生新版本。
			if live := s.liveSchedules(); len(live) != 1 {
				t.Errorf("rejected rollback changed live schedules, got %d rows, want 1", len(live))
			}
			if cur := s.currentVersionID(plan.ID); cur != v1.Version.ID {
				t.Errorf("current version = %d after rejected rollback, want %d", cur, v1.Version.ID)
			}
			if list, _ := s.svc.ListVersions(s.ctx, plan.ID); len(list) != 1 {
				t.Errorf("rejected rollback created a version, total = %d, want 1", len(list))
			}

			// 恢复主数据后回滚成功，证明门禁不是无条件阻断。
			tc.restore(s)
			rb, err := s.rollback(plan.ID, v1.Version.ID, "bob", "恢复后回滚")
			if err != nil {
				t.Fatalf("rollback after restoring master data: %v", err)
			}
			if rb.RollbackVersion.Status != constants.VersionStatusPublished {
				t.Errorf("rollback version status = %q, want published", rb.RollbackVersion.Status)
			}
			if live := s.liveSchedules(); len(live) != 1 {
				t.Errorf("live after successful rollback = %d rows, want 1", len(live))
			}
		})
	}
}

// ---- 5. 审计 ----

func TestVM_AuditLog(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("审计方案")
	v1 := s.draft(plan.ID, s.buildDraftRequest("审计草稿", "alice", s.t1))

	// 一次被拒绝的发布。
	s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20)
	if _, err := s.publish(plan.ID, v1.Version.ID, "charlie"); err == nil {
		t.Fatal("expected conflict rejection")
	}
	s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 50)
	if _, err := s.publish(plan.ID, v1.Version.ID, "dora"); err != nil {
		t.Fatalf("publish after fix: %v", err)
	}
	if _, err := s.rollback(plan.ID, v1.Version.ID, "eric", ""); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	logs, total, err := s.svc.ListOperationLogs(s.ctx, plan.ID, 1, 200)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if int(total) != len(logs) {
		t.Errorf("total=%d but returned %d logs", total, len(logs))
	}
	wantActions := map[string]string{
		constants.ActionPlanCreate:      "alice",
		constants.ActionVersionDraft:    "alice",
		constants.ActionVersionReject:   "charlie",
		constants.ActionVersionPublish:  "dora",
		constants.ActionVersionRollback: "eric",
	}
	seenAction := map[string]string{}
	for _, l := range logs {
		if l.Operator == "" || l.CreatedAt == "" {
			t.Errorf("audit entry missing operator/time: %+v", l)
		}
		seenAction[l.Action] = l.Operator
	}
	for action, operator := range wantActions {
		if got := seenAction[action]; got != operator {
			t.Errorf("action %s operator = %q, want %q (actions seen: %v)", action, got, operator, seenAction)
		}
	}
}

// ---- 6. 并发：创建草稿版本号不重复、不缺口 ----

func TestVM_ConcurrentDraftVersionNumbers(t *testing.T) {
	const n = 12
	for run := 0; run < 3; run++ { // 重复运行以提高竞态命中率
		s := newVersionSuite(t)
		plan := s.createPlan(fmt.Sprintf("并发草稿-run%d", run))

		var wg sync.WaitGroup
		errs := make([]error, n)
		nos := make([]int, n)
		ids := make([]uint, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := s.buildDraftRequest(fmt.Sprintf("并发草稿-%d", i), fmt.Sprintf("op-%d", i), s.t1)
				<-start
				resp, err := s.svc.CreateDraft(s.ctx, plan.ID, req)
				errs[i] = err
				if err == nil {
					nos[i] = resp.Version.VersionNo
					ids[i] = resp.Version.ID
				}
			}(i)
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("run %d draft %d failed: %v", run, i, err)
			}
		}
		sort.Ints(nos)
		for i := 0; i < n; i++ {
			if nos[i] != i+1 {
				t.Fatalf("run %d version numbers = %v, want exactly 1..%d unique & contiguous", run, nos, n)
			}
		}
		idSeen := map[uint]bool{}
		for _, id := range ids {
			if id == 0 || idSeen[id] {
				t.Fatalf("run %d duplicate/zero version id: %v", run, ids)
			}
			idSeen[id] = true
		}
		if list, _ := s.svc.ListVersions(s.ctx, plan.ID); len(list) != n {
			t.Fatalf("run %d persisted versions = %d, want %d", run, len(list), n)
		}
		if got := len(s.liveSchedules()); got != 0 {
			t.Fatalf("run %d concurrent drafts touched live schedules: %d rows", run, got)
		}
	}
}

// ---- 7. 并发：同一版本发布恰好一个赢家 ----

func TestVM_ConcurrentPublishSameVersion(t *testing.T) {
	const n = 8
	for run := 0; run < 3; run++ {
		s := newVersionSuite(t)
		plan := s.createPlan(fmt.Sprintf("并发同版发布-run%d", run))
		v1 := s.draft(plan.ID, s.buildDraftRequest("唯一草稿", "alice", s.t1))

		var wg sync.WaitGroup
		results := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, results[i] = s.publish(plan.ID, v1.Version.ID, fmt.Sprintf("op-%d", i))
			}(i)
		}
		close(start)
		wg.Wait()

		wins, losses := 0, 0
		for i, err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, service.ErrConflict):
				losses++
			default:
				t.Fatalf("run %d publisher %d unexpected err: %v", run, i, err)
			}
		}
		if wins != 1 || losses != n-1 {
			t.Fatalf("run %d publish winners=%d losers=%d, want exactly 1 winner and %d losers", run, wins, losses, n-1)
		}
		if got := s.versionStatus(plan.ID, v1.Version.ID); got != constants.VersionStatusPublished {
			t.Fatalf("run %d version status = %q, want published", run, got)
		}
		if cur := s.currentVersionID(plan.ID); cur != v1.Version.ID {
			t.Fatalf("run %d current = %d, want %d", run, cur, v1.Version.ID)
		}
		if live := s.liveSchedules(); len(live) != 1 {
			t.Fatalf("run %d live rows = %d, want exactly 1", run, len(live))
		}
		actions := s.auditActions(plan.ID)
		if actions[constants.ActionVersionPublish] != 1 {
			t.Fatalf("run %d publish audit entries = %d, want 1", run, actions[constants.ActionVersionPublish])
		}
		if actions[constants.ActionVersionReject] != n-1 {
			t.Fatalf("run %d rejection audit entries = %d, want %d", run, actions[constants.ActionVersionReject], n-1)
		}
	}
}

// ---- 8. 并发：冲突版本发布无一漏网，在线课表与当前版本不变 ----

func TestVM_ConcurrentPublishAllConflict(t *testing.T) {
	const n = 8
	s := newVersionSuite(t)
	plan := s.createPlan("并发冲突发布")
	v1 := s.draft(plan.ID, s.buildDraftRequest("冲突草稿", "alice", s.t1))
	s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20)

	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = s.publish(plan.ID, v1.Version.ID, fmt.Sprintf("op-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range results {
		if !errors.Is(err, service.ErrConflict) {
			t.Fatalf("publisher %d err = %v, want ErrConflict for all", i, err)
		}
	}
	if got := s.versionStatus(plan.ID, v1.Version.ID); got != constants.VersionStatusDraft {
		t.Errorf("version status = %q, want draft", got)
	}
	if cur := s.currentVersionID(plan.ID); cur != 0 {
		t.Errorf("current version = %d, want 0", cur)
	}
	if live := s.liveSchedules(); len(live) != 0 {
		t.Errorf("live rows = %d, want 0 (conflict publishes must never replace timetable)", len(live))
	}
	if actions := s.auditActions(plan.ID); actions[constants.ActionVersionPublish] != 0 {
		t.Errorf("successful publish audit = %d, want 0", actions[constants.ActionVersionPublish])
	}
}

// ---- 9. 并发：不同版本发布后当前版本与在线课表事务一致 ----

func TestVM_ConcurrentPublishDistinctVersionsConsistency(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("并发不同版本")
	va := s.draft(plan.ID, s.buildDraftRequest("张老师版", "alice", s.t1)).Version
	vb := s.draft(plan.ID, s.buildDraftRequest("李老师版", "alice", s.t2)).Version

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, _ = s.publish(plan.ID, va.ID, "op-a") }()
	go func() {
		defer wg.Done()
		<-start
		_, _ = s.publish(plan.ID, vb.ID, "op-b")
	}()
	close(start)
	wg.Wait()

	cur := s.currentVersionID(plan.ID)
	if cur != va.ID && cur != vb.ID {
		t.Fatalf("current version = %d, want one of %d/%d", cur, va.ID, vb.ID)
	}
	live := s.liveSchedules()
	if len(live) != 1 {
		t.Fatalf("live rows = %d, want 1", len(live))
	}
	// 事务一致性：当前版本指向谁，在线课表就必须是谁的快照。
	wantTeacher := s.t1
	if cur == vb.ID {
		wantTeacher = s.t2
	}
	if live[0].TeacherID != wantTeacher {
		t.Errorf("current version=%d but live teacher=%d, want teacher=%d (plan/timetable mismatch)",
			cur, live[0].TeacherID, wantTeacher)
	}
}

// ---- 10. 并发：回滚版本号唯一，且冲突回滚不落任何痕迹 ----

func TestVM_ConcurrentRollback(t *testing.T) {
	const n = 6
	s := newVersionSuite(t)
	plan := s.createPlan("并发回滚")
	v1 := s.draft(plan.ID, s.buildDraftRequest("第一版", "alice", s.t1))
	if _, err := s.publish(plan.ID, v1.Version.ID, "alice"); err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, n)
	nos := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := s.rollback(plan.ID, v1.Version.ID, fmt.Sprintf("op-%d", i), "")
			results[i] = err
			if err == nil {
				nos[i] = resp.RollbackVersion.VersionNo
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("rollback %d failed: %v", i, err)
		}
	}
	valid := make([]int, 0, n)
	for _, no := range nos {
		if no != 0 {
			valid = append(valid, no)
		}
	}
	sort.Ints(valid)
	// 原始 v1 占 version_no=1，回滚版本应连续占 2..n+1。
	if len(valid) != n {
		t.Fatalf("successful rollbacks = %d, want %d; nos=%v", len(valid), n, valid)
	}
	for i, no := range valid {
		if no != i+2 {
			t.Fatalf("rollback version numbers = %v, want 2..%d unique & contiguous", valid, n+1)
		}
	}
	if live := s.liveSchedules(); len(live) != 1 {
		t.Fatalf("live rows = %d, want 1", len(live))
	}
}

// ---- 辅助：独立的、带 busy_timeout 的内存库 ----

func openSuiteDB(dsn string) (*gorm.DB, error) {
	return database.OpenMemory(dsn)
}
