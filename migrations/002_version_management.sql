-- 教室排课助手 version-management schema (SQLite)
-- 方案 -> 版本(草稿/已发布) -> 操作审计日志
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schedule_plans (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at DATETIME,
    updated_at DATETIME,
    deleted_at DATETIME,
    name TEXT NOT NULL,
    description TEXT,
    current_version_id INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_schedule_plans_current_version ON schedule_plans(current_version_id);

CREATE TABLE IF NOT EXISTS schedule_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at DATETIME,
    updated_at DATETIME,
    deleted_at DATETIME,
    plan_id INTEGER NOT NULL,
    version_no INTEGER NOT NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL,              -- draft | published
    snapshot TEXT NOT NULL,            -- JSON array of lessons
    lesson_count INTEGER NOT NULL DEFAULT 0,
    semester TEXT,
    weeks INTEGER,
    days_per_week INTEGER,
    published_by TEXT,
    published_at DATETIME,
    created_by TEXT NOT NULL,
    remark TEXT
);
CREATE INDEX IF NOT EXISTS idx_schedule_versions_plan ON schedule_versions(plan_id);
CREATE INDEX IF NOT EXISTS idx_schedule_versions_status ON schedule_versions(status);
-- 同一方案内版本号唯一
CREATE UNIQUE INDEX IF NOT EXISTS uniq_plan_version_no
    ON schedule_versions(plan_id, version_no);

CREATE TABLE IF NOT EXISTS version_operation_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at DATETIME,
    updated_at DATETIME,
    deleted_at DATETIME,
    plan_id INTEGER NOT NULL,
    version_id INTEGER,
    action TEXT NOT NULL,             -- plan_create/version_draft/version_compare/version_publish/version_rollback/version_rejected
    operator TEXT NOT NULL,
    detail TEXT
);
CREATE INDEX IF NOT EXISTS idx_version_operation_logs_plan ON version_operation_logs(plan_id);
CREATE INDEX IF NOT EXISTS idx_version_operation_logs_version ON version_operation_logs(version_id);
