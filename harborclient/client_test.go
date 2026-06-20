package harborclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHost pins the registry-host derivation from a configured Harbor URL:
// strip a leading http(s):// scheme and a trailing slash so the result is a
// bare `host[:port]` usable both as an ORAS repository prefix and an auth
// credential key.
func TestHost(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"https scheme stripped", "https://harbor.example.com", "harbor.example.com"},
		{"http scheme stripped", "http://harbor.example.com", "harbor.example.com"},
		{"no scheme passthrough", "harbor.example.com", "harbor.example.com"},
		{"trailing slash trimmed", "https://harbor.example.com/", "harbor.example.com"},
		{"scheme and trailing slash", "http://harbor.example.com:8890/", "harbor.example.com:8890"},
		{"host:port preserved", "https://harbor.example.com:443", "harbor.example.com:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := host(Config{URL: tc.url}); got != tc.want {
				t.Errorf("host(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestListArchives_Flattening pins that a Harbor artifacts response is
// flattened to one Archive per (artifact, tag) pair, carrying the artifact's
// digest/size/push_time with the tag name as the CLI upload id, in order.
func TestListArchives_Flattening(t *testing.T) {
	pushA := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	pushB := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)

	var gotPath string
	var gotAuthUser, gotAuthPass string
	srv := newHarborServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuthUser, gotAuthPass, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"digest":    "sha256:aaa",
				"size":      111,
				"push_time": pushA,
				"tags":      []map[string]string{{"name": "upload-1"}, {"name": "upload-2"}},
			},
			{
				"digest":    "sha256:bbb",
				"size":      222,
				"push_time": pushB,
				"tags":      []map[string]string{{"name": "upload-3"}},
			},
		})
	})

	cfg := Config{URL: srv.URL, Username: "robot$archives", Password: "s3cret"}
	got, err := ListArchives(context.Background(), cfg, "app-mn4pq")
	if err != nil {
		t.Fatalf("ListArchives: %v", err)
	}

	// 2 tags on the first artifact + 1 on the second = 3 flattened entries.
	want := []Archive{
		{CliUploadID: "upload-1", Digest: "sha256:aaa", Size: 111, PushTime: pushA},
		{CliUploadID: "upload-2", Digest: "sha256:aaa", Size: 111, PushTime: pushA},
		{CliUploadID: "upload-3", Digest: "sha256:bbb", Size: 222, PushTime: pushB},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d archives, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].CliUploadID != want[i].CliUploadID || got[i].Digest != want[i].Digest ||
			got[i].Size != want[i].Size || !got[i].PushTime.Equal(want[i].PushTime) {
			t.Errorf("archive[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The request must hit the v2 artifacts endpoint for the OSID repo under
	// the archives project, with basic auth.
	wantPath := fmt.Sprintf("/api/v2.0/projects/%s/repositories/app-mn4pq/artifacts?with_tag=true&page_size=100", ArchivesProject)
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if gotAuthUser != "robot$archives" || gotAuthPass != "s3cret" {
		t.Errorf("basic auth = %q:%q, want robot$archives:s3cret", gotAuthUser, gotAuthPass)
	}
}

// TestListArchives_NotFoundIsEmpty pins that a 404 (no such repository yet)
// returns an empty, non-nil slice and no error: an app with no uploads is a
// normal state, not a failure.
func TestListArchives_NotFoundIsEmpty(t *testing.T) {
	srv := newHarborServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	got, err := ListArchives(context.Background(), Config{URL: srv.URL}, "app-none")
	if err != nil {
		t.Fatalf("ListArchives on 404: %v", err)
	}
	if got == nil {
		t.Error("ListArchives on 404 returned nil slice, want non-nil empty")
	}
	if len(got) != 0 {
		t.Errorf("ListArchives on 404 = %+v, want empty", got)
	}
}

// TestListArchives_ErrorStatus pins that a non-200/404 status is surfaced as
// an error that carries the status code and the response body.
func TestListArchives_ErrorStatus(t *testing.T) {
	srv := newHarborServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	_, err := ListArchives(context.Background(), Config{URL: srv.URL}, "app-x")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q should carry the status code and body", err.Error())
	}
}

// TestListArchives_EmptyResponse pins that a 200 with an empty artifact list
// yields an empty (non-nil) result.
func TestListArchives_EmptyResponse(t *testing.T) {
	srv := newHarborServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	got, err := ListArchives(context.Background(), Config{URL: srv.URL}, "app-empty")
	if err != nil {
		t.Fatalf("ListArchives: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want empty", got)
	}
}

// newHarborServer starts an httptest server running handler and registers its
// shutdown with t.Cleanup.
func newHarborServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}
