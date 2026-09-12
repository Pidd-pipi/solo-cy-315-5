package service

import (
	"context"

	"github.com/gbschedule/gbschedule/internal/constants"
	"github.com/gbschedule/gbschedule/internal/model"
	"gorm.io/gorm"
)

// MasterDataLoader reads the master data needed for conflict validation.
// Every method receives the current executor so the same implementation works
// on a plain connection or inside a write transaction; passing the
// transaction's *gorm.DB is what makes the publish/rollback re-check read its
// own locked snapshot. It is exported so tests can inject a fault/blocking
// loader via WithMasterDataLoader.
type MasterDataLoader interface {
	LoadClasses(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Class, error)
	LoadClassrooms(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Classroom, error)
	LoadTeachers(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Teacher, error)
	LoadCourses(ctx context.Context, exec *gorm.DB, ids []uint) ([]model.Course, error)
	LoadTimeSlots(ctx context.Context, exec *gorm.DB) ([]model.TimeSlot, error)
}

// NewGormMasterDataLoader returns the production GORM-backed loader.
func NewGormMasterDataLoader() MasterDataLoader { return gormMasterDataLoader{} }

// gormMasterDataLoader is the production loader backed by GORM.
type gormMasterDataLoader struct{}

func (gormMasterDataLoader) LoadClasses(_ context.Context, exec *gorm.DB, ids []uint) ([]model.Class, error) {
	var out []model.Class
	if err := findByIDs(exec, &out, ids); err != nil {
		return nil, err
	}
	return out, nil
}

func (gormMasterDataLoader) LoadClassrooms(_ context.Context, exec *gorm.DB, ids []uint) ([]model.Classroom, error) {
	var out []model.Classroom
	if err := findByIDs(exec, &out, ids); err != nil {
		return nil, err
	}
	return out, nil
}

func (gormMasterDataLoader) LoadTeachers(_ context.Context, exec *gorm.DB, ids []uint) ([]model.Teacher, error) {
	var out []model.Teacher
	if err := findByIDs(exec, &out, ids); err != nil {
		return nil, err
	}
	return out, nil
}

func (gormMasterDataLoader) LoadCourses(_ context.Context, exec *gorm.DB, ids []uint) ([]model.Course, error) {
	var out []model.Course
	if err := findByIDs(exec, &out, ids); err != nil {
		return nil, err
	}
	return out, nil
}

func (gormMasterDataLoader) LoadTimeSlots(_ context.Context, exec *gorm.DB) ([]model.TimeSlot, error) {
	var out []model.TimeSlot
	if err := exec.Limit(constants.MaxPageSize).Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func findByIDs(exec *gorm.DB, dest any, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return exec.Where("id IN ?", ids).Find(dest).Error
}
