package webhook

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/datastore"
)

// requestStageRecheck is the injected fresh-decision client (story 338): one
// bounded, correlated request per call over the authenticated control
// stream. A package var so tests can supply a fake transport without a live
// stream. Production wiring is agentstream.RequestStageRecheck.
var requestStageRecheck = agentstream.RequestStageRecheck

// uploadRefusal is a refusal to admit an upload past the checks that run
// before the atomic token claim. status and message are what the caller
// sees; message is deliberately generic (plan "Refusal behavior": never
// reveal whether another account, cluster, app, binding, or credential
// exists). reason is the detailed cause, for logs only.
type uploadRefusal struct {
	status  int
	message string
	reason  string
}

func forbiddenUpload(reason string) *uploadRefusal {
	return &uploadRefusal{
		status:  http.StatusForbidden,
		message: "Upload refused",
		reason:  reason,
	}
}

func unavailableUpload(reason string) *uploadRefusal {
	return &uploadRefusal{
		status:  http.StatusServiceUnavailable,
		message: "Upload temporarily unavailable, retry shortly",
		reason:  reason,
	}
}

// admitScopedUpload performs the scoped admission sequence's authoritative
// step: ask Conductor/Roid for a current, correlated archive-persist
// decision bound to the stored binding, and accept only an exact current
// permit. It never claims the token and never persists anything; the caller
// does that only when this returns nil. Every other outcome - deny, needs,
// unavailable, or a transport/validation error (malformed reply, wrong
// binding, stale or replayed proof, disconnect, timeout) - refuses, fails
// closed, and carries no detail the caller could use to enumerate another
// account, cluster, app, binding, or credential.
func admitScopedUpload(ctx context.Context, binding *datastore.UploadAuthorityBinding) *uploadRefusal {
	if binding == nil {
		// A scoped classification with no stored binding is a storage
		// invariant violation, not a legacy token (plan "Legacy behavior":
		// "A missing binding on a scoped classification is invalid, not
		// legacy"). Fail closed rather than falling back to legacy
		// authorization.
		return unavailableUpload("scoped token has no stored authorization binding")
	}

	retryID := uuid.New().String()
	authority := agentstream.UploadAuthority(binding.BindingID, binding.UploadID)
	resp, err := requestStageRecheck(ctx, authority, agentstream.StageArchivePersist, retryID)
	if err != nil {
		return unavailableUpload(fmt.Sprintf("deployment stage recheck unavailable: %v", err))
	}

	switch resp.Outcome {
	case agentstream.OutcomePermit:
		return nil
	case agentstream.OutcomeDeny, agentstream.OutcomeNeeds:
		return forbiddenUpload(fmt.Sprintf("deployment stage recheck outcome %q (code %q)", resp.Outcome, resp.Code))
	default:
		// agentstream already rejects any outcome value it doesn't know, so
		// the only value left here is OutcomeUnavailable.
		return unavailableUpload(fmt.Sprintf("deployment stage recheck outcome %q (code %q)", resp.Outcome, resp.Code))
	}
}

// verifyBuildImageHarborIdentity implements story 338 criterion 6(b): before
// any build or push, compare the Harbor identity Conductor verified and
// locked before minting the token (persisted on the token row) with the
// Harbor the shared BuildKit configuration names right now, from the SAME
// read the caller will push to - never a second read. A token minted before
// this field existed (tokenHarborOsid empty) keeps today's behavior
// unchanged and skips the comparison (criterion 6(d)'s explicit legacy
// branch).
func verifyBuildImageHarborIdentity(tokenHarborOsid, configHarborOsid string) *uploadRefusal {
	if tokenHarborOsid == "" {
		return nil
	}
	if configHarborOsid == "" {
		return forbiddenUpload("buildkit configuration names no Harbor; cannot verify the identity locked at mint time")
	}
	if tokenHarborOsid != configHarborOsid {
		return forbiddenUpload("buildkit configuration names a different Harbor than the one locked at mint time")
	}
	return nil
}
