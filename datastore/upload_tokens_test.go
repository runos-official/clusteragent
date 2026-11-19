package datastore

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// TestUploadToken_RawTokenNeverStored proves the security invariant: the raw
// token is never written to the database. After minting a token we scan every
// upload_tokens row and assert no stored value equals the raw token, and that
// token_hash equals sha256(raw) hex. It also confirms lookup-by-raw-token works
// via hashing.
func TestUploadToken_RawTokenNeverStored(t *testing.T) {
	setupTestDB(t)

	const raw = "deadbeefcafef00d0123456789abcdef0123456789abcdef0123456789abcdef"
	expires := time.Now().Add(5 * time.Minute)
	if err := CreateUploadToken(raw, "osid-secure", "upload-secure", "Dockerfile", nil, expires); err != nil {
		t.Fatalf("CreateUploadToken: %v", err)
	}

	sum := sha256.Sum256([]byte(raw))
	wantHash := hex.EncodeToString(sum[:])

	// Scan the raw rows of the model: assert no column carries the plaintext
	// token and the hash matches.
	var rows []UploadTokenModel
	if err := testDB(t).Find(&rows).Error; err != nil {
		t.Fatalf("scan rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.TokenHash != wantHash {
		t.Fatalf("token_hash = %q, want sha256(raw) = %q", r.TokenHash, wantHash)
	}
	// No stored string field may equal the raw token.
	for name, val := range map[string]string{
		"token_hash":                   r.TokenHash,
		"deploy_config":                r.DeployConfig,
		"dockerfile":                   r.Dockerfile,
		"build_args":                   r.BuildArgs,
		"build_target":                 r.BuildTarget,
		"purpose":                      r.Purpose,
		"authorization_classification": r.AuthorizationClassification,
		"authorization_version":        r.AuthorizationBindingVersion,
		"authorization_binding":        r.AuthorizationBinding,
	} {
		if val == raw {
			t.Fatalf("raw token leaked into column %q", name)
		}
	}

	// Get-by-raw-token works (it hashes internally) and never exposes a stored
	// plaintext token (Token is echoed from the caller's input).
	tok, err := GetUploadToken(raw)
	if err != nil {
		t.Fatalf("GetUploadToken(raw): %v", err)
	}
	if tok.DeployConfig != "osid-secure:upload-secure" {
		t.Fatalf("deploy_config = %q, want osid-secure:upload-secure", tok.DeployConfig)
	}
	if tok.Token != raw {
		t.Fatalf("returned Token = %q, want the raw token echoed back", tok.Token)
	}

	// A wrong token must not match.
	if _, err := GetUploadToken("not-the-token"); err == nil {
		t.Fatalf("GetUploadToken(wrong) returned a match, want not-found")
	}
}

// TestUploadToken_MarkUsedByRawToken pins that single-use consumption works by
// hashing the presented raw token: the first mark succeeds, the second fails.
func TestUploadToken_MarkUsedByRawToken(t *testing.T) {
	setupTestDB(t)

	const raw = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	expires := time.Now().Add(5 * time.Minute)
	if err := CreatePullToken(raw, "osid-1", "upload-1", expires); err != nil {
		t.Fatalf("CreatePullToken: %v", err)
	}

	if err := MarkUploadTokenUsed(raw, nil); err != nil {
		t.Fatalf("first MarkUploadTokenUsed: %v", err)
	}
	if err := MarkUploadTokenUsed(raw, nil); err == nil {
		t.Fatalf("second MarkUploadTokenUsed should fail (already used)")
	}

	tok, err := GetPullToken(raw)
	if err != nil {
		t.Fatalf("GetPullToken: %v", err)
	}
	if !tok.Used {
		t.Fatalf("token should be marked used")
	}
}

// TestMarkUploadTokenUsed_RefusesExpiredTokenAtomically pins review finding
// 1076: the atomic claim itself must enforce expiry, not just an earlier,
// separate check. A token already past its expires_at must refuse the
// claim even though it is otherwise unused - closing the window where body
// reads, tarball validation, a config read, or a scoped recheck round trip
// consume enough time for the token to expire between an earlier check and
// the actual claim. A token that is NOT expired must still claim normally
// (a legitimate request must still pass).
func TestMarkUploadTokenUsed_RefusesExpiredTokenAtomically(t *testing.T) {
	setupTestDB(t)

	const expiredRaw = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	past := time.Now().Add(-1 * time.Minute)
	if err := CreatePullToken(expiredRaw, "osid-1", "upload-exp", past); err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	if err := MarkUploadTokenUsed(expiredRaw, nil); err == nil {
		t.Fatal("expected MarkUploadTokenUsed to refuse an already-expired token")
	}
	tok, err := GetPullToken(expiredRaw)
	if err != nil {
		t.Fatalf("GetPullToken: %v", err)
	}
	if tok.Used {
		t.Fatal("an expired token must not be left marked used by the refused claim")
	}

	const liveRaw = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	future := time.Now().Add(5 * time.Minute)
	if err := CreatePullToken(liveRaw, "osid-1", "upload-live", future); err != nil {
		t.Fatalf("create live token: %v", err)
	}
	if err := MarkUploadTokenUsed(liveRaw, nil); err != nil {
		t.Fatalf("a non-expired token must still claim: %v", err)
	}
}

// TestMarkUploadTokenUsed_RefusesExpiredTokenAtomically_AcrossTimezones pins
// review finding 1203: the fix for finding 1128 must give the correct
// answer regardless of the process's local timezone, not only on a UTC
// host. Measured in this repository: the prior fix (a bare
// "expires_at > CURRENT_TIMESTAMP" comparison on SQLite) refused a LIVE
// token under America/New_York and admitted an EXPIRED token under
// Europe/Berlin, because GORM stores a local-offset time.Time as text
// while SQLite's CURRENT_TIMESTAMP is bare UTC text. This test sets
// time.Local directly (not the TZ environment variable), so it proves the
// property regardless of the environment it runs in, in both directions
// (UTC+1 and UTC-5, plus the widest real offsets, UTC+14 and UTC-11).
func TestMarkUploadTokenUsed_RefusesExpiredTokenAtomically_AcrossTimezones(t *testing.T) {
	zones := []string{"Europe/Berlin", "America/New_York", "Pacific/Kiritimati", "Pacific/Niue"}
	for _, zoneName := range zones {
		t.Run(zoneName, func(t *testing.T) {
			loc, err := time.LoadLocation(zoneName)
			if err != nil {
				t.Fatalf("time.LoadLocation(%q): %v", zoneName, err)
			}
			prevLocal := time.Local
			time.Local = loc
			t.Cleanup(func() { time.Local = prevLocal })

			setupTestDB(t)

			expiredRaw := "expired-token-" + zoneName
			past := time.Now().Add(-1 * time.Minute)
			if err := CreatePullToken(expiredRaw, "osid-1", "upload-exp", past); err != nil {
				t.Fatalf("create expired token: %v", err)
			}
			if err := MarkUploadTokenUsed(expiredRaw, nil); err == nil {
				t.Fatalf("[%s] expected MarkUploadTokenUsed to refuse an already-expired token", zoneName)
			}

			liveRaw := "live-token-" + zoneName
			future := time.Now().Add(5 * time.Minute)
			if err := CreatePullToken(liveRaw, "osid-1", "upload-live", future); err != nil {
				t.Fatalf("create live token: %v", err)
			}
			if err := MarkUploadTokenUsed(liveRaw, nil); err != nil {
				t.Fatalf("[%s] a non-expired token must still claim: %v", zoneName, err)
			}
		})
	}
}

// TestUploadToken_DeleteExpired pins that expired tokens are swept and live ones
// are retained.
func TestUploadToken_DeleteExpired(t *testing.T) {
	setupTestDB(t)

	past := time.Now().Add(-1 * time.Minute)
	future := time.Now().Add(5 * time.Minute)
	if err := CreatePullToken("expired-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "osid-1", "u1", past); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if err := CreatePullToken("live-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "osid-1", "u2", future); err != nil {
		t.Fatalf("create live: %v", err)
	}

	if err := DeleteExpiredUploadTokens(); err != nil {
		t.Fatalf("DeleteExpiredUploadTokens: %v", err)
	}

	if _, err := GetPullToken("expired-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatalf("expired token should be gone")
	}
	if _, err := GetPullToken("live-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatalf("live token should remain: %v", err)
	}
}

// TestHashToken_IsSHA256Hex pins the hashing function shape so a future change
// to HMAC/pepper is a conscious decision.
func TestHashToken_IsSHA256Hex(t *testing.T) {
	const raw = "some-raw-token"
	sum := sha256.Sum256([]byte(raw))
	if got, want := hashToken(raw), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hashToken = %q, want %q", got, want)
	}
}
