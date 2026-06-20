package datastore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
// DeployConfig holds "{osid}:{cliUploadID}" for both purposes. For legacy
// upload-purpose rows written by older conductors the value may be just
// "{osid}" with no separator; callers must handle both shapes.
//
// Dockerfile is the path to the Dockerfile inside the uploaded source
// tree, relative to the tarball root. Empty defaults to "Dockerfile" at
// the root, matching the pre-monorepo single-app shape. Closes iter-27
// I27-Y: monorepo CLI deploys send paths like "apps/api/Dockerfile" via
// `runos deploy --dockerfile <path>` (or the yaml's `dockerfile:` field)
// and the CLI build handler needs to honor them instead of stripping to
// a basename. Only meaningful for upload-purpose tokens.
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
// Iter-27 I27-Y.
func CreateUploadToken(token, osid, cliUploadID, dockerfile string, buildArgs []BuildArg, expiresAt time.Time) error {
	deployConfig := osid
	if cliUploadID != "" {
		deployConfig = osid + ":" + cliUploadID
	}
	// Serialise the effective build args as a JSON array. nil marshals to
	// "null", so default to an empty array to keep the stored shape stable
	// and the read-back unmarshal clean.
	buildArgsJSON := []byte("[]")
	if len(buildArgs) > 0 {
		encoded, err := json.Marshal(buildArgs)
		if err != nil {
			return fmt.Errorf("marshal build args: %w", err)
		}
		buildArgsJSON = encoded
	}
	query := `
		INSERT INTO upload_tokens (token, deploy_config, dockerfile, build_args, expires_at, purpose)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := db.Exec(query, token, deployConfig, dockerfile, string(buildArgsJSON), expiresAt, PurposeUpload)
	return err
}

// CreateBuildImageToken creates a single-use token for an app-less
// build-and-push (obj-47). uploadID is the conductor-assigned upload
// identity that doubles as the BuildKit jobID (so conductor can poll
// LIST_BUILD_JOBS/LIST_BUILD_LOGS by it). repo + tags are the explicit
// Harbor target under the fixed `runos-apps` project; the same built
// image is pushed to every tag. dockerfile is the path inside the tarball
// (relative to the context root, empty => "Dockerfile"). buildArgs are the
// effective args resolved by conductor.
//
// deploy_config is stored as "<repo>:<uploadID>" so the shared upload
// handler's existing `osid:uploadID` split yields a synthetic, app-less
// build identity (osid = repo) that keeps the console builds UI
// recognisable without inventing a fake app. The tag list lives in the
// build_target JSON column (deploy_config carries only one tag's worth of
// space, and we need N).
func CreateBuildImageToken(token, uploadID, repo string, tags []string, dockerfile string, buildArgs []BuildArg, expiresAt time.Time) error {
	deployConfig := repo + ":" + uploadID

	buildArgsJSON := []byte("[]")
	if len(buildArgs) > 0 {
		encoded, err := json.Marshal(buildArgs)
		if err != nil {
			return fmt.Errorf("marshal build args: %w", err)
		}
		buildArgsJSON = encoded
	}

	targetJSON, err := json.Marshal(BuildTargetSpec{Repo: repo, Tags: tags})
	if err != nil {
		return fmt.Errorf("marshal build target: %w", err)
	}

	query := `
		INSERT INTO upload_tokens (token, deploy_config, dockerfile, build_args, build_target, expires_at, purpose)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`
	_, err = db.Exec(query, token, deployConfig, dockerfile, string(buildArgsJSON), string(targetJSON), expiresAt, PurposeBuildImage)
	return err
}

// CreatePullToken creates a new single-use token for downloading a stored archive.
func CreatePullToken(token, osid, cliUploadID string, expiresAt time.Time) error {
	query := `
		INSERT INTO upload_tokens (token, deploy_config, expires_at, purpose)
		VALUES (?, ?, ?, ?)
	`
	_, err := db.Exec(query, token, osid+":"+cliUploadID, expiresAt, PurposePull)
	return err
}

// GetUploadToken retrieves an upload-purpose (app deploy) token by value.
func GetUploadToken(token string) (*UploadToken, error) {
	return getTokenByPurpose(token, PurposeUpload)
}

// GetPullToken retrieves a pull-purpose token by its value.
func GetPullToken(token string) (*UploadToken, error) {
	return getTokenByPurpose(token, PurposePull)
}

// GetUploadableToken retrieves a token usable at the /cli-deploy upload
// endpoint: either an app-deploy upload or an app-less build-image token
// (obj-47). The shared handler branches on the returned Purpose. Keeping
// pull tokens out (they are consumed by the pull endpoint) preserves the
// single-use purpose isolation the original split lookup gave.
func GetUploadableToken(token string) (*UploadToken, error) {
	return getTokenByPurposes(token, PurposeUpload, PurposeBuildImage)
}

func getTokenByPurpose(token string, purpose string) (*UploadToken, error) {
	return getTokenByPurposes(token, purpose)
}

func getTokenByPurposes(token string, purposes ...string) (*UploadToken, error) {
	if len(purposes) == 0 {
		return nil, fmt.Errorf("no purpose specified")
	}
	placeholders := make([]string, len(purposes))
	args := make([]any, 0, len(purposes)+1)
	args = append(args, token)
	for i, p := range purposes {
		placeholders[i] = "?"
		args = append(args, p)
	}
	query := fmt.Sprintf(`
		SELECT id, token, deploy_config, dockerfile, build_args, build_target, expires_at, used, created_at, purpose
		FROM upload_tokens
		WHERE token = ? AND purpose IN (%s)
	`, strings.Join(placeholders, ", "))

	var t UploadToken
	var usedInt int
	var buildArgsJSON string
	var buildTargetJSON sql.NullString
	err := db.QueryRow(query, args...).Scan(
		&t.ID,
		&t.Token,
		&t.DeployConfig,
		&t.Dockerfile,
		&buildArgsJSON,
		&buildTargetJSON,
		&t.ExpiresAt,
		&usedInt,
		&t.CreatedAt,
		&t.Purpose,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("token not found")
		}
		return nil, err
	}
	t.Used = usedInt != 0
	// Empty/legacy rows store '[]' (column default); only unmarshal when
	// there's something to parse so a blank value can't error the lookup.
	if buildArgsJSON != "" && buildArgsJSON != "[]" {
		if err := json.Unmarshal([]byte(buildArgsJSON), &t.BuildArgs); err != nil {
			return nil, fmt.Errorf("unmarshal build args for token: %w", err)
		}
	}
	// build_target is set only for PurposeBuildImage tokens; older rows and
	// app-deploy/pull tokens leave it empty/'' (NULL on pre-migration rows).
	if bt := strings.TrimSpace(buildTargetJSON.String); buildTargetJSON.Valid && bt != "" && bt != "{}" {
		var spec BuildTargetSpec
		if err := json.Unmarshal([]byte(bt), &spec); err != nil {
			return nil, fmt.Errorf("unmarshal build target for token: %w", err)
		}
		t.BuildTarget = &spec
	}
	return &t, nil
}

// MarkUploadTokenUsed atomically marks a token (any purpose) as used.
// Returns error if token doesn't exist or is already used.
func MarkUploadTokenUsed(token string) error {
	query := `
		UPDATE upload_tokens
		SET used = 1
		WHERE token = ? AND used = 0
	`
	result, err := db.Exec(query, token)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("token not found or already used")
	}

	return nil
}

// DeleteExpiredUploadTokens removes tokens (any purpose) that have expired.
func DeleteExpiredUploadTokens() error {
	query := `
		DELETE FROM upload_tokens
		WHERE expires_at < CURRENT_TIMESTAMP
	`
	_, err := db.Exec(query)
	return err
}
