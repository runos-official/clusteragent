package agentstream

// Deployment stage recheck: the bounded, correlated request-response client
// that lets the cluster agent ask Nodeward/Conductor for a current,
// authoritative permit before an upload or queued stage mutates anything.
//
// The wire shapes here are the published contract (handbook articles 950 and
// 963): the agent envelope reuses the existing Type/JsonB64/Tag fields
// (no protobuf change), and the JsonB64 payload is the exact
// runos.deployment-stage-request.v1 / runos.deployment-stage-response.v1
// JSON the handbook defines. This file only builds and validates that
// payload; it does not evaluate authority itself (no grant evaluator lives
// here) and it makes exactly one request per call — no retry. A caller that
// wants another attempt passes a fresh retryId.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
)

// RecheckStage identifies which deployment pipeline stage requests the
// recheck (handbook article 950, Stage).
type RecheckStage string

const (
	StageArchivePersist  RecheckStage = "archive_persist"
	StageSourceFetch     RecheckStage = "source_fetch"
	StageQueueAdmission  RecheckStage = "queue_admission"
	StageBuildStart      RecheckStage = "build_start"
	StageDeploymentPatch RecheckStage = "deployment_patch"
)

// RecheckOutcome is the authoritative decision outcome (handbook article 950,
// Outcome). Only OutcomePermit may continue past the admission boundary.
type RecheckOutcome string

const (
	OutcomePermit      RecheckOutcome = "permit"
	OutcomeDeny        RecheckOutcome = "deny"
	OutcomeNeeds       RecheckOutcome = "needs"
	OutcomeUnavailable RecheckOutcome = "unavailable"
)

const (
	stageRequestVersion  = "runos.deployment-stage-request.v1"
	stageResponseVersion = "runos.deployment-stage-response.v1"
	recheckRequestType   = "DeploymentStageRecheckToServer"
	recheckResponseType  = "DeploymentStageRecheckToAgent"

	// recheckTimeout bounds the whole round trip at the published five-second
	// total monotonic callback deadline (handbook article 963): a stalled
	// reply fails closed as "unavailable" instead of hanging the caller.
	recheckTimeout = 5 * time.Second

	// recheckProofLifetime is the published ceiling on a response's own
	// issuedAt..expiresAt window ("Proof lifetime cannot exceed five
	// seconds"). recheckClockSkew is the published allowance ("Allow
	// one-second clock skew for proof validation"); it never extends expiry.
	recheckProofLifetime = 5 * time.Second
	recheckClockSkew     = 1 * time.Second
)

var lowercaseUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// RecheckAuthority is the discriminated authority reference the published
// contract requires (handbook article 950, Authority): exactly one of the
// two branches is populated, selected by Kind. The zero value is invalid.
type RecheckAuthority struct {
	Kind      string // "upload" or "job"
	BindingID string // kind=upload only
	UploadID  string // kind=upload only
	JobID     string // kind=job only
}

// UploadAuthority builds the kind=upload branch used for the archive_persist
// stage: the immutable upload binding plus the upload identity it governs.
func UploadAuthority(bindingID, uploadID string) RecheckAuthority {
	return RecheckAuthority{Kind: "upload", BindingID: bindingID, UploadID: uploadID}
}

// JobAuthority builds the kind=job branch used for every other stage
// (source_fetch, queue_admission, build_start, deployment_patch).
func JobAuthority(jobID string) RecheckAuthority {
	return RecheckAuthority{Kind: "job", JobID: jobID}
}

func (a RecheckAuthority) validate() error {
	switch a.Kind {
	case "upload":
		if a.JobID != "" {
			return fmt.Errorf("upload authority must not carry a jobId")
		}
		if !lowercaseUUIDPattern.MatchString(a.BindingID) {
			return fmt.Errorf("invalid authority bindingId")
		}
		if !lowercaseUUIDPattern.MatchString(a.UploadID) {
			return fmt.Errorf("invalid authority uploadId")
		}
		return nil
	case "job":
		if a.BindingID != "" || a.UploadID != "" {
			return fmt.Errorf("job authority must not carry bindingId/uploadId")
		}
		if !lowercaseUUIDPattern.MatchString(a.JobID) {
			return fmt.Errorf("invalid authority jobId")
		}
		return nil
	default:
		return fmt.Errorf("invalid authority kind %q", a.Kind)
	}
}

// MarshalJSON emits exactly the closed shape for the selected branch: the
// published schema sets additionalProperties:false per oneOf arm, so the
// unused branch's fields must be absent, not present as empty strings.
func (a RecheckAuthority) MarshalJSON() ([]byte, error) {
	switch a.Kind {
	case "upload":
		return json.Marshal(struct {
			Kind      string `json:"kind"`
			BindingID string `json:"bindingId"`
			UploadID  string `json:"uploadId"`
		}{"upload", a.BindingID, a.UploadID})
	case "job":
		return json.Marshal(struct {
			Kind  string `json:"kind"`
			JobID string `json:"jobId"`
		}{"job", a.JobID})
	default:
		return nil, fmt.Errorf("invalid authority kind %q", a.Kind)
	}
}

// UnmarshalJSON enforces the closed per-branch shape on the way in: an
// upload authority carrying jobId, or a job authority carrying
// bindingId/uploadId, is rejected rather than silently accepted.
func (a *RecheckAuthority) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode authority: %w", err)
	}
	kindRaw, ok := fields["kind"]
	if !ok {
		return fmt.Errorf("authority requires kind")
	}
	var kind string
	if err := json.Unmarshal(kindRaw, &kind); err != nil {
		return fmt.Errorf("invalid authority kind: %w", err)
	}

	decodeID := func(key string) (string, error) {
		raw, ok := fields[key]
		if !ok {
			return "", fmt.Errorf("authority kind %s requires %s", kind, key)
		}
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", fmt.Errorf("invalid authority %s: %w", key, err)
		}
		return id, nil
	}

	switch kind {
	case "upload":
		for key := range fields {
			if key != "kind" && key != "bindingId" && key != "uploadId" {
				return fmt.Errorf("authority kind upload has unexpected field %q", key)
			}
		}
		bindingID, err := decodeID("bindingId")
		if err != nil {
			return err
		}
		uploadID, err := decodeID("uploadId")
		if err != nil {
			return err
		}
		decoded := RecheckAuthority{Kind: "upload", BindingID: bindingID, UploadID: uploadID}
		if err := decoded.validate(); err != nil {
			return err
		}
		*a = decoded
		return nil
	case "job":
		for key := range fields {
			if key != "kind" && key != "jobId" {
				return fmt.Errorf("authority kind job has unexpected field %q", key)
			}
		}
		jobID, err := decodeID("jobId")
		if err != nil {
			return err
		}
		decoded := RecheckAuthority{Kind: "job", JobID: jobID}
		if err := decoded.validate(); err != nil {
			return err
		}
		*a = decoded
		return nil
	default:
		return fmt.Errorf("unknown authority kind %q", kind)
	}
}

// stageForAuthority reports whether stage requires an upload or a job
// authority (handbook article 950, StageRequest's allOf): archive_persist
// is upload, every other stage is job.
func stageWantsUploadAuthority(stage RecheckStage) bool {
	return stage == StageArchivePersist
}

func validateStageForAuthority(stage RecheckStage, authority RecheckAuthority) error {
	switch stage {
	case StageArchivePersist, StageSourceFetch, StageQueueAdmission, StageBuildStart, StageDeploymentPatch:
	default:
		return fmt.Errorf("invalid stage %q", stage)
	}
	wantUpload := stageWantsUploadAuthority(stage)
	if wantUpload && authority.Kind != "upload" {
		return fmt.Errorf("stage %q requires an upload authority, got %q", stage, authority.Kind)
	}
	if !wantUpload && authority.Kind != "job" {
		return fmt.Errorf("stage %q requires a job authority, got %q", stage, authority.Kind)
	}
	return nil
}

// stageRequestWire is the exact runos.deployment-stage-request.v1 shape.
type stageRequestWire struct {
	Version   string           `json:"version"`
	Authority RecheckAuthority `json:"authority"`
	Stage     RecheckStage     `json:"stage"`
	RetryID   string           `json:"retryId"`
}

// StageResponse is the exact runos.deployment-stage-response.v1 shape,
// decoded and validated against the request that produced it.
type StageResponse struct {
	Authority     RecheckAuthority
	Stage         RecheckStage
	RetryID       string
	RequestID     string
	Outcome       RecheckOutcome
	Code          string
	DecisionID    string
	PolicyVersion int64
	IssuedAt      time.Time
	ExpiresAt     time.Time

	// StreamGeneration is CurrentStreamGeneration() captured at the moment
	// this response was validated (review finding 1169). The request and
	// this reply are correlated by tag on whichever stream was current at
	// send time; a disconnect and reconnect between then and when a caller
	// acts on the permit would otherwise be invisible, because the
	// response payload itself carries no connection identity. A caller
	// that compares this value against a fresh CurrentStreamGeneration()
	// before acting on the permit detects that gap.
	StreamGeneration uint64
}

// RequestStageRecheck sends one bounded, correlated recheck request over the
// authenticated control stream and validates the reply against the
// published contract. It is the production entry point; tests drive
// requestStageRecheckWith directly with a fake transport.
func RequestStageRecheck(ctx context.Context, authority RecheckAuthority, stage RecheckStage, retryID string) (*StageResponse, error) {
	return requestStageRecheckWith(ctx, SendAndWaitForResponse, authority, stage, retryID)
}

// requestStageRecheckWith is RequestStageRecheck with the transport
// injected, so tests can supply a fake request-response function without a
// live stream (plan: "Inject the clock and recheck client for deterministic
// tests"). send must match agentstream.SendAndWaitForResponse's signature.
func requestStageRecheckWith(
	ctx context.Context,
	send func(context.Context, *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, error),
	authority RecheckAuthority,
	stage RecheckStage,
	retryID string,
) (*StageResponse, error) {
	if err := authority.validate(); err != nil {
		return nil, fmt.Errorf("invalid authority: %w", err)
	}
	if !lowercaseUUIDPattern.MatchString(retryID) {
		return nil, fmt.Errorf("invalid retryId")
	}
	if err := validateStageForAuthority(stage, authority); err != nil {
		return nil, err
	}

	req := stageRequestWire{
		Version:   stageRequestVersion,
		Authority: authority,
		Stage:     stage,
		RetryID:   retryID,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal stage request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()

	// Review finding 1245 (repeat of 1169): sample the generation BEFORE the
	// send, not after the reply is decoded. agent.go's reply delivery writes
	// into a buffered, capacity-one tag channel and returns; a buffered
	// value stays readable even after establishAndMaintainStream closes that
	// channel on disconnect, so a reply legitimately sent on generation N
	// can still be read successfully after the stream has already moved to
	// generation N+1. Reading the generation only after that point would
	// record N+1 - the POST-disconnect value - erasing the only signal that
	// a disconnect happened at all.
	genAtSend := CurrentStreamGeneration()

	reply, err := send(reqCtx, &l2sec.FromClusterAgent{
		Type:    recheckRequestType,
		JsonB64: base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		return nil, fmt.Errorf("deployment stage recheck unavailable: %w", err)
	}

	resp, err := decodeStageResponse(reply, authority, stage, retryID)
	if err != nil {
		return nil, err
	}
	// If the generation moved at all between send and here, the reply may
	// have been read from a buffer across a disconnect (or a reconnect)
	// that happened while it was in flight: refuse rather than stamp a
	// generation that can no longer prove which connection the decision was
	// actually made for.
	if CurrentStreamGeneration() != genAtSend {
		return nil, fmt.Errorf("deployment stage recheck: control stream disconnected or reconnected while the reply was in flight")
	}
	resp.StreamGeneration = genAtSend
	return resp, nil
}

// decodeStageResponse validates the transport envelope, then the decoded
// payload's schema and its correlation with the request that produced it.
// Every check runs before the caller can treat the outcome as authoritative.
func decodeStageResponse(reply *l2sec.ToClusterAgent, wantAuthority RecheckAuthority, wantStage RecheckStage, wantRetryID string) (*StageResponse, error) {
	if reply == nil {
		return nil, fmt.Errorf("deployment stage recheck: empty reply")
	}
	// Reject a response with the wrong message type before decoding its
	// payload — this also rejects a control-plane ERROR response, which
	// carries no StageResponse payload at all.
	if reply.Type != recheckResponseType {
		return nil, fmt.Errorf("deployment stage recheck: unexpected response type %q", reply.Type)
	}

	raw, err := base64.StdEncoding.DecodeString(reply.JsonB64)
	if err != nil {
		return nil, fmt.Errorf("deployment stage recheck: malformed base64: %w", err)
	}

	decoded, err := decodeStageResponseJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("deployment stage recheck: %w", err)
	}

	if decoded.Authority != wantAuthority {
		return nil, fmt.Errorf("deployment stage recheck: response authority does not match the request")
	}
	if decoded.Stage != wantStage {
		return nil, fmt.Errorf("deployment stage recheck: response stage %q does not match requested %q", decoded.Stage, wantStage)
	}
	if decoded.RetryID != wantRetryID {
		return nil, fmt.Errorf("deployment stage recheck: response retryId does not match the request")
	}
	if !lowercaseUUIDPattern.MatchString(decoded.RequestID) {
		return nil, fmt.Errorf("deployment stage recheck: invalid response requestId")
	}

	// Handbook 963: "Proof lifetime cannot exceed five seconds. Allow
	// one-second clock skew for proof validation. Clock skew never extends
	// authority expiry." Review finding 1076: the published ceiling is
	// exactly recheckProofLifetime, and expiry is a hard deadline that skew
	// must never push back - skew is allowed only when judging issuedAt
	// against the local clock, never added to the lifetime cap or to
	// expiresAt. The prior version added recheckClockSkew to both,
	// accepting a proof that was already stale by up to one second and a
	// lifetime one second over the published cap.
	now := time.Now()
	if !decoded.ExpiresAt.After(decoded.IssuedAt) {
		return nil, fmt.Errorf("deployment stage recheck: response expiresAt must follow issuedAt")
	}
	if decoded.ExpiresAt.Sub(decoded.IssuedAt) > recheckProofLifetime {
		return nil, fmt.Errorf("deployment stage recheck: response lifetime exceeds the published five-second ceiling")
	}
	if decoded.IssuedAt.After(now.Add(recheckClockSkew)) {
		return nil, fmt.Errorf("deployment stage recheck: response issuedAt is in the future beyond the allowed clock skew")
	}
	// Review finding 1128: "Treat equality with expiresAt as expired." A
	// proof is fresh only strictly before its own deadline.
	if !now.Before(decoded.ExpiresAt) {
		return nil, fmt.Errorf("deployment stage recheck: response is stale or already expired")
	}

	return decoded, nil
}

// requiredStageResponseFields mirrors handbook article 950's StageResponse
// required list. decisionId and policyVersion are required KEYS whose
// VALUE may be null outside outcome=permit, so they are checked for
// presence here and for non-null separately once outcome is known.
var requiredStageResponseFields = []string{
	"version", "authority", "stage", "retryId", "requestId",
	"outcome", "code", "decisionId", "policyVersion", "issuedAt", "expiresAt",
}

// decodeStageResponseJSON strictly decodes the payload bytes: unknown
// fields, trailing data, and duplicate top-level keys from the decoder's own
// pass are rejected, mirroring datastore.UploadAuthorityBinding's decode
// discipline for the same reason (this is also an authority document).
func decodeStageResponseJSON(raw []byte) (*StageResponse, error) {
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	var probe struct {
		Version       string           `json:"version"`
		Authority     RecheckAuthority `json:"authority"`
		Stage         RecheckStage     `json:"stage"`
		RetryID       string           `json:"retryId"`
		RequestID     string           `json:"requestId"`
		Outcome       string           `json:"outcome"`
		Code          string           `json:"code"`
		DecisionID    *string          `json:"decisionId"`
		PolicyVersion *int64           `json:"policyVersion"`
		IssuedAt      string           `json:"issuedAt"`
		ExpiresAt     string           `json:"expiresAt"`
	}
	if err := strict.Decode(&probe); err != nil {
		return nil, fmt.Errorf("decode stage response: %w", err)
	}
	var trailing any
	if err := strict.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode stage response: trailing data")
		}
		return nil, fmt.Errorf("decode stage response: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode stage response: %w", err)
	}
	for _, name := range requiredStageResponseFields {
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("stage response requires %s", name)
		}
	}

	if probe.Version != stageResponseVersion {
		return nil, fmt.Errorf("unsupported stage response version %q", probe.Version)
	}
	if err := probe.Authority.validate(); err != nil {
		return nil, fmt.Errorf("invalid response authority: %w", err)
	}
	if err := validateStageForAuthority(probe.Stage, probe.Authority); err != nil {
		return nil, fmt.Errorf("invalid response stage/authority pairing: %w", err)
	}
	if !lowercaseUUIDPattern.MatchString(probe.RetryID) {
		return nil, fmt.Errorf("invalid response retryId")
	}

	outcome := RecheckOutcome(probe.Outcome)
	switch outcome {
	case OutcomePermit, OutcomeDeny, OutcomeNeeds, OutcomeUnavailable:
	default:
		return nil, fmt.Errorf("unknown response outcome %q", probe.Outcome)
	}
	if probe.Code == "" {
		return nil, fmt.Errorf("stage response code must not be empty")
	}

	var decisionID string
	var policyVersion int64
	if outcome == OutcomePermit {
		if probe.DecisionID == nil || *probe.DecisionID == "" {
			return nil, fmt.Errorf("a permit response requires a non-null decisionId")
		}
		if probe.PolicyVersion == nil {
			return nil, fmt.Errorf("a permit response requires a non-null policyVersion")
		}
		decisionID = *probe.DecisionID
		policyVersion = *probe.PolicyVersion
		if policyVersion < 0 {
			return nil, fmt.Errorf("invalid response policyVersion")
		}
	} else {
		if probe.DecisionID != nil {
			decisionID = *probe.DecisionID
		}
		if probe.PolicyVersion != nil {
			policyVersion = *probe.PolicyVersion
		}
	}

	issuedAt, err := parseContractTimestamp(probe.IssuedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid response issuedAt: %w", err)
	}
	expiresAt, err := parseContractTimestamp(probe.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("invalid response expiresAt: %w", err)
	}

	return &StageResponse{
		Authority:     probe.Authority,
		Stage:         probe.Stage,
		RetryID:       probe.RetryID,
		RequestID:     probe.RequestID,
		Outcome:       outcome,
		Code:          probe.Code,
		DecisionID:    decisionID,
		PolicyVersion: policyVersion,
		IssuedAt:      issuedAt,
		ExpiresAt:     expiresAt,
	}, nil
}

// parseContractTimestamp requires the published UTC RFC 3339 "Z" form
// (handbook article 950, Time: pattern "Z$"), matching
// datastore.parseContractTime's discipline for the same field shape.
func parseContractTimestamp(value string) (time.Time, error) {
	if len(value) == 0 || value[len(value)-1] != 'Z' {
		return time.Time{}, fmt.Errorf("must use UTC RFC 3339 ending in Z")
	}
	return time.Parse(time.RFC3339Nano, value)
}
