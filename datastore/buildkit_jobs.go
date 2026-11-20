package datastore

import (
	"fmt"
	"regexp"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BuildKitJob represents a build job record returned to callers. The JSON tags
// are part of the on-the-wire contract for LIST_BUILD_JOBS, so they are
// preserved exactly; the persistence shape lives in BuildKitJobModel.
//
// AuthorizationClassification and AuthorityJobID are internal attribution
// (story 339), not part of the LIST_BUILD_JOBS wire contract; they carry no
// `json` tag so they are never serialized to that response.
type BuildKitJob struct {
	ID                          int64      `json:"id"`
	JobID                       string     `json:"job_id"`
	OSID                        string     `json:"osid"`
	Repo                        string     `json:"repo"`
	Branch                      string     `json:"branch"`
	CommitHash                  string     `json:"commit_hash"`
	Status                      string     `json:"status"`
	CreatedAt                   time.Time  `json:"created_at"`
	UpdatedAt                   time.Time  `json:"updated_at"`
	CompletedAt                 *time.Time `json:"completed_at"`
	AuthorizationClassification string     `json:"-"`
	AuthorityJobID              string     `json:"-"`
}

// buildKitJobAuthorityJobIDPattern requires a canonical lowercase UUID,
// matching agentstream.JobAuthority's own requirement (agentstream's
// authority.validate()): storing anything else here would pass creation
// but fail every later stage recheck with an opaque transport error
// instead of a clear refusal now.
var buildKitJobAuthorityJobIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// JobStatus constants
const (
	JobStatusPending = "pending"
	JobStatusBusy    = "busy"
	JobStatusSuccess = "success"
	JobStatusFailed  = "failed"
)

func (m BuildKitJobModel) toDTO() BuildKitJob {
	return BuildKitJob{
		ID:                          int64(m.ID),
		JobID:                       m.JobID,
		OSID:                        m.OSID,
		Repo:                        m.Repo,
		Branch:                      m.Branch,
		CommitHash:                  m.CommitHash,
		Status:                      m.Status,
		CreatedAt:                   m.CreatedAt,
		UpdatedAt:                   m.UpdatedAt,
		CompletedAt:                 m.CompletedAt,
		AuthorizationClassification: m.AuthorizationClassification,
		AuthorityJobID:              m.AuthorityJobID,
	}
}

// CreateBuildKitJob creates a new build job record, or re-opens an existing
// one under the same job_id as a fresh pending row (review finding 1133).
//
// job_id carries a unique index, and the webhook upload handler now calls
// this (via recordFailedUpload/failUpload) for a TERMINAL pre-claim refusal
// - one where the token stays unclaimed and a legitimate retry with the
// same token (and so the same uploadID/job_id) is expected. A plain INSERT
// would fail that retry's own CreateBuildKitJob call with a duplicate-key
// error once a build actually starts. Upserting resets status, clears
// completed_at, and bumps updated_at, so the retry's row looks exactly like
// a fresh pending job; created_at is left untouched so the row's original
// first-seen time survives.
func CreateBuildKitJob(jobID, osid, repo, branch, commitHash string) error {
	return createBuildKitJob(jobID, osid, repo, branch, commitHash, AuthorizationLegacy, "")
}

// CreateScopedBuildKitJob creates a new build job record bound to a durable,
// authoritative job id (handbook 949 C3's "job: jobId" stage authority),
// or re-opens an existing row under the same job_id as a fresh pending row
// (the same upsert-reopens discipline CreateBuildKitJob uses, review
// finding 1133). authorityJobID is the public, non-secret job-authority
// reference every later queue_admission/build_start/deployment_patch
// recheck for this job presents to agentstream.JobAuthority; it must be a
// canonical lowercase UUID.
//
// The caller must request and receive a queue_admission permit for
// authorityJobID BEFORE calling this (plan "CLI/VCS propagation": "Request
// queue admission for the bound job. Create the build job only after
// permit."). This function only persists the attribution; it performs no
// recheck itself.
func CreateScopedBuildKitJob(jobID, authorityJobID, osid, repo, branch, commitHash string) error {
	if !buildKitJobAuthorityJobIDPattern.MatchString(authorityJobID) {
		return fmt.Errorf("invalid authority job id")
	}
	return createBuildKitJob(jobID, osid, repo, branch, commitHash, AuthorizationScoped, authorityJobID)
}

// createBuildKitJob is CreateBuildKitJob/CreateScopedBuildKitJob's shared
// upsert. The DoUpdates clause deliberately OMITS authorization_classification
// and authority_job_id: once a job_id row exists, its binding is immutable
// (plan "Durable job binding": "Keep the binding immutable after job
// creation"), so a reopen (retry with the same job_id) preserves whatever
// attribution the row was FIRST created with, even if a later caller on the
// same job_id passes a different classification.
func createBuildKitJob(jobID, osid, repo, branch, commitHash, classification, authorityJobID string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	job := BuildKitJobModel{
		JobID:                       jobID,
		OSID:                        osid,
		Repo:                        repo,
		Branch:                      branch,
		CommitHash:                  commitHash,
		Status:                      JobStatusPending,
		AuthorizationClassification: classification,
		AuthorityJobID:              authorityJobID,
	}
	return gdb.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "job_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"osid":         osid,
			"repo":         repo,
			"branch":       branch,
			"commit_hash":  commitHash,
			"status":       JobStatusPending,
			"completed_at": nil,
			"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
		}),
	}).Create(&job).Error
}

// RecordFailedUploadIfUnclaimed writes a failed build row for jobID, UNLESS
// the upload token identified by the raw token has already been claimed by
// a concurrent request (review finding 1267).
//
// A terminal PRE-claim refusal (the tarball/Dockerfile/Harbor-identity
// checks, or a scoped deny) can fire for a request that loaded the token
// early but took a while to decide, while a DIFFERENT concurrent request
// for the SAME token ran its own checks faster, claimed the token, and
// started or finished its build under the same job_id (job_id is always the
// token's own uploadID). CreateBuildKitJob's upsert (review finding 1133)
// unconditionally resets that row to pending and clears completed_at, so
// the slower request's refusal would overwrite the winner's success (or
// in-progress state) with a bogus failure - Conductor could then report a
// failed deployment that actually succeeded. A local datastore probe
// confirmed the upsert does exactly this reset.
//
// On PostgreSQL this reads the token's used state under FOR UPDATE and
// writes the failure row in the SAME transaction, so it cannot race the
// claim's own transaction (datastore.MarkUploadTokenUsed also locks this
// row via FOR UPDATE before updating it, review finding 1239): whichever
// transaction reaches the row first simply runs to completion before the
// other proceeds. SQLite (test-only; production always runs PostgreSQL)
// keeps a plain read, since it has no row-level locking to join.
//
// This must be called ONLY for a refusal where the CALLING request has not
// itself claimed the token - a request that already owns the claim (an
// archive-push failure, or a permit that expired between claim and push)
// must always record its failure and should keep calling CreateBuildKitJob
// directly instead.
func RecordFailedUploadIfUnclaimed(token, jobID, osid, reason string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	if gdb.Name() == "postgres" {
		return recordFailedUploadIfUnclaimedPostgres(gdb, token, jobID, osid, reason)
	}
	return recordFailedUploadIfUnclaimedSQLite(gdb, token, jobID, osid, reason)
}

func recordFailedUploadIfUnclaimedPostgres(gdb *gorm.DB, token, jobID, osid, reason string) error {
	hashed := hashToken(token)
	table := (UploadTokenModel{}).TableName()
	return gdb.Transaction(func(tx *gorm.DB) error {
		var used bool
		if err := tx.Raw("SELECT used FROM "+table+" WHERE token_hash = ? FOR UPDATE", hashed).Scan(&used).Error; err != nil {
			return err
		}
		if used {
			// A concurrent request already claimed this token: it owns
			// jobID's build row from here. Writing here would overwrite
			// its result.
			return nil
		}
		return writeFailedBuildRow(tx, jobID, osid, reason)
	})
}

func recordFailedUploadIfUnclaimedSQLite(gdb *gorm.DB, token, jobID, osid, reason string) error {
	hashed := hashToken(token)
	table := (UploadTokenModel{}).TableName()
	return gdb.Transaction(func(tx *gorm.DB) error {
		var used bool
		if err := tx.Raw("SELECT used FROM "+table+" WHERE token_hash = ?", hashed).Scan(&used).Error; err != nil {
			return err
		}
		if used {
			return nil
		}
		return writeFailedBuildRow(tx, jobID, osid, reason)
	})
}

// writeFailedBuildRow is CreateBuildKitJob + InsertBuildKitLog +
// UpdateBuildKitJobStatus's combined effect, run against the given
// transaction handle instead of a fresh activeDB() connection, so a caller
// can fold it into a larger transaction (RecordFailedUploadIfUnclaimed
// above).
func writeFailedBuildRow(tx *gorm.DB, jobID, osid, reason string) error {
	job := BuildKitJobModel{
		JobID:                       jobID,
		OSID:                        osid,
		CommitHash:                  jobID,
		Status:                      JobStatusPending,
		AuthorizationClassification: AuthorizationLegacy,
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "job_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"osid":         osid,
			"repo":         "",
			"branch":       "",
			"commit_hash":  jobID,
			"status":       JobStatusPending,
			"completed_at": nil,
			"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
		}),
	}).Create(&job).Error; err != nil {
		return err
	}
	if err := tx.Create(&BuildKitLogModel{JobID: jobID, LogEntry: "ERROR: " + reason}).Error; err != nil {
		return err
	}
	return tx.Model(&BuildKitJobModel{}).Where("job_id = ?", jobID).Updates(map[string]any{
		"status":       JobStatusFailed,
		"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
		"completed_at": gorm.Expr("CURRENT_TIMESTAMP"),
	}).Error
}

// UpdateBuildKitJobStatus updates the status of a build job
func UpdateBuildKitJobStatus(jobID, status string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	updates := map[string]any{
		"status":     status,
		"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
	}
	if status == JobStatusSuccess || status == JobStatusFailed {
		updates["completed_at"] = gorm.Expr("CURRENT_TIMESTAMP")
	}
	res := gdb.Model(&BuildKitJobModel{}).Where("job_id = ?", jobID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("job not found: %s", jobID)
	}
	return nil
}

// FailNonTerminalBuildKitJobs closes every build row that is still pending or
// busy: it marks the row failed and writes one "ERROR: <reason>" log line, so
// the reason reaches whoever reads the build log. It returns how many rows it
// closed.
//
// Only call it when no build can be running in this process: at startup, before
// the agent accepts work (the previous process took its builds with it), or at
// shutdown after the drain gave up. A row that a live build still owns would be
// failed under it.
func FailNonTerminalBuildKitJobs(reason string) (int, error) {
	gdb, err := activeDB()
	if err != nil {
		return 0, err
	}
	closed := 0
	err = gdb.Transaction(func(tx *gorm.DB) error {
		var jobIDs []string
		if err := tx.Model(&BuildKitJobModel{}).
			Where("status IN ?", []string{JobStatusPending, JobStatusBusy}).
			Pluck("job_id", &jobIDs).Error; err != nil {
			return err
		}
		if len(jobIDs) == 0 {
			return nil
		}
		res := tx.Model(&BuildKitJobModel{}).Where("job_id IN ?", jobIDs).Updates(map[string]any{
			"status":       JobStatusFailed,
			"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
			"completed_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
		if res.Error != nil {
			return res.Error
		}
		logs := make([]BuildKitLogModel, 0, len(jobIDs))
		for _, id := range jobIDs {
			logs = append(logs, BuildKitLogModel{JobID: id, LogEntry: "ERROR: " + reason})
		}
		if err := tx.Create(&logs).Error; err != nil {
			return err
		}
		closed = len(jobIDs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return closed, nil
}

// GetBuildKitJob retrieves a single job by job_id
func GetBuildKitJob(jobID string) (*BuildKitJob, error) {
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}
	var m BuildKitJobModel
	if err := gdb.Where("job_id = ?", jobID).First(&m).Error; err != nil {
		return nil, err
	}
	dto := m.toDTO()
	return &dto, nil
}

// QueryBuildKitJobsOptions holds filter options for querying jobs
type QueryBuildKitJobsOptions struct {
	Status    string
	OSID      string
	CreatedAt *time.Time
	Limit     int
	Desc      bool
}

// QueryBuildKitJobs retrieves jobs with optional filters
func QueryBuildKitJobs(opts QueryBuildKitJobsOptions) ([]BuildKitJob, error) {
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}
	q := gdb.Model(&BuildKitJobModel{})

	if opts.Status != "" {
		q = q.Where("status = ?", opts.Status)
	}
	if opts.OSID != "" {
		q = q.Where("osid = ?", opts.OSID)
	}
	if opts.CreatedAt != nil {
		q = q.Where("created_at >= ?", *opts.CreatedAt)
	}
	if opts.Desc {
		q = q.Order("created_at DESC")
	} else {
		q = q.Order("created_at ASC")
	}
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}

	var models []BuildKitJobModel
	if err := q.Find(&models).Error; err != nil {
		return nil, err
	}

	var jobs []BuildKitJob
	for _, m := range models {
		jobs = append(jobs, m.toDTO())
	}
	return jobs, nil
}
