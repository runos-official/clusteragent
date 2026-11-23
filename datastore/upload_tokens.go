package datastore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Scoped rows carry a receiver generation independently of the signed binding schema.
const scopedUploadStorageVersion = "runos.scoped-upload-storage.v2"

const (
	PurposeUpload = "upload"
	PurposePull   = "pull"
	// Older receivers select by purpose alone. Keep scoped rows outside every
	// legacy lookup, even on versions that predate authorization classification.
	scopedUploadPurpose = "scoped_upload_v2"
	// PurposeBuildImage marks an app-less build-and-push token (obj-47):
	// the uploaded tarball is a generic build context to be built and
	// pushed to explicit Harbor target ref(s), NOT an app deploy. There is
	// no owning app/osid; the target repo + tag list live in BuildTarget.
	PurposeBuildImage = "build_image"
)

// BuildTargetSpec is the explicit Harbor target for a PurposeBuildImage
// token: a repo name under the fixed `runos-apps` project plus one or more
// tags, the same built image pushed to each. Serialised into the
// upload_tokens.build_target JSON column and read back at upload time.
// Empty for app-deploy / pull tokens.
type BuildTargetSpec struct {
	Repo string   `json:"repo"`
	Tags []string `json:"tags"`
	// HarborOsid is the Harbor service identity Conductor verified and
	// locked before minting this token (story 390's criterion 5; FCR 2487).
	// Empty for tokens minted before this field existed, or for any
	// caller that still omits it: the upload path then keeps today's
	// behavior (no Harbor-identity comparison), per criterion 6(d).
	HarborOsid string `json:"harborOsid,omitempty"`
}

// UploadToken represents a single-use upload or pull token record.
//
// SECURITY: the raw token is NEVER persisted. Only sha256(token) hex is stored
// (column token_hash, unique). Create* functions hash the raw token before
// insert; Get*/Mark*/Delete* functions hash the presented raw token and match
// on the hash. The Token field on this DTO is the raw token the caller supplied
// at lookup time (echoed back for source compatibility); it is not read from
// the database.
//
// DeployConfig holds "{osid}:{cliUploadID}" for both purposes. For legacy
// upload-purpose rows written by older conductors the value may be just
// "{osid}" with no separator; callers must handle both shapes.
//
// Dockerfile is the path to the Dockerfile inside the uploaded source
// tree, relative to the tarball root. Empty defaults to "Dockerfile" at
// the root, matching the pre-monorepo single-app shape. Monorepo CLI deploys
// send paths like "apps/api/Dockerfile". Only meaningful for upload-purpose
// tokens.
//
// BuildArgs holds the effective Docker build args ([{key,value,source}])
// resolved by conductor and forwarded on the create-token request. They are
// captured here because the CLI build runs in a later request (the tarball
// upload) than the one that mints the token, so the args must survive the
// gap. Empty for tokens minted before this feature (column defaults to '[]').
// Only meaningful for upload-purpose tokens.
//
// BuildTarget is the explicit Harbor target (repo + tag list) for
// PurposeBuildImage tokens; nil for app-deploy / pull tokens (obj-47).
type UploadToken struct {
	ID                          int64                   `json:"id"`
	Token                       string                  `json:"token"`
	DeployConfig                string                  `json:"deploy_config"`
	Dockerfile                  string                  `json:"dockerfile"`
	BuildArgs                   []BuildArg              `json:"build_args"`
	ExpiresAt                   time.Time               `json:"expires_at"`
	Used                        bool                    `json:"used"`
	CreatedAt                   time.Time               `json:"created_at"`
	Purpose                     string                  `json:"purpose"`
	BuildTarget                 *BuildTargetSpec        `json:"build_target,omitempty"`
	AuthorizationClassification string                  `json:"authorizationClassification"`
	AuthorizationBinding        *UploadAuthorityBinding `json:"authorizationBinding,omitempty"`
	// DurableJobID is Conductor's durable job-authority id (story 339);
	// empty for a legacy token. See UploadTokenModel.DurableJobID.
	DurableJobID string `json:"durableJobId,omitempty"`
}

// CreateUploadToken creates a new single-use token for uploading code for an OSID.
// cliUploadID is the conductor-assigned identity for the upload; it ends up as
// the Harbor archive tag and the BuildKit jobID. May be empty for legacy
// callers; the upload handler will mint one in that case.
//
// dockerfile is the path to the Dockerfile inside the tarball, relative
// to the tarball root. Empty falls back to "Dockerfile" at the root.
//
// The raw token is hashed (sha256 hex) before storage; it is never persisted.
func CreateUploadToken(token, osid, cliUploadID, dockerfile string, buildArgs []BuildArg, expiresAt time.Time) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	deployConfig := osid
	if cliUploadID != "" {
		deployConfig = osid + ":" + cliUploadID
	}
	buildArgsJSON, err := encodeBuildArgs(buildArgs)
	if err != nil {
		return err
	}
	row := UploadTokenModel{
		TokenHash:                   hashToken(token),
		DeployConfig:                deployConfig,
		Dockerfile:                  dockerfile,
		BuildArgs:                   buildArgsJSON,
		ExpiresAt:                   expiresAt,
		Purpose:                     PurposeUpload,
		AuthorizationClassification: AuthorizationLegacy,
	}
	return gdb.Create(&row).Error
}

// CreateScopedUploadToken stores one immutable scoped authorization binding
// plus Conductor's durable job-authority id (story 339), REQUIRED: every
// later queue_admission/build_start/deployment_patch stage recheck for the
// resulting build presents it to agentstream.JobAuthority. Review finding
// 1397: a scoped classification with no durable job id must refuse the
// mint, not silently select legacy (no-recheck) behavior for the later
// stages - the authorization class and the presence of a job id are two
// independent signals, and a scoped upload that can never reach its later
// stage rechecks is not a supported shape.
func CreateScopedUploadToken(token, osid, cliUploadID, dockerfile string, buildArgs []BuildArg, expiresAt time.Time, binding UploadAuthorityBinding, durableJobID string) error {
	if err := ValidateUploadAuthorityBinding(binding, UploadAuthorityTarget{
		AppID:    osid,
		UploadID: cliUploadID,
		Purpose:  PurposeUpload,
	}, expiresAt); err != nil {
		return err
	}
	if !uuidPattern.MatchString(durableJobID) {
		return fmt.Errorf("invalid durable job id")
	}
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	buildArgsJSON, err := encodeBuildArgs(buildArgs)
	if err != nil {
		return err
	}
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("marshal authorization binding: %w", err)
	}
	row := UploadTokenModel{
		TokenHash:                   hashToken(token),
		DeployConfig:                osid + ":" + cliUploadID,
		Dockerfile:                  dockerfile,
		BuildArgs:                   buildArgsJSON,
		ExpiresAt:                   expiresAt,
		Purpose:                     scopedUploadPurpose,
		AuthorizationClassification: AuthorizationScoped,
		AuthorizationBindingVersion: scopedUploadStorageVersion,
		AuthorizationBinding:        string(bindingJSON),
		DurableJobID:                durableJobID,
	}
	return gdb.Create(&row).Error
}

// CreateBuildImageToken creates a single-use token for an app-less
// build-and-push (obj-47). uploadID is the conductor-assigned upload
// identity that doubles as the BuildKit jobID. repo + tags are the explicit
// Harbor target under the fixed `runos-apps` project; the same built image is
// pushed to every tag. harborOsid is the Harbor service identity Conductor
// verified and locked before minting the token (story 390 criterion 5); empty
// for a legacy caller that does not send it (criterion 6(d) explicit legacy
// branch). dockerfile is the path inside the tarball (relative to the
// context root, empty => "Dockerfile"). buildArgs are the effective args
// resolved by conductor.
//
// deploy_config is stored as "<repo>:<uploadID>" so the shared upload handler's
// existing `osid:uploadID` split yields a synthetic, app-less build identity
// (osid = repo). The tag list and Harbor identity live in the build_target
// JSON column.
//
// The raw token is hashed (sha256 hex) before storage; it is never persisted.
func CreateBuildImageToken(token, uploadID, repo string, tags []string, harborOsid, dockerfile string, buildArgs []BuildArg, expiresAt time.Time) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	deployConfig := repo + ":" + uploadID

	buildArgsJSON, err := encodeBuildArgs(buildArgs)
	if err != nil {
		return err
	}
	targetJSON, err := json.Marshal(BuildTargetSpec{Repo: repo, Tags: tags, HarborOsid: harborOsid})
	if err != nil {
		return fmt.Errorf("marshal build target: %w", err)
	}

	row := UploadTokenModel{
		TokenHash:                   hashToken(token),
		DeployConfig:                deployConfig,
		Dockerfile:                  dockerfile,
		BuildArgs:                   buildArgsJSON,
		BuildTarget:                 string(targetJSON),
		ExpiresAt:                   expiresAt,
		Purpose:                     PurposeBuildImage,
		AuthorizationClassification: AuthorizationLegacy,
	}
	return gdb.Create(&row).Error
}

// CreatePullToken creates a new single-use token for downloading a stored archive.
// The raw token is hashed (sha256 hex) before storage; it is never persisted.
func CreatePullToken(token, osid, cliUploadID string, expiresAt time.Time) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	row := UploadTokenModel{
		TokenHash:                   hashToken(token),
		DeployConfig:                osid + ":" + cliUploadID,
		ExpiresAt:                   expiresAt,
		Purpose:                     PurposePull,
		AuthorizationClassification: AuthorizationLegacy,
	}
	return gdb.Create(&row).Error
}

// encodeBuildArgs serialises the effective build args as a JSON array. nil
// marshals to "null", so default to "[]" to keep the stored shape stable and
// the read-back unmarshal clean.
func encodeBuildArgs(buildArgs []BuildArg) (string, error) {
	if len(buildArgs) == 0 {
		return "[]", nil
	}
	encoded, err := json.Marshal(buildArgs)
	if err != nil {
		return "", fmt.Errorf("marshal build args: %w", err)
	}
	return string(encoded), nil
}

// GetUploadToken retrieves an upload-purpose (app deploy) token by its raw value.
func GetUploadToken(token string) (*UploadToken, error) {
	return getTokenByPurposes(token, PurposeUpload, scopedUploadPurpose)
}

// GetPullToken retrieves a pull-purpose token by its raw value.
func GetPullToken(token string) (*UploadToken, error) {
	return getTokenByPurpose(token, PurposePull)
}

// GetUploadableToken retrieves a token usable at the /cli-deploy upload
// endpoint: either an app-deploy upload or an app-less build-image token
// (obj-47). The shared handler branches on the returned Purpose. Pull tokens
// are excluded so the single-use purpose isolation is preserved.
func GetUploadableToken(token string) (*UploadToken, error) {
	return getTokenByPurposes(token, PurposeUpload, PurposeBuildImage, scopedUploadPurpose)
}

func getTokenByPurpose(token string, purpose string) (*UploadToken, error) {
	return getTokenByPurposes(token, purpose)
}

// getTokenByPurposes looks a token up by its hash and a purpose allow-list. The
// raw token is hashed and matched against token_hash, so the plaintext is never
// queried.
func getTokenByPurposes(token string, purposes ...string) (*UploadToken, error) {
	if len(purposes) == 0 {
		return nil, fmt.Errorf("no purpose specified")
	}
	gdb, err := activeDB()
	if err != nil {
		return nil, err
	}

	var m UploadTokenModel
	err = gdb.Where("token_hash = ? AND purpose IN ?", hashToken(token), purposes).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("token not found")
	}
	if err != nil {
		return nil, err
	}

	// Defence in depth: the unique index already guarantees a single match, but
	// confirm the stored hash equals the presented token's hash with a
	// constant-time compare so a future non-equality lookup can't be timed.
	if !constantTimeEqualHash(m.TokenHash, hashToken(token)) {
		return nil, fmt.Errorf("token not found")
	}

	t := UploadToken{
		ID:                          int64(m.ID),
		Token:                       token, // echo the raw token back; never read from the DB
		DeployConfig:                m.DeployConfig,
		Dockerfile:                  m.Dockerfile,
		ExpiresAt:                   m.ExpiresAt,
		Used:                        m.Used,
		CreatedAt:                   m.CreatedAt,
		Purpose:                     m.Purpose,
		AuthorizationClassification: m.AuthorizationClassification,
		DurableJobID:                m.DurableJobID,
	}
	if err := loadAuthorizationBinding(m, &t); err != nil {
		return nil, err
	}
	// Empty/legacy rows store '[]' (column default); only unmarshal when
	// there's something to parse so a blank value can't error the lookup.
	if m.BuildArgs != "" && m.BuildArgs != "[]" {
		if err := json.Unmarshal([]byte(m.BuildArgs), &t.BuildArgs); err != nil {
			return nil, fmt.Errorf("unmarshal build args for token: %w", err)
		}
	}
	// build_target is set only for PurposeBuildImage tokens; older rows and
	// app-deploy/pull tokens leave it empty.
	if bt := strings.TrimSpace(m.BuildTarget); bt != "" && bt != "{}" {
		var spec BuildTargetSpec
		if err := json.Unmarshal([]byte(bt), &spec); err != nil {
			return nil, fmt.Errorf("unmarshal build target for token: %w", err)
		}
		t.BuildTarget = &spec
	}
	return &t, nil
}

func loadAuthorizationBinding(model UploadTokenModel, token *UploadToken) error {
	if (model.Purpose == scopedUploadPurpose) != (model.AuthorizationClassification == AuthorizationScoped) {
		return errors.New("stored token purpose does not match scoped classification")
	}
	switch model.AuthorizationClassification {
	case AuthorizationLegacy:
		if model.AuthorizationBindingVersion != "" || strings.TrimSpace(model.AuthorizationBinding) != "" {
			return fmt.Errorf("legacy token contains scoped authorization binding data")
		}
		return nil
	case AuthorizationScoped:
		if model.AuthorizationBindingVersion != scopedUploadStorageVersion {
			return fmt.Errorf("unsupported stored authorization binding version %q", model.AuthorizationBindingVersion)
		}
		if strings.TrimSpace(model.AuthorizationBinding) == "" {
			return fmt.Errorf("scoped token has no authorization binding")
		}
		binding, err := decodeUploadAuthorityBinding(model.AuthorizationBinding)
		if err != nil {
			return err
		}
		appID, uploadID, ok := strings.Cut(model.DeployConfig, ":")
		if !ok || appID == "" || uploadID == "" {
			return fmt.Errorf("scoped token has malformed deploy_config")
		}
		if err := ValidateUploadAuthorityBinding(binding, UploadAuthorityTarget{
			AppID:    appID,
			UploadID: uploadID,
			Purpose:  PurposeUpload,
		}, model.ExpiresAt); err != nil {
			return err
		}
		token.Purpose = PurposeUpload
		token.AuthorizationBinding = &binding
		return nil
	default:
		return fmt.Errorf("unknown authorization classification %q", model.AuthorizationClassification)
	}
}

// errTokenUnusable is returned by MarkUploadTokenUsed whenever the token
// cannot be claimed, for any reason (not found, already used, token
// expired, or permit expired): the caller-visible message deliberately
// does not distinguish these, matching the generic refusal the HTTP layer
// already builds around this call.
var errTokenUnusable = fmt.Errorf("token not found, already used, or expired")

// MarkUploadTokenUsed atomically marks a token (any purpose) as used, looked up
// by the hash of the presented raw token. Returns error if the token doesn't
// exist, is already used, has expired, or - when the caller supplies one -
// if permitDeadline has passed. permitDeadline is nil for every claim caller
// with no separate, shorter-lived permit to enforce (the build-image upload,
// the pull download, and an explicit legacy app upload); the scoped app
// upload caller passes its fresh recheck permit's own ExpiresAt. There are
// exactly three production claim callers (review finding 1239's derived
// list, pinned by TestMarkUploadTokenUsedClaimCallSites_MatchExpectedShape):
// webhook/cli_deploy_handler.go's claimUploadToken for the build-image
// purpose (no permit), the same claimUploadToken for the scoped/legacy app
// upload purpose (a permit only for the scoped classification), and
// webhook/cli_pull_handler.go's direct call (no permit).
//
// The expiry check runs in the SAME atomic claim as the used-state check
// (review finding 1076): an earlier, separate expiry check (the kind the
// upload handler does before reading the body) only proves the token was
// valid AT THAT MOMENT. Body reads, tarball validation, the BuildKit
// configuration read, and - for a scoped token - a network round trip to
// Conductor/Roid can all take long enough for the token (or, for a scoped
// upload, the fresh permit) to expire before the claim actually runs.
// Folding both deadlines into this one claim closes that window for every
// purpose and every classification, legacy included, with no second query
// and no extra round trip.
//
// Review finding 1128 (repeat of 1076): a bound Go time.Now() parameter
// captured before the statement is sent lets a delay between that capture
// and the claim actually executing (lock wait, a slow write, a stalled
// connection) commit the claim after the real deadline. The comparison must
// instead be evaluated by the database AT the moment the claim executes.
//
// Review finding 1168 (repeat of 1128) and its repeats 1239/1244: on
// PostgreSQL, folding clock_timestamp() into a single UPDATE's own WHERE
// clause is still not "at the moment the claim executes". A plain UPDATE's
// quals can be evaluated during the initial table scan, BEFORE the
// statement blocks waiting for a conflicting row lock; if that lock is
// released without the row's content actually changing (a lock held and
// released, not a competing UPDATE - review finding 1239's "row locks that
// leave the token row unchanged"), the executor has nothing to re-evaluate
// once the wait ends and proceeds with the qual result computed BEFORE the
// wait: a stale "not yet expired" clock_timestamp() value, even though real
// wall-clock time has since passed the deadline. The fix separates locking
// from evaluating: acquire the row lock in its own statement first
// (SELECT ... FOR UPDATE, which blocks until the lock is granted and only
// then returns the row), then read the database's wall clock in a second
// statement, so both happen strictly after the wait - all inside one
// transaction, so nothing else can touch the row in between. Finding 1244's
// permit deadline is folded into that same post-lock comparison.
//
// SQLite has no row-level locking: a write blocks at the whole-database
// level before it can even begin, so there is no pre-wait qual to go stale,
// and it keeps the simpler single-statement form. Review finding 1203:
// SQLite has no clock_timestamp(), and mixing its CURRENT_TIMESTAMP (bare
// UTC text) with a stored time.Time that carries a non-UTC local offset
// (what a caller's un-normalized time.Now() produces) compares as TEXT and
// is skewed by the host's UTC offset - measured in this repository to fail
// in both directions off UTC. julianday() parses either an offset-qualified
// or a bare timestamp into a real, comparable instant, so wrapping both
// sides in julianday() compares actual instants regardless of either
// side's textual timezone. SQLite is test-only here; production always
// runs PostgreSQL (datastore/database.go).
func MarkUploadTokenUsed(token string, permitDeadline *time.Time) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	if gdb.Name() == "postgres" {
		return markUploadTokenUsedPostgres(gdb, token, permitDeadline)
	}
	return markUploadTokenUsedSQLite(gdb, token, permitDeadline)
}

// markUploadTokenUsedPostgres implements the lock-then-evaluate claim
// described above. table is read once per call rather than cached,
// matching UploadTokenModel's own TableName() (always the fixed
// cluster_agent_upload_tokens, never request-dependent).
func markUploadTokenUsedPostgres(gdb *gorm.DB, token string, permitDeadline *time.Time) error {
	hashed := hashToken(token)
	table := (UploadTokenModel{}).TableName()
	txErr := gdb.Transaction(func(tx *gorm.DB) error {
		var row struct {
			ID        uint
			Used      bool
			ExpiresAt time.Time
		}
		// Blocks here until any existing lock on this row is released -
		// whether or not the holder actually changed the row - and only
		// returns the row once the lock is granted: the read below is
		// strictly after that wait, not before it.
		if err := tx.Raw(
			"SELECT id, used, expires_at FROM "+table+" WHERE token_hash = ? FOR UPDATE",
			hashed,
		).Scan(&row).Error; err != nil {
			return err
		}
		if row.ID == 0 {
			return errTokenUnusable
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		if row.Used || !now.Before(row.ExpiresAt) {
			return errTokenUnusable
		}
		if permitDeadline != nil && !now.Before(*permitDeadline) {
			return errTokenUnusable
		}
		return tx.Exec("UPDATE "+table+" SET used = true WHERE id = ?", row.ID).Error
	})
	if txErr != nil {
		return errTokenUnusable
	}
	return nil
}

// markUploadTokenUsedSQLite implements the claim with one statement:
// SQLite's single-writer semantics mean a concurrent write blocks before it
// can even start, so there is no pre-wait qual evaluation to go stale here
// (see the function doc above).
func markUploadTokenUsedSQLite(gdb *gorm.DB, token string, permitDeadline *time.Time) error {
	clause := "token_hash = ? AND used = ? AND julianday(expires_at) > julianday('now')"
	args := []any{hashToken(token), false}
	if permitDeadline != nil {
		clause += " AND julianday(?) > julianday('now')"
		args = append(args, *permitDeadline)
	}
	res := gdb.Model(&UploadTokenModel{}).Where(clause, args...).Update("used", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errTokenUnusable
	}
	return nil
}

// DeleteExpiredUploadTokens removes tokens (any purpose) that have expired.
func DeleteExpiredUploadTokens() error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	return gdb.Where("expires_at < ?", time.Now()).Delete(&UploadTokenModel{}).Error
}
