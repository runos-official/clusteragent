package agentstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/runos-official/clusteragent/agentstream/l2sec"
)

const (
	testBindingID = "00000000-0000-4000-8000-000000000003"
	testUploadID  = "00000000-0000-4000-8000-000000000004"
	testRetryID   = "00000000-0000-4000-8000-000000000005"
	testRequestID = "00000000-0000-4000-8000-000000000006"
)

// fakeRequestResponse builds a stub for the injectable send function. It
// records the outgoing message and returns a canned reply plus a live
// test Lifetime, mirroring
// agentstream.SendAndWaitForResponseWithLifetime's signature exactly so
// requestStageRecheckWith can take either.
func fakeRequestResponse(t *testing.T, reply *l2sec.ToClusterAgent, err error) (func(context.Context, *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, *Lifetime, error), *[]*l2sec.FromClusterAgent) {
	t.Helper()
	var sent []*l2sec.FromClusterAgent
	fn := func(_ context.Context, msg *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, *Lifetime, error) {
		sent = append(sent, msg)
		if err != nil {
			return nil, nil, err
		}
		return reply, NewTestLifetime(true), nil
	}
	return fn, &sent
}

func validStageResponseJSON(t *testing.T, authority RecheckAuthority, stage RecheckStage, retryID, outcome string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	payload := map[string]any{
		"version":   stageResponseVersion,
		"authority": authority,
		"stage":     stage,
		"retryId":   retryID,
		"requestId": testRequestID,
		"outcome":   outcome,
		"code":      "ok",
		"issuedAt":  now.Format(time.RFC3339Nano),
		"expiresAt": now.Add(3 * time.Second).Format(time.RFC3339Nano),
	}
	if outcome == string(OutcomePermit) {
		payload["decisionId"] = "decision-1"
		payload["policyVersion"] = 7
	} else {
		payload["decisionId"] = nil
		payload["policyVersion"] = nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal stage response fixture: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func wrapResponse(jsonB64, tag string) *l2sec.ToClusterAgent {
	return &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: jsonB64, Tag: tag}
}

// TestRequestStageRecheck_CapturesDeliveringLifetime pins review finding
// 1313/1169/1245: a validated response must carry the Lifetime that
// delivered it (L4), so a caller can later detect a disconnect/reconnect
// that happened since by checking Live(). Replaces
// TestRequestStageRecheck_CapturesCurrentStreamGeneration (generation
// counter replaced by Lifetime).
func TestRequestStageRecheck_CapturesDeliveringLifetime(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	b64 := validStageResponseJSON(t, authority, StageArchivePersist, testRetryID, string(OutcomePermit))
	wantLifetime := NewTestLifetime(true)
	send := func(_ context.Context, msg *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, *Lifetime, error) {
		return wrapResponse(b64, "whatever"), wantLifetime, nil
	}

	resp, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Lifetime != wantLifetime {
		t.Fatalf("Lifetime = %p, want the one send delivered on: %p", resp.Lifetime, wantLifetime)
	}
}

// The other half of review finding 1245/1169 (repeated as 1313) - that a
// reply must never be accepted once the Lifetime it was sent on has ended,
// even though it arrived successfully through a buffered channel - is now
// enforced once, generically, by sendAndWait itself (see
// agent_send_test.go's TestSendAndWait_RefusesReplyAfterLifetimeEnded),
// not by requestStageRecheckWith. This replaces the old
// TestRequestStageRecheck_RefusesWhenGenerationMovesDuringSend, which
// exercised that check at this layer back when it lived here.

// TestRequestStageRecheck_RefusesWhenRecheckDeadlinePassedBeforeAcceptingReply
// pins review finding 1271: a Go select with both a ready reply and a
// ready ctx.Done() can pick either case, so the transport can return a
// valid, correctly-correlated reply with a nil error even though the
// recheck's own five-second deadline already passed (the caller's wider
// HTTP context can still be live). This models that exact race: the
// injected send waits for its context to actually expire, then returns
// the reply anyway with a nil error, exactly like sendAndWait's second
// select choosing the response case despite ctx also being ready.
func TestRequestStageRecheck_RefusesWhenRecheckDeadlinePassedBeforeAcceptingReply(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	b64 := validStageResponseJSON(t, authority, StageArchivePersist, testRetryID, string(OutcomePermit))
	reply := wrapResponse(b64, "whatever")

	send := func(ctx context.Context, _ *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, *Lifetime, error) {
		<-ctx.Done() // the recheck's own deadline passes before the reply is "accepted"
		return reply, NewTestLifetime(true), nil
	}

	// A short parent context, so reqCtx (a child bounded by recheckTimeout
	// too) expires quickly instead of the test waiting a full 5s.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	resp, err := requestStageRecheckWith(ctx, send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatalf("expected a refusal once the recheck's own deadline passed before the reply was accepted, got permit %+v", resp)
	}
	if resp != nil {
		t.Error("expected no response alongside the refusal")
	}
}

func TestRequestStageRecheck_SendsExactEnvelope(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	b64 := validStageResponseJSON(t, authority, StageArchivePersist, testRetryID, string(OutcomePermit))
	send, sent := fakeRequestResponse(t, wrapResponse(b64, "whatever"), nil)

	resp, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != OutcomePermit {
		t.Fatalf("outcome = %q, want permit", resp.Outcome)
	}
	if len(*sent) != 1 {
		t.Fatalf("expected exactly one send, got %d", len(*sent))
	}
	msg := (*sent)[0]
	if msg.Type != recheckRequestType {
		t.Fatalf("request Type = %q, want %q", msg.Type, recheckRequestType)
	}
	raw, err := base64.StdEncoding.DecodeString(msg.JsonB64)
	if err != nil {
		t.Fatalf("request JsonB64 is not valid base64: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("request payload is not valid JSON: %v", err)
	}
	if decoded["version"] != stageRequestVersion {
		t.Fatalf("request version = %v, want %q", decoded["version"], stageRequestVersion)
	}
	if decoded["stage"] != string(StageArchivePersist) {
		t.Fatalf("request stage = %v, want archive_persist", decoded["stage"])
	}
	if decoded["retryId"] != testRetryID {
		t.Fatalf("request retryId = %v, want %q", decoded["retryId"], testRetryID)
	}
	auth, ok := decoded["authority"].(map[string]any)
	if !ok {
		t.Fatalf("request authority is not an object: %v", decoded["authority"])
	}
	if auth["kind"] != "upload" || auth["bindingId"] != testBindingID || auth["uploadId"] != testUploadID {
		t.Fatalf("request authority = %v, want upload/%s/%s", auth, testBindingID, testUploadID)
	}
	if _, present := auth["jobId"]; present {
		t.Fatalf("upload authority must not carry jobId, got %v", auth)
	}
}

func TestRequestStageRecheck_JobAuthorityShape(t *testing.T) {
	authority := JobAuthority(testUploadID)
	b64 := validStageResponseJSON(t, authority, StageBuildStart, testRetryID, string(OutcomeDeny))
	send, sent := fakeRequestResponse(t, wrapResponse(b64, "whatever"), nil)

	resp, err := requestStageRecheckWith(context.Background(), send, authority, StageBuildStart, testRetryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != OutcomeDeny {
		t.Fatalf("outcome = %q, want deny", resp.Outcome)
	}
	raw, _ := base64.StdEncoding.DecodeString((*sent)[0].JsonB64)
	var decoded map[string]any
	json.Unmarshal(raw, &decoded)
	auth := decoded["authority"].(map[string]any)
	if auth["kind"] != "job" || auth["jobId"] != testUploadID {
		t.Fatalf("job authority = %v, want job/%s", auth, testUploadID)
	}
	if _, present := auth["bindingId"]; present {
		t.Fatalf("job authority must not carry bindingId, got %v", auth)
	}
}

func TestRequestStageRecheck_RejectsErrorResponse(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: "ERROR", JsonB64: ""}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for a control-plane ERROR response")
	}
}

func TestRequestStageRecheck_RejectsWrongResponseType(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: "SOME_OTHER_TYPE", JsonB64: "e30="}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for an unexpected response type")
	}
}

func TestRequestStageRecheck_RejectsMalformedBase64(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: "not-base64!!"}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for malformed base64")
	}
}

func TestRequestStageRecheck_RejectsMalformedJSON(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	b64 := base64.StdEncoding.EncodeToString([]byte("{not json"))
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestRequestStageRecheck_RejectsUnknownFields(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	now := time.Now().UTC()
	payload := map[string]any{
		"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
		"retryId": testRetryID, "requestId": testRequestID, "outcome": "deny", "code": "ok",
		"decisionId": nil, "policyVersion": nil,
		"issuedAt": now.Format(time.RFC3339Nano), "expiresAt": now.Add(time.Second).Format(time.RFC3339Nano),
		"unexpectedField": "surprise",
	}
	raw, _ := json.Marshal(payload)
	b64 := base64.StdEncoding.EncodeToString(raw)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for an unknown field in the decoded payload")
	}
}

func TestRequestStageRecheck_RejectsMissingRequiredField(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	now := time.Now().UTC()
	payload := map[string]any{
		"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
		"retryId": testRetryID, "requestId": testRequestID, "outcome": "deny",
		// code is missing
		"decisionId": nil, "policyVersion": nil,
		"issuedAt": now.Format(time.RFC3339Nano), "expiresAt": now.Add(time.Second).Format(time.RFC3339Nano),
	}
	raw, _ := json.Marshal(payload)
	b64 := base64.StdEncoding.EncodeToString(raw)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for a missing required field")
	}
}

func TestRequestStageRecheck_RejectsBindingMismatch(t *testing.T) {
	sentAuthority := UploadAuthority(testBindingID, testUploadID)
	otherAuthority := UploadAuthority("00000000-0000-4000-8000-00000000ffff", testUploadID)
	b64 := validStageResponseJSON(t, otherAuthority, StageArchivePersist, testRetryID, string(OutcomePermit))
	send, _ := fakeRequestResponse(t, wrapResponse(b64, "whatever"), nil)

	_, err := requestStageRecheckWith(context.Background(), send, sentAuthority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error when the response authority does not match the request")
	}
}

func TestRequestStageRecheck_RejectsStageMismatch(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	b64 := validStageResponseJSON(t, authority, StageBuildStart, testRetryID, string(OutcomePermit))
	send, _ := fakeRequestResponse(t, wrapResponse(b64, "whatever"), nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error when the response stage does not match the request")
	}
}

func TestRequestStageRecheck_RejectsRetryIDMismatch(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	b64 := validStageResponseJSON(t, authority, StageArchivePersist, "00000000-0000-4000-8000-00000000dead", string(OutcomePermit))
	send, _ := fakeRequestResponse(t, wrapResponse(b64, "whatever"), nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error when the response retryId does not match the request")
	}
}

func TestRequestStageRecheck_RejectsUnknownOutcome(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	now := time.Now().UTC()
	payload := map[string]any{
		"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
		"retryId": testRetryID, "requestId": testRequestID, "outcome": "granted", "code": "ok",
		"decisionId": nil, "policyVersion": nil,
		"issuedAt": now.Format(time.RFC3339Nano), "expiresAt": now.Add(time.Second).Format(time.RFC3339Nano),
	}
	raw, _ := json.Marshal(payload)
	b64 := base64.StdEncoding.EncodeToString(raw)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for an unknown outcome value")
	}
}

func TestRequestStageRecheck_PermitRequiresDecisionIdAndPolicyVersion(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	now := time.Now().UTC()
	payload := map[string]any{
		"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
		"retryId": testRetryID, "requestId": testRequestID, "outcome": "permit", "code": "ok",
		"decisionId": nil, "policyVersion": nil, // permit with no decision: must refuse
		"issuedAt": now.Format(time.RFC3339Nano), "expiresAt": now.Add(time.Second).Format(time.RFC3339Nano),
	}
	raw, _ := json.Marshal(payload)
	b64 := base64.StdEncoding.EncodeToString(raw)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for a permit with no decisionId/policyVersion")
	}
}

func TestRequestStageRecheck_RejectsStaleExpiry(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	now := time.Now().UTC()
	payload := map[string]any{
		"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
		"retryId": testRetryID, "requestId": testRequestID, "outcome": "permit", "code": "ok",
		"decisionId": "d1", "policyVersion": 1,
		"issuedAt":  now.Add(-10 * time.Second).Format(time.RFC3339Nano),
		"expiresAt": now.Add(-5 * time.Second).Format(time.RFC3339Nano), // already expired
	}
	raw, _ := json.Marshal(payload)
	b64 := base64.StdEncoding.EncodeToString(raw)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for an already-expired response")
	}
}

func TestRequestStageRecheck_RejectsLifetimeOverFiveSeconds(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	now := time.Now().UTC()
	payload := map[string]any{
		"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
		"retryId": testRetryID, "requestId": testRequestID, "outcome": "permit", "code": "ok",
		"decisionId": "d1", "policyVersion": 1,
		"issuedAt":  now.Format(time.RFC3339Nano),
		"expiresAt": now.Add(30 * time.Second).Format(time.RFC3339Nano), // exceeds the 5s proof-lifetime ceiling
	}
	raw, _ := json.Marshal(payload)
	b64 := base64.StdEncoding.EncodeToString(raw)
	send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for a response lifetime over the published five-second ceiling")
	}
}

// TestRequestStageRecheck_ClockSkewNeverExtendsLifetimeOrExpiry pins review
// finding 1076: handbook 963 caps proof lifetime at exactly five seconds and
// says clock skew never extends authority expiry. The skew allowance applies
// only to judging issuedAt against the local clock. A lifetime or a
// staleness just inside the old (buggy) skew-extended ceiling must now
// refuse.
func TestRequestStageRecheck_ClockSkewNeverExtendsLifetimeOrExpiry(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)

	t.Run("lifetime 500ms over 5s refuses even though it was under the old 6s ceiling", func(t *testing.T) {
		now := time.Now().UTC()
		payload := map[string]any{
			"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
			"retryId": testRetryID, "requestId": testRequestID, "outcome": "permit", "code": "ok",
			"decisionId": "d1", "policyVersion": 1,
			"issuedAt":  now.Format(time.RFC3339Nano),
			"expiresAt": now.Add(5500 * time.Millisecond).Format(time.RFC3339Nano),
		}
		raw, _ := json.Marshal(payload)
		b64 := base64.StdEncoding.EncodeToString(raw)
		send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

		_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
		if err == nil {
			t.Fatal("expected refusal: lifetime exceeds the exact five-second ceiling")
		}
	})

	t.Run("expired 500ms ago refuses even though it was inside the old 1s skew grace", func(t *testing.T) {
		now := time.Now().UTC()
		payload := map[string]any{
			"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
			"retryId": testRetryID, "requestId": testRequestID, "outcome": "permit", "code": "ok",
			"decisionId": "d1", "policyVersion": 1,
			"issuedAt":  now.Add(-4500 * time.Millisecond).Format(time.RFC3339Nano),
			"expiresAt": now.Add(-500 * time.Millisecond).Format(time.RFC3339Nano),
		}
		raw, _ := json.Marshal(payload)
		b64 := base64.StdEncoding.EncodeToString(raw)
		send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

		_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
		if err == nil {
			t.Fatal("expected refusal: already expired, clock skew must not extend expiry")
		}
	})

	t.Run("issuedAt far in the future refuses", func(t *testing.T) {
		now := time.Now().UTC()
		payload := map[string]any{
			"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
			"retryId": testRetryID, "requestId": testRequestID, "outcome": "permit", "code": "ok",
			"decisionId": "d1", "policyVersion": 1,
			"issuedAt":  now.Add(10 * time.Second).Format(time.RFC3339Nano),
			"expiresAt": now.Add(14 * time.Second).Format(time.RFC3339Nano),
		}
		raw, _ := json.Marshal(payload)
		b64 := base64.StdEncoding.EncodeToString(raw)
		send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

		_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
		if err == nil {
			t.Fatal("expected refusal: issuedAt in the future beyond the allowed clock skew")
		}
	})

	t.Run("a legitimate response at the exact boundary still succeeds", func(t *testing.T) {
		now := time.Now().UTC()
		payload := map[string]any{
			"version": stageResponseVersion, "authority": authority, "stage": StageArchivePersist,
			"retryId": testRetryID, "requestId": testRequestID, "outcome": "permit", "code": "ok",
			"decisionId": "d1", "policyVersion": 1,
			"issuedAt":  now.Format(time.RFC3339Nano),
			"expiresAt": now.Add(5 * time.Second).Format(time.RFC3339Nano), // exactly the 5s ceiling
		}
		raw, _ := json.Marshal(payload)
		b64 := base64.StdEncoding.EncodeToString(raw)
		send, _ := fakeRequestResponse(t, &l2sec.ToClusterAgent{Type: recheckResponseType, JsonB64: b64}, nil)

		resp, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
		if err != nil {
			t.Fatalf("a legitimate, in-window permit must still succeed: %v", err)
		}
		if resp.Outcome != OutcomePermit {
			t.Fatalf("outcome = %q, want permit", resp.Outcome)
		}
	})
}

func TestRequestStageRecheck_TransportFailureIsUnavailable(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	send, _ := fakeRequestResponse(t, nil, errors.New("boom"))

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error when the transport call fails")
	}
}

func TestRequestStageRecheck_RejectsInvalidRetryID(t *testing.T) {
	authority := UploadAuthority(testBindingID, testUploadID)
	send, _ := fakeRequestResponse(t, nil, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, "not-a-uuid")
	if err == nil {
		t.Fatal("expected an error for a malformed retryId")
	}
}

func TestRequestStageRecheck_RejectsStageAuthorityMismatchBeforeSending(t *testing.T) {
	// archive_persist requires an upload authority; sending a job authority
	// for it is a programming error and must be caught before any send.
	authority := JobAuthority(testUploadID)
	send, sent := fakeRequestResponse(t, nil, nil)

	_, err := requestStageRecheckWith(context.Background(), send, authority, StageArchivePersist, testRetryID)
	if err == nil {
		t.Fatal("expected an error for archive_persist with a job authority")
	}
	if len(*sent) != 0 {
		t.Fatalf("expected no send for an invalid authority/stage pair, got %d", len(*sent))
	}
}

func TestRecheckAuthority_RoundTripsThroughJSON(t *testing.T) {
	for _, a := range []RecheckAuthority{
		UploadAuthority(testBindingID, testUploadID),
		JobAuthority(testUploadID),
	} {
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal %+v: %v", a, err)
		}
		var decoded RecheckAuthority
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if decoded != a {
			t.Fatalf("round-trip mismatch: got %+v, want %+v", decoded, a)
		}
	}
}

func TestRecheckAuthority_UnmarshalRejectsCrossFields(t *testing.T) {
	// kind=upload with a jobId, or kind=job with a bindingId/uploadId, must
	// refuse: the published schema is additionalProperties:false per branch.
	cases := []string{
		`{"kind":"upload","bindingId":"` + testBindingID + `","uploadId":"` + testUploadID + `","jobId":"` + testUploadID + `"}`,
		`{"kind":"job","jobId":"` + testUploadID + `","bindingId":"` + testBindingID + `"}`,
		`{"kind":"upload","bindingId":"` + testBindingID + `"}`, // missing uploadId
		`{"kind":"bogus","jobId":"` + testUploadID + `"}`,
	}
	for _, c := range cases {
		var a RecheckAuthority
		if err := json.Unmarshal([]byte(c), &a); err == nil {
			t.Fatalf("expected rejection for %s", c)
		}
	}
}
