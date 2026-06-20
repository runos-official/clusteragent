package datastore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	PurposeUpload = "upload"
	PurposePull   = "pull"
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
	ID           int64            `json:"id"`
	Token        string           `json:"token"`
	DeployConfig string           `json:"deploy_config"`
	Dockerfile   string           `json:"dockerfile"`
	BuildArgs    []BuildArg       `json:"build_args"`
	ExpiresAt    time.Time        `json:"expires_at"`
	Used         bool             `json:"used"`
	CreatedAt    time.Time        `json:"created_at"`
	Purpose      string           `json:"purpose"`
	BuildTarget  *BuildTargetSpec `json:"build_target,omitempty"`
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
		TokenHash:    hashToken(token),
		DeployConfig: deployConfig,
		Dockerfile:   dockerfile,
		BuildArgs:    buildArgsJSON,
		ExpiresAt:    expiresAt,
		Purpose:      PurposeUpload,
	}
	return gdb.Create(&row).Error
}

// CreateBuildImageToken creates a single-use token for an app-less
// build-and-push (obj-47). uploadID is the conductor-assigned upload
// identity that doubles as the BuildKit jobID. repo + tags are the explicit
// Harbor target under the fixed `runos-apps` project; the same built image is
// pushed to every tag. dockerfile is the path inside the tarball (relative to
// the context root, empty => "Dockerfile"). buildArgs are the effective args
// resolved by conductor.
//
// deploy_config is stored as "<repo>:<uploadID>" so the shared upload handler's
// existing `osid:uploadID` split yields a synthetic, app-less build identity
// (osid = repo). The tag list lives in the build_target JSON column.
//
// The raw token is hashed (sha256 hex) before storage; it is never persisted.
func CreateBuildImageToken(token, uploadID, repo string, tags []string, dockerfile string, buildArgs []BuildArg, expiresAt time.Time) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	deployConfig := repo + ":" + uploadID

	buildArgsJSON, err := encodeBuildArgs(buildArgs)
	if err != nil {
		return err
	}
	targetJSON, err := json.Marshal(BuildTargetSpec{Repo: repo, Tags: tags})
	if err != nil {
		return fmt.Errorf("marshal build target: %w", err)
	}

	row := UploadTokenModel{
		TokenHash:    hashToken(token),
		DeployConfig: deployConfig,
		Dockerfile:   dockerfile,
		BuildArgs:    buildArgsJSON,
		BuildTarget:  string(targetJSON),
		ExpiresAt:    expiresAt,
		Purpose:      PurposeBuildImage,
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
		TokenHash:    hashToken(token),
		DeployConfig: osid + ":" + cliUploadID,
		ExpiresAt:    expiresAt,
		Purpose:      PurposePull,
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
	return getTokenByPurpose(token, PurposeUpload)
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
	return getTokenByPurposes(token, PurposeUpload, PurposeBuildImage)
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
		ID:           int64(m.ID),
		Token:        token, // echo the raw token back; never read from the DB
		DeployConfig: m.DeployConfig,
		Dockerfile:   m.Dockerfile,
		ExpiresAt:    m.ExpiresAt,
		Used:         m.Used,
		CreatedAt:    m.CreatedAt,
		Purpose:      m.Purpose,
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

// MarkUploadTokenUsed atomically marks a token (any purpose) as used, looked up
// by the hash of the presented raw token. Returns error if the token doesn't
// exist or is already used.
func MarkUploadTokenUsed(token string) error {
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	res := gdb.Model(&UploadTokenModel{}).
		Where("token_hash = ? AND used = ?", hashToken(token), false).
		Update("used", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("token not found or already used")
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
