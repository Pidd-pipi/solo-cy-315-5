package constants

// Version lifecycle states. A version starts as a draft; once published it
// keeps the "published" state forever — there can be several historical
// releases of one plan and the active release is tracked on the plan itself
// via current_version_id.
const (
	VersionStatusDraft     = "draft"
	VersionStatusPublished = "published"
)

// Version-management audit actions.
const (
	ActionPlanCreate      = "plan_create"
	ActionVersionDraft    = "version_draft"
	ActionVersionCompare  = "version_compare"
	ActionVersionPublish  = "version_publish"
	ActionVersionReject   = "version_rejected"
	ActionVersionRollback = "version_rollback"
)
