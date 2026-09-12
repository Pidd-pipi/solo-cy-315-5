package handler

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/gbschedule/gbschedule/internal/dto"
	"github.com/gbschedule/gbschedule/internal/service"
)

// VersionHandler handles schedule-plan version management endpoints.
type VersionHandler struct {
	service service.VersionService
	logger  *slog.Logger
}

// NewVersionHandler constructs a version handler.
func NewVersionHandler(service service.VersionService, logger *slog.Logger) *VersionHandler {
	return &VersionHandler{service: service, logger: logger}
}

// CreatePlan godoc
// @Summary Create a timetable plan
// @Tags plan-versions
// @Accept json
// @Produce json
// @Param input body dto.CreatePlanRequest true "plan payload"
// @Success 201 {object} dto.Response
// @Router /api/v1/plans [post]
func (h *VersionHandler) CreatePlan(c *gin.Context) {
	var req dto.CreatePlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}
	item, err := h.service.CreatePlan(c.Request.Context(), &req)
	if err != nil {
		Error(c, err)
		return
	}
	Created(c, item)
}

// ListPlans godoc
// @Summary List timetable plans
// @Tags plan-versions
// @Produce json
// @Param page query int false "page"
// @Param page_size query int false "page size"
// @Success 200 {object} dto.Response
// @Router /api/v1/plans [get]
func (h *VersionHandler) ListPlans(c *gin.Context) {
	var p dto.Pagination
	if err := c.ShouldBindQuery(&p); err != nil {
		BadRequest(c, "invalid pagination")
		return
	}
	p.Normalize()
	items, total, err := h.service.ListPlans(c.Request.Context(), p.Page, p.PageSize)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, dto.PageData{Items: items, Total: total, Page: p.Page, PageSize: p.PageSize})
}

// GetPlan godoc
// @Summary Get one timetable plan
// @Tags plan-versions
// @Produce json
// @Param plan_id path int true "plan id"
// @Success 200 {object} dto.Response
// @Router /api/v1/plans/{plan_id} [get]
func (h *VersionHandler) GetPlan(c *gin.Context) {
	planID, ok := parseID(c, "plan_id")
	if !ok {
		return
	}
	item, err := h.service.GetPlan(c.Request.Context(), planID)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, item)
}

// CreateDraft godoc
// @Summary Generate a named draft version
// @Tags plan-versions
// @Accept json
// @Produce json
// @Param plan_id path int true "plan id"
// @Param input body dto.CreateDraftRequest true "draft payload"
// @Success 201 {object} dto.Response
// @Router /api/v1/plans/{plan_id}/versions/drafts [post]
func (h *VersionHandler) CreateDraft(c *gin.Context) {
	planID, ok := parseID(c, "plan_id")
	if !ok {
		return
	}
	var req dto.CreateDraftRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}
	item, err := h.service.CreateDraft(c.Request.Context(), planID, &req)
	if err != nil {
		Error(c, err)
		return
	}
	Created(c, item)
}

// ListVersions godoc
// @Summary List versions of a plan
// @Tags plan-versions
// @Produce json
// @Param plan_id path int true "plan id"
// @Success 200 {object} dto.Response
// @Router /api/v1/plans/{plan_id}/versions [get]
func (h *VersionHandler) ListVersions(c *gin.Context) {
	planID, ok := parseID(c, "plan_id")
	if !ok {
		return
	}
	items, err := h.service.ListVersions(c.Request.Context(), planID)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, items)
}

// GetVersion godoc
// @Summary Get one version with its snapshot
// @Tags plan-versions
// @Produce json
// @Param plan_id path int true "plan id"
// @Param version_id path int true "version id"
// @Success 200 {object} dto.Response
// @Router /api/v1/plans/{plan_id}/versions/{version_id} [get]
func (h *VersionHandler) GetVersion(c *gin.Context) {
	planID, versionID, ok := parsePlanVersionIDs(c)
	if !ok {
		return
	}
	item, err := h.service.GetVersion(c.Request.Context(), planID, versionID)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, item)
}

// Compare godoc
// @Summary Compare two versions of the same plan
// @Tags plan-versions
// @Accept json
// @Produce json
// @Param plan_id path int true "plan id"
// @Param input body dto.CompareVersionsRequest true "compare payload"
// @Success 200 {object} dto.Response
// @Router /api/v1/plans/{plan_id}/versions/compare [post]
func (h *VersionHandler) Compare(c *gin.Context) {
	planID, ok := parseID(c, "plan_id")
	if !ok {
		return
	}
	var req dto.CompareVersionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}
	item, err := h.service.Compare(c.Request.Context(), planID, &req)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, item)
}

// Publish godoc
// @Summary Publish a draft version (conflicts are re-checked)
// @Tags plan-versions
// @Accept json
// @Produce json
// @Param plan_id path int true "plan id"
// @Param version_id path int true "version id"
// @Param input body dto.PublishVersionRequest true "publish payload"
// @Success 200 {object} dto.Response
// @Failure 409 {object} dto.Response "conflicts rejected the release"
// @Router /api/v1/plans/{plan_id}/versions/{version_id}/publish [post]
func (h *VersionHandler) Publish(c *gin.Context) {
	planID, versionID, ok := parsePlanVersionIDs(c)
	if !ok {
		return
	}
	var req dto.PublishVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}
	item, err := h.service.Publish(c.Request.Context(), planID, versionID, &req)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, item)
}

// Rollback godoc
// @Summary Rollback to a previously published version
// @Tags plan-versions
// @Accept json
// @Produce json
// @Param plan_id path int true "plan id"
// @Param version_id path int true "target published version id"
// @Param input body dto.RollbackVersionRequest true "rollback payload"
// @Success 200 {object} dto.Response
// @Router /api/v1/plans/{plan_id}/versions/{version_id}/rollback [post]
func (h *VersionHandler) Rollback(c *gin.Context) {
	planID, versionID, ok := parsePlanVersionIDs(c)
	if !ok {
		return
	}
	var req dto.RollbackVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}
	item, err := h.service.Rollback(c.Request.Context(), planID, versionID, &req)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, item)
}

// OperationLogs godoc
// @Summary List version-management audit logs of a plan
// @Tags plan-versions
// @Produce json
// @Param plan_id path int true "plan id"
// @Param page query int false "page"
// @Param page_size query int false "page size"
// @Success 200 {object} dto.Response
// @Router /api/v1/plans/{plan_id}/operation-logs [get]
func (h *VersionHandler) OperationLogs(c *gin.Context) {
	planID, ok := parseID(c, "plan_id")
	if !ok {
		return
	}
	var p dto.Pagination
	if err := c.ShouldBindQuery(&p); err != nil {
		BadRequest(c, "invalid pagination")
		return
	}
	p.Normalize()
	items, total, err := h.service.ListOperationLogs(c.Request.Context(), planID, p.Page, p.PageSize)
	if err != nil {
		Error(c, err)
		return
	}
	OK(c, dto.PageData{Items: items, Total: total, Page: p.Page, PageSize: p.PageSize})
}

func parsePlanVersionIDs(c *gin.Context) (uint, uint, bool) {
	planID, ok := parseID(c, "plan_id")
	if !ok {
		return 0, 0, false
	}
	versionID, ok := parseID(c, "version_id")
	if !ok {
		return 0, 0, false
	}
	return planID, versionID, true
}
