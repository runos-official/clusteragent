package webhook

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/datastore"
)

// jobAuthorityIDPattern requires a canonical lowercase UUID, matching
// agentstream.JobAuthority's own requirement (agentstream's
// authority.validate()). requireJobStagePermit checks this locally before
// ever calling the transport, so a scoped caller whose stored durable job
// id is missing or malformed (review finding 1397: this must never be
// confused with a legacy job that simply has none to check) gets a clean
// refusal instead of an opaque transport error - or, in a test with a
// transport fake that does not itself validate shape, a false permit.
var jobAuthorityIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// permitClock is the injected clock admitScopedUpload and its caller use to
// judge a permit's own expiry. A package var so tests can simulate time
// passing between the permit being granted and the claim boundary without
// a real sleep (plan: "Inject the clock and recheck client for
// deterministic tests").
var permitClock = time.Now

// requestStageRecheck is the injected fresh-decision client (story 338): one
// bounded, correlated request per call over the authenticated control
// stream. A package var so tests can supply a fake transport without a live
// stream. Production wiring is agentstream.RequestStageRecheck.
var requestStageRecheck = agentstream.RequestStageRecheck

// uploadRefusal is a refusal to admit an upload past the checks that run
// before the atomic token claim. status and message are what the caller
// sees; message is deliberately generic (plan "Refusal behavior": never
// reveal whether another account, cluster, app, binding, or credential
// exists). reason is the detailed cause, for logs only. terminal says
// whether retrying with the SAME token could plausibly help (review
// finding 1133): a denial or a step-up condition will not change on retry,
// but an outage, a transport error, or a timeout might.
type uploadRefusal struct {
	status   int
	message  string
	reason   string
	terminal bool
}

// forbiddenUpload builds a terminal (403) refusal: an authoritative denial
// or step-up condition. Retrying the same request will not change this
// outcome, so the caller's uploadID gets a recorded, pollable reason
// (review finding 1133).
func forbiddenUpload(reason string) *uploadRefusal {
	return &uploadRefusal{
		status:   http.StatusForbidden,
		message:  "Upload refused",
		reason:   reason,
		terminal: true,
	}
}

// unavailableUpload builds a retryable (503) refusal: an outage, a
// transport error, or a timeout. The token stays unclaimed and no build
// row is recorded, so a legitimate retry with the same token is never
// blocked by this attempt (review findings 1075/1086).
func unavailableUpload(reason string) *uploadRefusal {
	return &uploadRefusal{
		status:   http.StatusServiceUnavailable,
		message:  "Upload temporarily unavailable, retry shortly",
		reason:   reason,
		terminal: false,
	}
}

// admitScopedUpload performs the scoped admission sequence's authoritative
// step: ask Conductor/Roid for a current, correlated archive-persist
// decision bound to the stored binding, and accept only an exact current
// permit. It never claims the token and never persists anything; the
// returned response's ExpiresAt is the permit's own deadline, which the
// caller must still hold at the claim boundary (review finding 1076: a
// permit's five-second deadline is real and must be enforced again right
// before the first mutation, not discarded once admission succeeds).
// Every other outcome - deny, needs, unavailable, or a transport/validation
// error (malformed reply, wrong binding, stale or replayed proof,
// disconnect, timeout) - refuses, fails closed, and carries no detail the
// caller could use to enumerate another account, cluster, app, binding, or
// credential.
func admitScopedUpload(ctx context.Context, binding *datastore.UploadAuthorityBinding) (*agentstream.StageResponse, *uploadRefusal) {
	if binding == nil {
		// A scoped classification with no stored binding is a storage
		// invariant violation, not a legacy token (plan "Legacy behavior":
		// "A missing binding on a scoped classification is invalid, not
		// legacy"). Fail closed rather than falling back to legacy
		// authorization.
		return nil, unavailableUpload("scoped token has no stored authorization binding")
	}

	retryID := uuid.New().String()
	authority := agentstream.UploadAuthority(binding.BindingID, binding.UploadID)
	resp, err := requestStageRecheck(ctx, authority, agentstream.StageArchivePersist, retryID)
	if err != nil {
		return nil, unavailableUpload(fmt.Sprintf("deployment stage recheck unavailable: %v", err))
	}

	switch resp.Outcome {
	case agentstream.OutcomePermit:
		return resp, nil
	case agentstream.OutcomeDeny, agentstream.OutcomeNeeds:
		return nil, forbiddenUpload(fmt.Sprintf("deployment stage recheck outcome %q (code %q)", resp.Outcome, resp.Code))
	default:
		// agentstream already rejects any outcome value it doesn't know, so
		// the only value left here is OutcomeUnavailable.
		return nil, unavailableUpload(fmt.Sprintf("deployment stage recheck outcome %q (code %q)", resp.Outcome, resp.Code))
	}
}

// permitStillValid re-checks a granted permit's own deadline and the
// Lifetime of the control stream it arrived on (review findings
// 1076/1128, and 1313 repeat of 1270/1240/1169). The handler calls this
// twice: once before the atomic claim (time can pass between
// admitScopedUpload returning and the claim), and again immediately
// before the archive push (L6) - after the claim itself, and after every
// blocking step since, including the log write that used to sit between
// that second check and the push with no check of its own in between.
// Two calls close both gaps without a second network round trip.
// Equality counts as expired (finding 1128: "Treat equality with
// expiresAt as expired") - a permit is valid only strictly before its own
// deadline.
//
// L4: the response payload carries no connection identity of its own, so a
// disconnect and reconnect between receiving the permit and acting on it
// would otherwise be invisible - a replacement connection can be live by
// then, and the original permit would keep authorizing persistence until
// its timestamp expires. resp.Lifetime is the Lifetime that delivered this
// permit (sendAndWait validated it against this exact value, L3); once a
// Lifetime goes live-to-ended it never comes back, so Live() here catches
// any disconnect or reconnect since, even if a new connection is active
// again by the time this runs.
func permitStillValid(resp *agentstream.StageResponse) *uploadRefusal {
	if !permitClock().Before(resp.ExpiresAt) {
		return unavailableUpload("permit expired at the claim or persistence boundary")
	}
	if !resp.Lifetime.Live() {
		return unavailableUpload("control stream disconnected or reconnected since the permit was received")
	}
	return nil
}

// requireJobStagePermit asks for one fresh, correlated job-authority permit
// (story 339) for authorityJobID at stage, over the SAME injected transport
// admitScopedUpload uses. It returns nil only for an exact current permit;
// every other outcome, or a transport/validation failure, returns a
// non-nil, generic refusal that reveals nothing about the account, cluster,
// app, or job involved. Callers invoke this ONLY for a scoped job (a
// non-empty authorityJobID); a legacy job skips it entirely, matching the
// upload path's own no-fallback discipline.
//
// Unlike admitScopedUpload's archive_persist check, no second
// permitStillValid-style recheck follows this one: every caller performs
// the stage's mutation immediately after this call returns, with no
// interposed round trip (no atomic claim) that could let the permit's own
// short deadline or a disconnect pass unnoticed in between.
func requireJobStagePermit(ctx context.Context, authorityJobID string, stage agentstream.RecheckStage) *uploadRefusal {
	if !jobAuthorityIDPattern.MatchString(authorityJobID) {
		return unavailableUpload("missing or invalid durable job authority")
	}
	retryID := uuid.New().String()
	resp, err := requestStageRecheck(ctx, agentstream.JobAuthority(authorityJobID), stage, retryID)
	if err != nil {
		return unavailableUpload(fmt.Sprintf("deployment stage recheck unavailable: %v", err))
	}
	switch resp.Outcome {
	case agentstream.OutcomePermit:
		return nil
	case agentstream.OutcomeDeny, agentstream.OutcomeNeeds:
		return forbiddenUpload(fmt.Sprintf("deployment stage recheck outcome %q (code %q)", resp.Outcome, resp.Code))
	default:
		return unavailableUpload(fmt.Sprintf("deployment stage recheck outcome %q (code %q)", resp.Outcome, resp.Code))
	}
}

// verifyBuildImageHarborIdentity implements story 338 criteria 6(b) and
// 6(b2): before any build or push, compare the Harbor identity Conductor
// verified and locked before minting the token (persisted on the token row)
// with the Harbor the shared BuildKit configuration names right now, from
// the SAME read the caller will push to - never a second read. A token
// minted before this field existed (tokenHarborOsid empty) keeps today's
// behavior unchanged and skips the comparison (criterion 6(d)'s explicit
// legacy branch).
//
// Review finding 1268: a matching, non-empty identity alone is not enough.
// The ConfigMap and Secret are independent objects, and a configuration can
// name a Harbor identity (criterion 6(b)'s comparison) while its endpoint
// (harborURL) is absent or empty - predating full provisioning, or torn by
// an in-flight repair. Criterion 6(b2) requires a named refusal for that
// case too, before claim and before BuildKit starts a pod against an
// unusable image reference.
func verifyBuildImageHarborIdentity(tokenHarborOsid, configHarborOsid, configHarborURL string) *uploadRefusal {
	if tokenHarborOsid == "" {
		return nil
	}
	if configHarborOsid == "" {
		return forbiddenUpload("buildkit configuration names no Harbor; cannot verify the identity locked at mint time")
	}
	if tokenHarborOsid != configHarborOsid {
		return forbiddenUpload("buildkit configuration names a different Harbor than the one locked at mint time")
	}
	if strings.TrimSpace(configHarborURL) == "" {
		return forbiddenUpload("buildkit configuration names the locked Harbor identity but no registry endpoint")
	}
	return nil
}
