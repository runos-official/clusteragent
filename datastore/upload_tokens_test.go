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
		"token_hash":    r.TokenHash,
		"deploy_config": r.DeployConfig,
		"dockerfile":    r.Dockerfile,
		"build_args":    r.BuildArgs,
		"build_target":  r.BuildTarget,
		"purpose":       r.Purpose,
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

	if err := MarkUploadTokenUsed(raw); err != nil {
		t.Fatalf("first MarkUploadTokenUsed: %v", err)
	}
	if err := MarkUploadTokenUsed(raw); err == nil {
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
