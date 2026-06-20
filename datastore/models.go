package datastore

import "time"

// GORM model structs for the cluster agent's persistent tables. A package-wide
// NamingStrategy (TablePrefix "cluster_agent_", set in Initialize) gives every
// table the physical name cluster_agent_<gorm-default>, so the agent can share
// the runos Postgres database with other RunOS components without colliding.
//
// Column shapes are pinned to the original SQLite schema: NOT NULL, UNIQUE,
// DEFAULT, and index definitions are reproduced via struct tags so AutoMigrate
// builds an equivalent schema. Integer autoincrement primary keys become a
// `uint` ID with GORM autoincrement. DATETIME columns become time.Time
// (pointer where the column was nullable). The created_at/updated_at columns
// keep DB-side CURRENT_TIMESTAMP defaults so insert/update semantics match the
// old hand-written SQL that relied on them.

// BuildKitJobModel maps the buildkit_jobs table
// (physical: cluster_agent_buildkit_jobs).
type BuildKitJobModel struct {
	ID          uint       `gorm:"primaryKey;autoIncrement"`
	JobID       string     `gorm:"column:job_id;uniqueIndex:idx_buildkit_jobs_job_id;not null"`
	OSID        string     `gorm:"column:osid;not null"`
	Repo        string     `gorm:"column:repo;not null"`
	Branch      string     `gorm:"column:branch;not null"`
	CommitHash  string     `gorm:"column:commit_hash;not null"`
	Status      string     `gorm:"column:status;index:idx_buildkit_jobs_status;not null"`
	CreatedAt   time.Time  `gorm:"column:created_at;index:idx_buildkit_jobs_created_at;default:CURRENT_TIMESTAMP"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;default:CURRENT_TIMESTAMP"`
	CompletedAt *time.Time `gorm:"column:completed_at"`
}

// TableName pins the unprefixed table name; the prefix is applied by the
// NamingStrategy, yielding cluster_agent_buildkit_jobs.
func (BuildKitJobModel) TableName() string { return "buildkit_jobs" }

// BuildKitLogModel maps the buildkit_logs table
// (physical: cluster_agent_buildkit_logs).
type BuildKitLogModel struct {
	ID        uint      `gorm:"primaryKey;autoIncrement"`
	JobID     string    `gorm:"column:job_id;index:idx_buildkit_logs_job_id;not null"`
	LogEntry  string    `gorm:"column:log_entry;not null"`
	CreatedAt time.Time `gorm:"column:created_at;index:idx_buildkit_logs_created_at;default:CURRENT_TIMESTAMP"`
}

// TableName -> cluster_agent_buildkit_logs.
func (BuildKitLogModel) TableName() string { return "buildkit_logs" }

// BuildKitJobArgModel maps the buildkit_job_args table
// (physical: cluster_agent_buildkit_job_args).
type BuildKitJobArgModel struct {
	ID         uint      `gorm:"primaryKey;autoIncrement"`
	BuildJobID string    `gorm:"column:build_job_id;index:idx_buildkit_job_args_build_job_id;not null"`
	ArgKey     string    `gorm:"column:arg_key;not null"`
	ArgValue   string    `gorm:"column:arg_value;not null"`
	Source     string    `gorm:"column:source;not null"`
	CreatedAt  time.Time `gorm:"column:created_at;default:CURRENT_TIMESTAMP"`
}

// TableName -> cluster_agent_buildkit_job_args.
func (BuildKitJobArgModel) TableName() string { return "buildkit_job_args" }

// UploadTokenModel maps the upload_tokens table
// (physical: cluster_agent_upload_tokens).
//
// Security change vs. the original schema: the raw token is NEVER persisted.
// The old `token TEXT UNIQUE NOT NULL` column is replaced by `token_hash`
// (sha256 hex of the raw token), which carries the UNIQUE + index. Callers
// pass the raw token at create/lookup time; the query layer hashes it.
type UploadTokenModel struct {
	ID           uint      `gorm:"primaryKey;autoIncrement"`
	TokenHash    string    `gorm:"column:token_hash;uniqueIndex:idx_upload_tokens_token_hash;not null"`
	DeployConfig string    `gorm:"column:deploy_config;not null"`
	Dockerfile   string    `gorm:"column:dockerfile;not null;default:''"`
	BuildArgs    string    `gorm:"column:build_args;not null;default:'[]'"`
	BuildTarget  string    `gorm:"column:build_target;not null;default:''"`
	ExpiresAt    time.Time `gorm:"column:expires_at;index:idx_upload_tokens_expires_at;not null"`
	Used         bool      `gorm:"column:used;not null;default:false"`
	CreatedAt    time.Time `gorm:"column:created_at;default:CURRENT_TIMESTAMP"`
	Purpose      string    `gorm:"column:purpose;not null;default:'upload'"`
}

// TableName -> cluster_agent_upload_tokens.
func (UploadTokenModel) TableName() string { return "upload_tokens" }

// SQLSchemaCacheModel maps the sql_schema_cache table
// (physical: cluster_agent_sql_schema_cache).
type SQLSchemaCacheModel struct {
	ID             uint      `gorm:"primaryKey;autoIncrement"`
	ConnectionHash string    `gorm:"column:connection_hash;uniqueIndex:idx_sql_schema_cache_hash;not null"`
	SchemaJSON     string    `gorm:"column:schema_json;not null"`
	CreatedAt      time.Time `gorm:"column:created_at;default:CURRENT_TIMESTAMP"`
	UpdatedAt      time.Time `gorm:"column:updated_at;default:CURRENT_TIMESTAMP"`
}

// TableName -> cluster_agent_sql_schema_cache.
func (SQLSchemaCacheModel) TableName() string { return "sql_schema_cache" }

// OneShotJobModel maps the one_shot_jobs table
// (physical: cluster_agent_one_shot_jobs).
type OneShotJobModel struct {
	ID             uint       `gorm:"primaryKey;autoIncrement"`
	RunID          string     `gorm:"column:run_id;uniqueIndex:idx_one_shot_jobs_run_id;not null"`
	OSID           string     `gorm:"column:osid;index:idx_one_shot_jobs_osid;not null"`
	Namespace      string     `gorm:"column:namespace;not null"`
	Image          string     `gorm:"column:image;not null"`
	Command        string     `gorm:"column:command;not null"`
	K8sJobName     string     `gorm:"column:k8s_job_name;not null"`
	Status         string     `gorm:"column:status;index:idx_one_shot_jobs_status;not null"`
	ExitCode       *int       `gorm:"column:exit_code"`
	Actor          string     `gorm:"column:actor;not null;default:''"`
	TimeoutSeconds int        `gorm:"column:timeout_seconds;not null;default:0"`
	CreatedAt      time.Time  `gorm:"column:created_at;default:CURRENT_TIMESTAMP"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;default:CURRENT_TIMESTAMP"`
	CompletedAt    *time.Time `gorm:"column:completed_at"`
}

// TableName -> cluster_agent_one_shot_jobs.
func (OneShotJobModel) TableName() string { return "one_shot_jobs" }

// allModels lists every model AutoMigrate must manage. Initialize and the test
// helper both migrate this exact set so the schema is identical in both.
func allModels() []any {
	return []any{
		&BuildKitJobModel{},
		&BuildKitLogModel{},
		&BuildKitJobArgModel{},
		&UploadTokenModel{},
		&SQLSchemaCacheModel{},
		&OneShotJobModel{},
	}
}
