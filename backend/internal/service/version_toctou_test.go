package service_test

// 并发穿透（TOCTOU）回归测试。
//
// 冲突复检必须发生在发布/回滚的 IMMEDIATE 写事务内部：复检进行时事务已持有
// 写锁，另一个连接对教室容量、教师偏好等主数据的修改必须被数据库挡住
//（SQLITE_BUSY，busy_timeout 期间等待），不可能插入“复检通过”与“覆盖在线课
// 表”之间。这里用一个在复检入口阻塞的 loader 把事务稳定停在“复检中、写锁已
// 持有”的状态，确定性地验证该屏障。

import (
	"context"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gbschedule/gbschedule/internal/constants"
	"github.com/gbschedule/gbschedule/internal/dto"
	"github.com/gbschedule/gbschedule/internal/model"
	"github.com/gbschedule/gbschedule/internal/repository"
	"github.com/gbschedule/gbschedule/internal/service"
)

func publishOperator(operator string) *dto.PublishVersionRequest {
	return &dto.PublishVersionRequest{Operator: operator}
}

func rollbackOperator(operator string) *dto.RollbackVersionRequest {
	return &dto.RollbackVersionRequest{Operator: operator}
}

// blockingClassesLoader 包装生产 loader，仅拦截复检读取的第一类主数据
// （classes）：进入时关闭 entered，随后阻塞到 release 关闭。
type blockingClassesLoader struct {
	service.MasterDataLoader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockingClassesLoader) LoadClasses(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Class, error) {
	l.once.Do(func() { close(l.entered) })
	select {
	case <-l.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.MasterDataLoader.LoadClasses(ctx, exec, ids)
}

func svcWithMasterLoader(s *versionSuite, loader service.MasterDataLoader) service.VersionService {
	logger := silentLogger()
	roomRepo := repository.NewClassroomRepository(s.db)
	teacherRepo := repository.NewTeacherRepository(s.db)
	classRepo := repository.NewClassRepository(s.db)
	courseRepo := repository.NewCourseRepository(s.db)
	slotRepo := repository.NewTimeSlotRepository(s.db)
	scheduleRepo := repository.NewScheduleRepository(s.db)
	planRepo := repository.NewPlanRepository(s.db)
	planner := service.NewPlanner(s.db, roomRepo, teacherRepo, classRepo, courseRepo, slotRepo,
		service.WithMasterDataLoader(loader))
	return service.NewVersionService(planRepo, scheduleRepo, planner, logger)
}

// newBlockedSvc 构造使用阻塞 loader 的服务并返回加载器。
func newBlockedSvc(s *versionSuite) (service.VersionService, *blockingClassesLoader) {
	loader := &blockingClassesLoader{
		MasterDataLoader: service.NewGormMasterDataLoader(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	return svcWithMasterLoader(s, loader), loader
}

// waitFor 在 timeout 内等待 cond 成立。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// 发布 TOCTOU：复检进行中并发改容量必须被写锁挡住，无法在提交前生效。
func TestVM_PublishTOCTOUMasterDataChangeBlocked(t *testing.T) {
	s := newVersionSuiteFile(t, true)
	plan := s.createPlan("发布穿透方案")
	draft := s.draft(plan.ID, s.buildDraftRequest("待发布", "alice", s.t1))

	svc, loader := newBlockedSvc(s)

	// 另开一个共享同一内存库的连接，用来在复检进行时尝试修改教室容量。
	other := s.extraConn()

	pubErr := make(chan error, 1)
	go func() {
		_, e := svc.Publish(s.ctx, plan.ID, draft.Version.ID, publishOperator("alice"))
		pubErr <- e
	}()

	// 等发布事务进入复检（此时 BEGIN IMMEDIATE 已持有写锁）。
	waitFor(t, 2*time.Second, func() bool {
		select {
		case <-loader.entered:
			return true
		default:
			return false
		}
	}, "publish to enter in-transaction conflict check")

	// 并发修改容量：必须被挡住，在持锁期间无法完成。
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- other.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20).Error
	}()
	select {
	case e := <-updateDone:
		t.Fatalf("master-data change committed DURING conflict check (TOCTOU hole): update err=%v", e)
	case <-time.After(400 * time.Millisecond):
		// 期望：更新仍在等待写锁（busy_timeout=5s）。
	}

	// 复检通过后释放阻塞：发布完整提交；此时容量变更才获准执行。
	close(loader.release)
	select {
	case e := <-pubErr:
		if e != nil {
			t.Fatalf("publish should succeed after check: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish did not complete after release")
	}
	select {
	case e := <-updateDone:
		if e != nil {
			t.Fatalf("queued capacity update should succeed after publish commit: %v", e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("capacity update remained blocked after publish commit")
	}

	// 发布结果：版本已发布，应用的是“通过复检时”的快照（容量 50 时合法）。
	if got := s.versionStatus(plan.ID, draft.Version.ID); got != constants.VersionStatusPublished {
		t.Errorf("version status = %q, want published", got)
	}
	if got := s.currentVersionID(plan.ID); got != draft.Version.ID {
		t.Errorf("current version = %d, want %d", got, draft.Version.ID)
	}
	if got := len(s.liveSchedules()); got != 1 {
		t.Errorf("live schedules = %d, want 1 (full switch applied)", got)
	}
}

// 回滚 TOCTOU：回滚复检进行中并发改容量必须被写锁挡住。
func TestVM_RollbackTOCTOUMasterDataChangeBlocked(t *testing.T) {
	s := newVersionSuiteFile(t, true)
	plan := s.createPlan("回滚穿透方案")
	v1 := s.draft(plan.ID, s.buildDraftRequest("已发布版本", "alice", s.t1))
	if _, err := s.publish(plan.ID, v1.Version.ID, "alice"); err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	svc, loader := newBlockedSvc(s)
	other := s.extraConn()

	rbErr := make(chan error, 1)
	go func() {
		_, e := svc.Rollback(s.ctx, plan.ID, v1.Version.ID, rollbackOperator("bob"))
		rbErr <- e
	}()

	waitFor(t, 2*time.Second, func() bool {
		select {
		case <-loader.entered:
			return true
		default:
			return false
		}
	}, "rollback to enter in-transaction conflict check")

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- other.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20).Error
	}()
	select {
	case e := <-updateDone:
		t.Fatalf("master-data change committed DURING rollback check (TOCTOU hole): update err=%v", e)
	case <-time.After(400 * time.Millisecond):
	}

	close(loader.release)
	select {
	case e := <-rbErr:
		if e != nil {
			t.Fatalf("rollback should succeed after check: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rollback did not complete after release")
	}
	select {
	case e := <-updateDone:
		if e != nil {
			t.Fatalf("queued capacity update should succeed after rollback commit: %v", e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("capacity update remained blocked after rollback commit")
	}

	if got := len(s.liveSchedules()); got != 1 {
		t.Errorf("live schedules = %d, want 1 (rollback fully applied)", got)
	}
	versions, err := s.svc.ListVersions(s.ctx, plan.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("version count = %d, want 2 (original + rollback)", len(versions))
	}
	if versions[len(versions)-1].Status != constants.VersionStatusPublished {
		t.Errorf("rollback version status = %q, want published", versions[len(versions)-1].Status)
	}
}

// 原子性：若主数据在持锁窗口内已经是冲突态（容量提前变小），发布必须整体回滚：
// 版本状态、当前版本、在线课表三者要么全部切换，要么全部不变。
func TestVM_PublishAtomicityWhenConflictAtCommit(t *testing.T) {
	s := newVersionSuite(t)
	plan := s.createPlan("原子性方案")
	draft := s.draft(plan.ID, s.buildDraftRequest("待发布", "alice", s.t1))

	// 在开始发布前就让容量冲突存在：事务内复检必然发现冲突并整体回滚。
	if err := s.db.Model(&model.Classroom{}).Where("id = ?", s.roomID).Update("capacity", 20).Error; err != nil {
		t.Fatalf("shrink room: %v", err)
	}

	currentBefore := s.currentVersionID(plan.ID)
	_, err := s.publish(plan.ID, draft.Version.ID, "alice")
	if err == nil {
		t.Fatal("publish with commit-time conflict must be rejected")
	}
	if got := s.versionStatus(plan.ID, draft.Version.ID); got != constants.VersionStatusDraft {
		t.Errorf("version status = %q, want draft (no partial flip)", got)
	}
	if got := s.currentVersionID(plan.ID); got != currentBefore {
		t.Errorf("current version = %d, want unchanged %d", got, currentBefore)
	}
	if got := len(s.liveSchedules()); got != 0 {
		t.Errorf("live schedules = %d, want 0 (no partial timetable replacement)", got)
	}
}
