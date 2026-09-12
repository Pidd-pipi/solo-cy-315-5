package service

import (
	"context"
	"fmt"

	"github.com/gbschedule/gbschedule/internal/constants"
	"github.com/gbschedule/gbschedule/internal/dto"
	"github.com/gbschedule/gbschedule/internal/model"
	"github.com/gbschedule/gbschedule/internal/repository"
	"gorm.io/gorm"
)

// Planner runs the greedy scheduling algorithm and conflict detection
// purely in memory. It never touches the schedules table, so it can be used
// both for the live "generate" flow and for storing version snapshots.
type Planner interface {
	// Plan computes a timetable for the request. Placement conflicts describe
	// course requirements the greedy algorithm could not place at all.
	Plan(ctx context.Context, req *dto.GenerateScheduleRequest) (items []model.Schedule, placementConflicts []dto.ConflictResponse, required int, err error)
	// DetectConflicts re-validates lessons against current master data
	// (teacher/class/classroom occupancy, room capacity, teacher preferences).
	// Master-data lookup failures are returned as errors: callers must treat
	// them as internal failures, never as "no conflict" or "reference missing".
	DetectConflicts(ctx context.Context, items []model.Schedule) ([]dto.ConflictResponse, error)
	// DetectConflictsTx is the transaction-scoped variant: master data is read
	// through tx, so a publish/rollback can re-validate inside the same write
	// transaction that applies the timetable, closing the check-then-act race.
	DetectConflictsTx(ctx context.Context, tx *gorm.DB, items []model.Schedule) ([]dto.ConflictResponse, error)
	// Enrich fills related names for timetable entries.
	Enrich(ctx context.Context, items []model.Schedule) ([]dto.ScheduleResponse, error)
}

type planner struct {
	db         *gorm.DB
	classrooms repository.ClassroomRepository
	teachers   repository.TeacherRepository
	classes    repository.ClassRepository
	courses    repository.CourseRepository
	timeSlots  repository.TimeSlotRepository
	loader     MasterDataLoader
}

// Option customizes a planner, used mainly to inject a faulty/blocking
// master-data loader in tests.
type Option func(*planner)

// WithMasterDataLoader overrides the default GORM-backed master-data loader.
func WithMasterDataLoader(loader MasterDataLoader) Option {
	return func(p *planner) { p.loader = loader }
}

// NewPlanner constructs the in-memory scheduling planner. db is used for
// transaction-scoped conflict detection; the repositories serve planning and
// enrichment outside transactions.
func NewPlanner(
	db *gorm.DB,
	classrooms repository.ClassroomRepository,
	teachers repository.TeacherRepository,
	classes repository.ClassRepository,
	courses repository.CourseRepository,
	timeSlots repository.TimeSlotRepository,
	opts ...Option,
) Planner {
	p := &planner{
		db:         db,
		classrooms: classrooms,
		teachers:   teachers,
		classes:    classes,
		courses:    courses,
		timeSlots:  timeSlots,
		loader:     gormMasterDataLoader{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Plan runs the greedy scheduling algorithm without persisting anything.
func (p *planner) Plan(ctx context.Context, req *dto.GenerateScheduleRequest) ([]model.Schedule, []dto.ConflictResponse, int, error) {
	allSlots, _, err := p.timeSlots.List(ctx, 1, constants.MaxPageSize)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load time slots: %w", err)
	}
	if req.PeriodsPerDay > len(allSlots) {
		return nil, nil, 0, fmt.Errorf("generate schedule: %w: periods_per_day %d exceeds available time slots %d", ErrInvalid, req.PeriodsPerDay, len(allSlots))
	}
	slots := allSlots[:req.PeriodsPerDay]

	courses, err := p.courses.GetByIDs(ctx, requirementCourseIDs(req.Courses))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load courses: %w", err)
	}
	courseMap := entityMap(courses, func(c model.Course) uint { return c.ID })

	classes, err := p.resolveClasses(ctx, req)
	if err != nil {
		return nil, nil, 0, err
	}
	teachers, err := p.resolveTeachers(ctx, req)
	if err != nil {
		return nil, nil, 0, err
	}
	classrooms, err := p.resolveClassrooms(ctx, req)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(teachers) == 0 || len(classes) == 0 || len(classrooms) == 0 {
		return nil, nil, 0, fmt.Errorf("generate schedule: %w: teachers, classes and classrooms must not be empty", ErrInvalid)
	}

	teacherCursor := 0
	var allSchedules []model.Schedule
	var conflicts []dto.ConflictResponse
	required := 0

	for week := 1; week <= req.Weeks; week++ {
		occ := newOccupancy()
		for _, requirement := range req.Courses {
			targetClasses := targetClassesForRequirement(classes, requirement, req.ClassIDs)
			for _, class := range targetClasses {
				teacher := pickTeacher(teachers, requirement, courses, courseMap, &teacherCursor)
				if teacher == nil {
					continue
				}
				course, ok := courseMap[requirement.CourseID]
				if !ok {
					conflicts = append(conflicts, dto.ConflictResponse{
						Type: constants.ConflictTeacherTime, EntityType: "course", EntityID: requirement.CourseID,
						EntityName: fmt.Sprintf("course %d", requirement.CourseID), Week: uint(week),
						Suggestion: "course not found",
					})
					continue
				}
				required += requirement.WeeklyPeriods

				positions := buildCandidatePositions(req.DaysPerWeek, slots, requirement.Consecutive, requirement.WeeklyPeriods)
				chosen, chosenClassrooms, ok := placeGreedy(uint(week), positions, requirement.WeeklyPeriods, occ, class, *teacher, course, classrooms, slots)
				if !ok {
					conflicts = append(conflicts, dto.ConflictResponse{
						Type:       constants.ConflictTeacherTime,
						EntityType: "course",
						EntityID:   requirement.CourseID,
						EntityName: course.Name,
						Week:       uint(week),
						Suggestion: fmt.Sprintf("unable to place %d periods for course %s in week %d; add teachers/classrooms or relax constraints", requirement.WeeklyPeriods, course.Name, week),
					})
					continue
				}
				for i := range chosen {
					allSchedules = append(allSchedules, model.Schedule{
						Week:        uint(week),
						DayOfWeek:   chosen[i].Day,
						TimeSlotID:  chosen[i].Slot.ID,
						ClassroomID: chosenClassrooms[i].ID,
						TeacherID:   teacher.ID,
						ClassID:     class.ID,
						CourseID:    course.ID,
					})
				}
			}
		}
	}
	return allSchedules, conflicts, required, nil
}

func (p *planner) Enrich(ctx context.Context, items []model.Schedule) ([]dto.ScheduleResponse, error) {
	timeSlots, _, err := p.timeSlots.List(ctx, 1, constants.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("load time slots: %w", err)
	}
	slotMap := map[uint]model.TimeSlot{}
	for i := range timeSlots {
		slotMap[timeSlots[i].ID] = timeSlots[i]
	}
	classroomList, err := p.classrooms.GetByIDs(ctx, uniqueClassroomIDs(items))
	if err != nil {
		return nil, err
	}
	teacherList, err := p.teachers.GetByIDs(ctx, uniqueTeacherIDs(items))
	if err != nil {
		return nil, err
	}
	classList, err := p.classes.GetByIDs(ctx, uniqueClassIDs(items))
	if err != nil {
		return nil, err
	}
	courseList, err := p.courses.GetByIDs(ctx, uniqueUint(items, func(s model.Schedule) uint { return s.CourseID }))
	if err != nil {
		return nil, err
	}
	return enrichSchedules(
		items,
		slotMap,
		entityMap(classroomList, func(c model.Classroom) uint { return c.ID }),
		entityMap(teacherList, func(t model.Teacher) uint { return t.ID }),
		entityMap(classList, func(c model.Class) uint { return c.ID }),
		entityMap(courseList, func(c model.Course) uint { return c.ID }),
	), nil
}

func (p *planner) resolveClasses(ctx context.Context, req *dto.GenerateScheduleRequest) ([]model.Class, error) {
	if len(req.ClassIDs) > 0 {
		items, err := p.classes.GetByIDs(ctx, req.ClassIDs)
		if err != nil {
			return nil, fmt.Errorf("load requested classes: %w", err)
		}
		return items, nil
	}
	ids := make([]uint, 0)
	for _, r := range req.Courses {
		if r.ClassID != 0 {
			ids = append(ids, r.ClassID)
		}
	}
	if len(ids) > 0 {
		items, err := p.classes.GetByIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("load requirement classes: %w", err)
		}
		return items, nil
	}
	items, _, err := p.classes.List(ctx, 1, constants.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("load all classes: %w", err)
	}
	return items, nil
}

func (p *planner) resolveTeachers(ctx context.Context, req *dto.GenerateScheduleRequest) ([]model.Teacher, error) {
	if len(req.TeacherIDs) > 0 {
		items, err := p.teachers.GetByIDs(ctx, req.TeacherIDs)
		if err != nil {
			return nil, fmt.Errorf("load requested teachers: %w", err)
		}
		return items, nil
	}
	ids := make([]uint, 0)
	for _, r := range req.Courses {
		if r.TeacherID != 0 {
			ids = append(ids, r.TeacherID)
		}
	}
	if len(ids) > 0 {
		items, err := p.teachers.GetByIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("load requirement teachers: %w", err)
		}
		return items, nil
	}
	items, _, err := p.teachers.List(ctx, 1, constants.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("load all teachers: %w", err)
	}
	return items, nil
}

func (p *planner) resolveClassrooms(ctx context.Context, req *dto.GenerateScheduleRequest) ([]model.Classroom, error) {
	if len(req.ClassroomIDs) > 0 {
		items, err := p.classrooms.GetByIDs(ctx, req.ClassroomIDs)
		if err != nil {
			return nil, fmt.Errorf("load requested classrooms: %w", err)
		}
		return items, nil
	}
	items, _, err := p.classrooms.List(ctx, 1, constants.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("load all classrooms: %w", err)
	}
	return items, nil
}

func (p *planner) DetectConflicts(ctx context.Context, items []model.Schedule) ([]dto.ConflictResponse, error) {
	return p.detectConflicts(ctx, p.db.WithContext(ctx), items)
}

func (p *planner) DetectConflictsTx(ctx context.Context, tx *gorm.DB, items []model.Schedule) ([]dto.ConflictResponse, error) {
	return p.detectConflicts(ctx, tx.WithContext(ctx), items)
}

// DetectConflicts checks teacher/class/classroom time overlaps, classroom
// capacity, teacher slot preferences and dangling references to master data
// that has changed since a snapshot was taken. It works on both stored
// lessons (which have IDs) and snapshot lessons (which do not), by comparing
// slice positions rather than record IDs.
//
// Master data is read through the supplied executor (a plain connection or a
// write transaction), and any lookup failure is returned. When called inside
// an IMMEDIATE transaction the data seen here is the same data that will be
// valid at commit, so a concurrent master-data change cannot sneak in between
// the check and applying the timetable.
func (p *planner) detectConflicts(ctx context.Context, exec *gorm.DB, items []model.Schedule) ([]dto.ConflictResponse, error) {
	var conflicts []dto.ConflictResponse
	teacherSlots := map[string]int{}
	classSlots := map[string]int{}
	classroomSlots := map[string]int{}

	classes, err := p.loader.LoadClasses(ctx, exec, uniqueClassIDs(items))
	if err != nil {
		return nil, fmt.Errorf("load classes for conflict check: %w", err)
	}
	classroomList, err := p.loader.LoadClassrooms(ctx, exec, uniqueClassroomIDs(items))
	if err != nil {
		return nil, fmt.Errorf("load classrooms for conflict check: %w", err)
	}
	teacherList, err := p.loader.LoadTeachers(ctx, exec, uniqueTeacherIDs(items))
	if err != nil {
		return nil, fmt.Errorf("load teachers for conflict check: %w", err)
	}
	courseList, err := p.loader.LoadCourses(ctx, exec, uniqueUint(items, func(s model.Schedule) uint { return s.CourseID }))
	if err != nil {
		return nil, fmt.Errorf("load courses for conflict check: %w", err)
	}
	slotList, err := p.loader.LoadTimeSlots(ctx, exec)
	if err != nil {
		return nil, fmt.Errorf("load time slots for conflict check: %w", err)
	}

	classMap := map[uint]model.Class{}
	for i := range classes {
		classMap[classes[i].ID] = classes[i]
	}
	classroomMap := map[uint]model.Classroom{}
	for i := range classroomList {
		classroomMap[classroomList[i].ID] = classroomList[i]
	}
	teacherMap := map[uint]model.Teacher{}
	for i := range teacherList {
		teacherMap[teacherList[i].ID] = teacherList[i]
	}
	courseMap := map[uint]model.Course{}
	for i := range courseList {
		courseMap[courseList[i].ID] = courseList[i]
	}
	slotMap := map[uint]model.TimeSlot{}
	for i := range slotList {
		slotMap[slotList[i].ID] = slotList[i]
	}

	for idx := range items {
		item := items[idx]

		// A snapshot may reference master data deleted after the version was
		// created. Flag every dangling reference so publish/rollback can
		// reject a snapshot that is no longer applicable.
		if _, ok := teacherMap[item.TeacherID]; !ok {
			conflicts = append(conflicts, missingReferenceConflict("teacher", item.TeacherID, item,
				fmt.Sprintf("teacher %d referenced by the version no longer exists; update the timetable before publishing", item.TeacherID)))
		}
		if _, ok := classMap[item.ClassID]; !ok {
			conflicts = append(conflicts, missingReferenceConflict("class", item.ClassID, item,
				fmt.Sprintf("class %d referenced by the version no longer exists; update the timetable before publishing", item.ClassID)))
		}
		if _, ok := classroomMap[item.ClassroomID]; !ok {
			conflicts = append(conflicts, missingReferenceConflict("classroom", item.ClassroomID, item,
				fmt.Sprintf("classroom %d referenced by the version no longer exists; update the timetable before publishing", item.ClassroomID)))
		}
		if _, ok := courseMap[item.CourseID]; !ok {
			conflicts = append(conflicts, missingReferenceConflict("course", item.CourseID, item,
				fmt.Sprintf("course %d referenced by the version no longer exists; update the timetable before publishing", item.CourseID)))
		}
		if _, ok := slotMap[item.TimeSlotID]; !ok {
			conflicts = append(conflicts, missingReferenceConflict("time_slot", item.TimeSlotID, item,
				fmt.Sprintf("time slot %d referenced by the version no longer exists; update the timetable before publishing", item.TimeSlotID)))
		}

		slotKey := fmt.Sprintf("%d-%d-%d", item.Week, item.DayOfWeek, item.TimeSlotID)
		teacherKey := slotKey + "-t-" + fmt.Sprint(item.TeacherID)
		classKey := slotKey + "-c-" + fmt.Sprint(item.ClassID)
		classroomKey := slotKey + "-r-" + fmt.Sprint(item.ClassroomID)
		if existing, ok := teacherSlots[teacherKey]; ok && existing != idx {
			teacher := teacherMap[item.TeacherID]
			conflicts = append(conflicts, dto.ConflictResponse{
				Type: constants.ConflictTeacherTime, EntityType: "teacher", EntityID: item.TeacherID,
				EntityName: teacher.Name, Week: item.Week, DayOfWeek: item.DayOfWeek, TimeSlotID: item.TimeSlotID,
				Suggestion: fmt.Sprintf("teacher already has a lesson at week %d day %d slot %d; move one of the lessons", item.Week, item.DayOfWeek, item.TimeSlotID),
			})
		}
		if existing, ok := classSlots[classKey]; ok && existing != idx {
			class := classMap[item.ClassID]
			conflicts = append(conflicts, dto.ConflictResponse{
				Type: constants.ConflictClassTime, EntityType: "class", EntityID: item.ClassID,
				EntityName: class.Name, Week: item.Week, DayOfWeek: item.DayOfWeek, TimeSlotID: item.TimeSlotID,
				Suggestion: fmt.Sprintf("class already has a lesson at week %d day %d slot %d; move one of the lessons", item.Week, item.DayOfWeek, item.TimeSlotID),
			})
		}
		if existing, ok := classroomSlots[classroomKey]; ok && existing != idx {
			classroom := classroomMap[item.ClassroomID]
			conflicts = append(conflicts, dto.ConflictResponse{
				Type: constants.ConflictClassroomTime, EntityType: "classroom", EntityID: item.ClassroomID,
				EntityName: classroom.Name, Week: item.Week, DayOfWeek: item.DayOfWeek, TimeSlotID: item.TimeSlotID,
				Suggestion: fmt.Sprintf("classroom already has a lesson at week %d day %d slot %d; move one of the lessons", item.Week, item.DayOfWeek, item.TimeSlotID),
			})
		}
		if class, ok := classMap[item.ClassID]; ok {
			if classroom, ok2 := classroomMap[item.ClassroomID]; ok2 && class.StudentCount > classroom.Capacity {
				conflicts = append(conflicts, dto.ConflictResponse{
					Type: constants.ConflictClassroomCap, EntityType: "class", EntityID: item.ClassID,
					EntityName: class.Name, Week: item.Week, DayOfWeek: item.DayOfWeek, TimeSlotID: item.TimeSlotID,
					Suggestion: fmt.Sprintf("class size %d exceeds classroom capacity %d; choose a larger classroom", class.StudentCount, classroom.Capacity),
				})
			}
		}
		if teacher, ok := teacherMap[item.TeacherID]; ok {
			if slot, ok2 := slotMap[item.TimeSlotID]; ok2 && contains(teacher.UnavailableSlots, slot.Code) {
				conflicts = append(conflicts, dto.ConflictResponse{
					Type: constants.ConflictTeacherPref, EntityType: "teacher", EntityID: item.TeacherID,
					EntityName: teacher.Name, Week: item.Week, DayOfWeek: item.DayOfWeek, TimeSlotID: item.TimeSlotID,
					Suggestion: fmt.Sprintf("slot %s is in the teacher's unavailable periods; choose another time", slot.Code),
				})
			}
		}
		teacherSlots[teacherKey] = idx
		classSlots[classKey] = idx
		classroomSlots[classroomKey] = idx
	}
	return conflicts, nil
}

func missingReferenceConflict(entityType string, entityID uint, item model.Schedule, suggestion string) dto.ConflictResponse {
	return dto.ConflictResponse{
		Type:       constants.ConflictReferenceMissing,
		EntityType: entityType,
		EntityID:   entityID,
		Week:       item.Week,
		DayOfWeek:  item.DayOfWeek,
		TimeSlotID: item.TimeSlotID,
		Suggestion: suggestion,
	}
}
