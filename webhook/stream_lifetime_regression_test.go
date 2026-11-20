package webhook

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/harborclient"
	"google.golang.org/grpc/metadata"
)

// TestHandleCLIDeployUpload_StreamLifetime is THE regression for review
// finding 1313 (repeat of 1270/1240/1169): it drives the real production
// send, receive, and tag-dispatch path (agentstream.DriveTestStream plus
// the real, un-faked agentstream.RequestStageRecheck - every other test in
// this package fakes requestStageRecheck; this one deliberately does not)
// with the manager's own pendingResponses-drain cleanup held back by a
// pauseCleanup channel for the whole row, so each row proves the fix holds
// purely from the stream's own Lifetime, never from how fast - or whether
// at all - that drain runs.
//
// Rows (a)-(d) must see zero claim/archive/build effects and no HTTP 202.
// Row (e) proves a delayed, stale invalidation call from an old stream can
// never affect a live successor's Lifetime. Row (f) is the live, timely
// control: it must still proceed exactly once.
func TestHandleCLIDeployUpload_StreamLifetime(t *testing.T) {
	t.Run("a_closed_before_send", func(t *testing.T) {
		h := newRegressionHarness(t)
		ts := newRegressionStream(t)
		ts.fail(io.ErrClosedPipe) // the stream is already dead before admission ever sends

		w := h.upload(t, 2*time.Second)
		if w.Code == http.StatusAccepted {
			t.Fatalf("status = %d, want a refusal (stream already closed before send)", w.Code)
		}
		h.assertNoEffects(t)
	})

	t.Run("b_closed_while_reply_pending", func(t *testing.T) {
		h := newRegressionHarness(t)
		ts := newRegressionStream(t)

		resultCh := h.uploadAsync(t, 2*time.Second)
		req := ts.awaitSent(t)
		_ = req // the reply is deliberately never sent; the stream just dies
		ts.fail(io.ErrClosedPipe)

		w := <-resultCh
		if w.Code == http.StatusAccepted {
			t.Fatalf("status = %d, want a refusal (closed while the reply was pending)", w.Code)
		}
		h.assertNoEffects(t)
	})

	t.Run("c_closed_during_token_claim", func(t *testing.T) {
		h := newRegressionHarness(t)
		ts := newRegressionStream(t)
		h.claimHook = func() { ts.fail(io.ErrClosedPipe) }

		resultCh := h.uploadAsync(t, 2*time.Second)
		req := ts.awaitSent(t)
		ts.deliver(permitReplyFor(t, req))

		w := <-resultCh
		if w.Code == http.StatusAccepted {
			t.Fatalf("status = %d, want a refusal (stream closed during the claim)", w.Code)
		}
		// The claim itself succeeded before the stream died, so - unlike
		// rows a/b - the token IS claimed; only the archive/build must
		// never happen (review finding 1128's boundary).
		h.assertClaimedButNoArchiveOrBuild(t)
	})

	t.Run("d_closed_immediately_before_archive_entry", func(t *testing.T) {
		h := newRegressionHarness(t)
		ts := newRegressionStream(t)
		prevLog := logReceivedCLIUpload
		logReceivedCLIUpload = func(n int, osid, uploadID, token string) {
			ts.fail(io.ErrClosedPipe) // fail at the exact point L6 must still catch
			prevLog(n, osid, uploadID, token)
		}
		t.Cleanup(func() { logReceivedCLIUpload = prevLog })

		resultCh := h.uploadAsync(t, 2*time.Second)
		req := ts.awaitSent(t)
		ts.deliver(permitReplyFor(t, req))

		w := <-resultCh
		if w.Code == http.StatusAccepted {
			t.Fatalf("status = %d, want a refusal (stream closed during the log write immediately before archive entry)", w.Code)
		}
		h.assertClaimedButNoArchiveOrBuild(t)
	})

	t.Run("e_delayed_old_stream_callback_cannot_touch_successor", func(t *testing.T) {
		h := newRegressionHarness(t)

		ts1 := newRegressionStream(t)
		lt1 := ts1.lifetime
		ts1.fail(io.ErrClosedPipe)

		ts2 := newRegressionStream(t)
		lt2 := ts2.lifetime
		if lt2 == lt1 || !lt2.Live() {
			t.Fatalf("test bug: expected a fresh, live successor Lifetime")
		}

		// A delayed callback from the OLD stream - exactly what a slow
		// heartbeat-exhaustion goroutine produces in production - arrives
		// after the successor is already live. It must change nothing but
		// its own, already-superseded Lifetime.
		agentstream.EndTestLifetime(lt1)
		if agentstream.CurrentLifetime() != lt2 || !lt2.Live() {
			t.Fatal("the successor's Lifetime was affected by a delayed callback from the old stream")
		}

		// The successor must still be usable for an ordinary request.
		resultCh := h.uploadAsync(t, 2*time.Second)
		req := ts2.awaitSent(t)
		ts2.deliver(permitReplyFor(t, req))
		got := <-resultCh
		if got.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 on the live successor stream (body %q)", got.Code, got.Body.String())
		}
		h.assertClaimedPushedAndBuilt(t)
	})

	t.Run("f_live_timely_request_proceeds_exactly_once", func(t *testing.T) {
		h := newRegressionHarness(t)
		ts := newRegressionStream(t)

		resultCh := h.uploadAsync(t, 2*time.Second)
		req := ts.awaitSent(t)
		ts.deliver(permitReplyFor(t, req))

		w := <-resultCh
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
		}
		h.assertClaimedPushedAndBuilt(t)
	})
}

// --- fake stream -----------------------------------------------------------

// regressionStream is a minimal, fully test-controlled fake of the
// generated grpc.BidiStreamingClient[l2sec.FromClusterAgent,
// l2sec.ToClusterAgent] stream, used only to drive agentstream's real
// production send/receive/dispatch path (agentstream.DriveTestStream) with
// no live gRPC connection. Send publishes every outgoing message on sent
// so the test can react to it (craft and queue a matching reply, or fail
// the stream) at exactly the moment it chooses; Recv blocks until the test
// enqueues a reply or a failure.
type regressionStream struct {
	sent    chan *l2sec.FromClusterAgent
	replies chan *l2sec.ToClusterAgent
	failc   chan error

	t          *testing.T
	done       chan error
	pauseClean chan struct{}

	// lifetime is the Lifetime DriveTestStream published for this exact
	// stream, captured once right after publication. fail() polls THIS
	// value, not agentstream.CurrentLifetime() (which, in row e, has
	// already moved on to a successor stream by the time the old stream's
	// own fail() might still be settling).
	lifetime *agentstream.Lifetime
}

func (s *regressionStream) Send(m *l2sec.FromClusterAgent) error {
	s.sent <- m
	return nil
}
func (s *regressionStream) Recv() (*l2sec.ToClusterAgent, error) {
	select {
	case r := <-s.replies:
		return r, nil
	case err := <-s.failc:
		return nil, err
	}
}
func (s *regressionStream) Header() (metadata.MD, error) { return nil, nil }
func (s *regressionStream) Trailer() metadata.MD         { return nil }
func (s *regressionStream) CloseSend() error             { return nil }
func (s *regressionStream) Context() context.Context     { return context.Background() }
func (s *regressionStream) SendMsg(m any) error           { return nil }
func (s *regressionStream) RecvMsg(m any) error           { return nil }

// newRegressionStream publishes a fresh stream as the current Lifetime
// (via agentstream.DriveTestStream, so every goroutine it starts - listen,
// heartbeat - is the real production code) with its cleanup paused for the
// whole subtest: t.Cleanup only releases it (and waits for the stream to
// actually end and DriveTestStream to return) once the subtest is done.
// heartbeatInterval is set far longer than any test so the heartbeat never
// fires and interferes with a row's own timeline.
func newRegressionStream(t *testing.T) *regressionStream {
	t.Helper()
	s := &regressionStream{
		sent:       make(chan *l2sec.FromClusterAgent, 8),
		replies:    make(chan *l2sec.ToClusterAgent, 8),
		failc:      make(chan error, 1),
		t:          t,
		done:       make(chan error, 1),
		pauseClean: make(chan struct{}),
	}
	go func() {
		s.done <- agentstream.DriveTestStream(s, time.Hour, s.pauseClean)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if lt := agentstream.CurrentLifetime(); lt != nil && lt.Live() {
			s.lifetime = lt
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the test stream to publish its Lifetime")
		}
		time.Sleep(time.Millisecond)
	}

	t.Cleanup(func() {
		select {
		case s.failc <- io.ErrClosedPipe:
		default:
		}
		close(s.pauseClean)
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			t.Error("the test stream's DriveTestStream goroutine never returned")
		}
	})
	return s
}

// fail ends the stream (a Recv failure), simulating a disconnect, and
// blocks until its Lifetime has actually ended before returning - so a
// caller's next step never races the real listenForInstructions goroutine
// still being scheduled. Safe to call from any goroutine (including the
// HTTP handler's own, for rows c and d's hooks), so it reports a timeout
// with t.Errorf, never t.Fatal. The manager's cleanup (the pendingResponses
// drain) stays paused regardless - only t.Cleanup releases it.
func (s *regressionStream) fail(err error) {
	select {
	case s.failc <- err:
	default:
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.lifetime.Live() {
		if time.Now().After(deadline) {
			s.t.Errorf("timed out waiting for the stream's Lifetime to end after fail()")
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// deliver makes Recv return reply, so the real listenForInstructions loop
// dispatches it through handleInstruction exactly as production does.
func (s *regressionStream) deliver(reply *l2sec.ToClusterAgent) {
	s.replies <- reply
}

// awaitSent waits for the next outgoing message (the recheck request) and
// returns it, so the test can craft a correlated reply.
func (s *regressionStream) awaitSent(t *testing.T) *l2sec.FromClusterAgent {
	t.Helper()
	select {
	case m := <-s.sent:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the recheck request to be sent")
		return nil
	}
}

// permitReplyFor crafts a runos.deployment-stage-response.v1 PERMIT reply
// correlated to req (same authority/stage/retryId, a fresh requestId), the
// exact published shape agentstream.decodeStageResponse validates.
func permitReplyFor(t *testing.T, req *l2sec.FromClusterAgent) *l2sec.ToClusterAgent {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(req.JsonB64)
	if err != nil {
		t.Fatalf("decode sent request: %v", err)
	}
	var decoded struct {
		Authority json.RawMessage `json:"authority"`
		Stage     string          `json:"stage"`
		RetryID   string          `json:"retryId"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal sent request: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	payload := map[string]any{
		"version":       "runos.deployment-stage-response.v1",
		"authority":     decoded.Authority,
		"stage":         decoded.Stage,
		"retryId":       decoded.RetryID,
		"requestId":     uuid.New().String(),
		"outcome":       "permit",
		"code":          "ok",
		"decisionId":    "decision-1",
		"policyVersion": 1,
		"issuedAt":      now.Format(time.RFC3339Nano),
		"expiresAt":     now.Add(4 * time.Second).Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal permit reply: %v", err)
	}
	return &l2sec.ToClusterAgent{
		Type:    "DeploymentStageRecheckToAgent",
		JsonB64: base64.StdEncoding.EncodeToString(b),
		Tag:     req.Tag,
	}
}

// --- handler harness ---------------------------------------------------

// regressionHarness installs every HandleCLIDeployUpload collaborator
// EXCEPT requestStageRecheck, which stays the real, un-faked
// agentstream.RequestStageRecheck for the whole of this regression - that
// is the point of it.
type regressionHarness struct {
	t *testing.T

	mu         sync.Mutex
	claimed    []string
	pushed     bool
	cliStarted bool

	// claimHook, if set, runs synchronously inside claimUploadToken, before
	// it records the claim - so a row can fail the stream "during the
	// claim" deterministically.
	claimHook func()
}

func newRegressionHarness(t *testing.T) *regressionHarness {
	t.Helper()
	h := &regressionHarness{t: t}

	prevLoad, prevClaim, prevConfig, prevPush := loadUploadableToken, claimUploadToken, readBuildKitConfig, pushArchive
	prevCLI := startCLIDeploymentAsync
	prevBegin := beginWork
	prevRecordFailed := recordFailedUpload
	prevRecordFailedIfUnclaimed := recordFailedUploadIfUnclaimed
	t.Cleanup(func() {
		loadUploadableToken, claimUploadToken, readBuildKitConfig, pushArchive = prevLoad, prevClaim, prevConfig, prevPush
		startCLIDeploymentAsync = prevCLI
		beginWork = prevBegin
		recordFailedUpload = prevRecordFailed
		recordFailedUploadIfUnclaimed = prevRecordFailedIfUnclaimed
	})

	beginWork = func() (func(), error) { return func() {}, nil }
	recordFailedUpload = func(string, string, string) error { return nil }
	recordFailedUploadIfUnclaimed = func(string, string, string, string) error { return nil }

	loadUploadableToken = func(string) (*datastore.UploadToken, error) {
		return scopedUploadToken(), nil
	}
	claimUploadToken = func(token string, _ *time.Time) error {
		if h.claimHook != nil {
			h.claimHook()
		}
		h.mu.Lock()
		h.claimed = append(h.claimed, token)
		h.mu.Unlock()
		return nil
	}
	readBuildKitConfig = func(context.Context) (*agentstream.BuildKitConfig, error) {
		return &agentstream.BuildKitConfig{}, nil
	}
	pushArchive = func(context.Context, harborclient.Config, string, string, []byte) error {
		h.mu.Lock()
		h.pushed = true
		h.mu.Unlock()
		return nil
	}
	startCLIDeploymentAsync = func(_ []byte, _, _, _ string, _ []datastore.BuildArg, _ *agentstream.BuildKitConfig, end func()) {
		h.mu.Lock()
		h.cliStarted = true
		h.mu.Unlock()
		end()
	}

	return h
}

// upload drives one HandleCLIDeployUpload call synchronously and returns
// the recorder, bounding the request context (and so, transitively, the
// recheck's own 5s ceiling - context.WithTimeout always takes the earlier
// of the two deadlines) to timeout.
func (h *regressionHarness) upload(t *testing.T, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/cli-deploy/tok-fixture", bytes.NewReader(mustValidTarGz(t))).WithContext(ctx)
	w := httptest.NewRecorder()
	HandleCLIDeployUpload(w, req)
	return w
}

// uploadAsync runs upload in a goroutine, for a row that must act on the
// stream (deliver a reply, fail it) while the handler is still blocked
// waiting on the recheck.
func (h *regressionHarness) uploadAsync(t *testing.T, timeout time.Duration) <-chan *httptest.ResponseRecorder {
	t.Helper()
	resultCh := make(chan *httptest.ResponseRecorder, 1)
	go func() { resultCh <- h.upload(t, timeout) }()
	return resultCh
}

func (h *regressionHarness) assertNoEffects(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 0 {
		t.Errorf("claimed = %v, want no claim", h.claimed)
	}
	if h.pushed {
		t.Error("archive was pushed; want none")
	}
	if h.cliStarted {
		t.Error("build was started; want none")
	}
}

func (h *regressionHarness) assertClaimedButNoArchiveOrBuild(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim", h.claimed)
	}
	if h.pushed {
		t.Error("archive was pushed; want none once the Lifetime had ended")
	}
	if h.cliStarted {
		t.Error("build was started; want none once the Lifetime had ended")
	}
}

func (h *regressionHarness) assertClaimedPushedAndBuilt(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claimed) != 1 {
		t.Errorf("claimed = %v, want exactly one claim", h.claimed)
	}
	if !h.pushed {
		t.Error("expected the archive to be pushed")
	}
	if !h.cliStarted {
		t.Error("expected the CLI deployment build to start")
	}
}

func mustValidTarGz(t *testing.T) []byte {
	t.Helper()
	return validTarGz(t)
}
