package repository

import (
	"context"
	"fmt"

	"github.com/gbschedule/gbschedule/internal/model"
	"gorm.io/gorm"
)

// PlanRepository persists timetable plans, their versions and audit logs.
type PlanRepository interface {
	CreatePlan(ctx context.Context, plan *model.SchedulePlan) error
	UpdatePlanTx(ctx context.Context, tx *gorm.DB, plan *model.SchedulePlan) error
	GetPlan(ctx context.Context, id uint) (*model.SchedulePlan, error)
	GetPlanTx(ctx context.Context, tx *gorm.DB, id uint) (*model.SchedulePlan, error)
	ListPlans(ctx context.Context, page, pageSize int) ([]model.SchedulePlan, int64, error)

	CreateVersion(ctx context.Context, version *model.ScheduleVersion) error
	CreateVersionTx(ctx context.Context, tx *gorm.DB, version *model.ScheduleVersion) error
	GetVersion(ctx context.Context, id uint) (*model.ScheduleVersion, error)
	GetVersionTx(ctx context.Context, tx *gorm.DB, id uint) (*model.ScheduleVersion, error)
	ListVersions(ctx context.Context, planID uint) ([]model.ScheduleVersion, error)
	CountVersions(ctx context.Context, planID uint) (int64, error)
	UpdateVersionTx(ctx context.Context, tx *gorm.DB, version *model.ScheduleVersion) error

	CreateOperationLog(ctx context.Context, log *model.VersionOperationLog) error
	CreateOperationLogTx(ctx context.Context, tx *gorm.DB, log *model.VersionOperationLog) error
	ListOperationLogs(ctx context.Context, planID uint, page, pageSize int) ([]model.VersionOperationLog, int64, error)

	// Transaction runs fn inside a database transaction.
	Transaction(ctx context.Context, fn func(tx *gorm.DB) error) error
}

type planRepository struct {
	db *gorm.DB
}

// NewPlanRepository constructs a plan repository.
func NewPlanRepository(db *gorm.DB) PlanRepository {
	return &planRepository{db: db}
}

func (r *planRepository) CreatePlan(ctx context.Context, plan *model.SchedulePlan) error {
	if err := r.db.WithContext(ctx).Create(plan).Error; err != nil {
		return fmt.Errorf("create plan: %w", err)
	}
	return nil
}

func (r *planRepository) UpdatePlanTx(ctx context.Context, tx *gorm.DB, plan *model.SchedulePlan) error {
	if err := tx.WithContext(ctx).Save(plan).Error; err != nil {
		return fmt.Errorf("update plan: %w", err)
	}
	return nil
}

func (r *planRepository) GetPlan(ctx context.Context, id uint) (*model.SchedulePlan, error) {
	return r.GetPlanTx(ctx, r.db.WithContext(ctx), id)
}

func (r *planRepository) GetPlanTx(ctx context.Context, tx *gorm.DB, id uint) (*model.SchedulePlan, error) {
	var plan model.SchedulePlan
	if err := tx.WithContext(ctx).First(&plan, id).Error; err != nil {
		return nil, normalizeError(err)
	}
	return &plan, nil
}

func (r *planRepository) ListPlans(ctx context.Context, page, pageSize int) ([]model.SchedulePlan, int64, error) {
	var items []model.SchedulePlan
	var total int64
	if err := r.db.WithContext(ctx).Model(&model.SchedulePlan{}).Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count plans: %w", err)
	}
	if err := paginate(r.db.WithContext(ctx).Model(&model.SchedulePlan{}), page, pageSize).
		Order("id DESC").Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("list plans: %w", err)
	}
	return items, total, nil
}

func (r *planRepository) CreateVersion(ctx context.Context, version *model.ScheduleVersion) error {
	return r.CreateVersionTx(ctx, r.db.WithContext(ctx), version)
}

func (r *planRepository) CreateVersionTx(ctx context.Context, tx *gorm.DB, version *model.ScheduleVersion) error {
	if err := tx.WithContext(ctx).Create(version).Error; err != nil {
		if isConstraintError(err) {
			return ErrConstraint
		}
		return fmt.Errorf("create version: %w", err)
	}
	return nil
}

func (r *planRepository) GetVersion(ctx context.Context, id uint) (*model.ScheduleVersion, error) {
	return r.GetVersionTx(ctx, r.db.WithContext(ctx), id)
}

func (r *planRepository) GetVersionTx(ctx context.Context, tx *gorm.DB, id uint) (*model.ScheduleVersion, error) {
	var version model.ScheduleVersion
	if err := tx.WithContext(ctx).First(&version, id).Error; err != nil {
		return nil, normalizeError(err)
	}
	return &version, nil
}

func (r *planRepository) ListVersions(ctx context.Context, planID uint) ([]model.ScheduleVersion, error) {
	var items []model.ScheduleVersion
	if err := r.db.WithContext(ctx).
		Where("plan_id = ?", planID).
		Order("version_no ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	return items, nil
}

func (r *planRepository) CountVersions(ctx context.Context, planID uint) (int64, error) {
	var total int64
	if err := r.db.WithContext(ctx).Model(&model.ScheduleVersion{}).
		Where("plan_id = ?", planID).Count(&total).Error; err != nil {
		return 0, fmt.Errorf("count versions: %w", err)
	}
	return total, nil
}

func (r *planRepository) UpdateVersionTx(ctx context.Context, tx *gorm.DB, version *model.ScheduleVersion) error {
	if err := tx.WithContext(ctx).Save(version).Error; err != nil {
		return fmt.Errorf("update version: %w", err)
	}
	return nil
}

func (r *planRepository) CreateOperationLog(ctx context.Context, log *model.VersionOperationLog) error {
	return r.CreateOperationLogTx(ctx, r.db.WithContext(ctx), log)
}

func (r *planRepository) CreateOperationLogTx(ctx context.Context, tx *gorm.DB, log *model.VersionOperationLog) error {
	if err := tx.WithContext(ctx).Create(log).Error; err != nil {
		return fmt.Errorf("create operation log: %w", err)
	}
	return nil
}

func (r *planRepository) ListOperationLogs(ctx context.Context, planID uint, page, pageSize int) ([]model.VersionOperationLog, int64, error) {
	var items []model.VersionOperationLog
	var total int64
	base := func() *gorm.DB {
		return r.db.WithContext(ctx).Model(&model.VersionOperationLog{}).Where("plan_id = ?", planID)
	}
	if err := base().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count operation logs: %w", err)
	}
	if err := paginate(base(), page, pageSize).Order("id DESC").Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("list operation logs: %w", err)
	}
	return items, total, nil
}

func (r *planRepository) Transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(fn)
}
