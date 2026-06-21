package instructions

import (
	"net/http"
	"strings"
	"testing"
)

// TestWebFinalStatus pins the two regressions fixed in the follow flow:
//
//  1. The handler used to hardcode ResponseStatusCode "200 OK" regardless of
//     the real final HTTP status, masking 4xx/5xx auth and redirect failures as
//     success. webFinalStatus must return the response's actual Status line.
//  2. A nil response (no hop completed) must NOT panic; it returns a safe
//     synthetic status. This is the nil-guard counterpart to the http.NewRequest
//     error checks on the redirect/login paths (a malformed redirect/login URL
//     made http.NewRequest return nil, and feeding nil to client.Do panicked).
func TestWebFinalStatus(t *testing.T) {
	cases := []struct {
		name string
		resp *http.Response
		want string
	}{
		{
			name: "real 401 is reported, not masked as 200",
			resp: &http.Response{StatusCode: 401, Status: "401 Unauthorized"},
			want: "401 Unauthorized",
		},
		{
			name: "real 302 redirect status is reported",
			resp: &http.Response{StatusCode: 302, Status: "302 Found"},
			want: "302 Found",
		},
		{
			name: "real 200 is reported as-is",
			resp: &http.Response{StatusCode: 200, Status: "200 OK"},
			want: "200 OK",
		},
		{
			name: "nil response does not panic, returns synthetic unknown",
			resp: nil,
			want: "000 unknown",
		},
		{
			name: "empty Status falls back to code-derived status line",
			resp: &http.Response{StatusCode: 503},
			want: "503 Service Unavailable",
		},
		{
			name: "empty Status and zero code returns synthetic unknown",
			resp: &http.Response{},
			want: "000 unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := webFinalStatus(tc.resp)
			if got != tc.want {
				t.Fatalf("webFinalStatus(%+v) = %q, want %q", tc.resp, got, tc.want)
			}
			// Regression: the old code always returned "200 OK". A non-200
			// response must never be reported as 200.
			if tc.resp != nil && tc.resp.StatusCode != 0 && tc.resp.StatusCode != 200 {
				if strings.HasPrefix(got, "200") {
					t.Fatalf("webFinalStatus masked status %d as 200: %q", tc.resp.StatusCode, got)
				}
			}
		})
	}
}

// TestWebURLHostPath pins that the log helper strips everything a request URL
// can use to carry secrets: the query string (tokens), and any userinfo
// credentials. An unparseable URL must not be echoed back.
func TestWebURLHostPath(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "query string is stripped",
			url:  "https://example.com/oauth/callback?code=SECRET&state=xyz",
			want: "example.com/oauth/callback",
		},
		{
			name: "userinfo credentials are stripped",
			url:  "https://user:p@ss@example.com/path",
			want: "example.com/path",
		},
		{
			name: "bare host and path",
			url:  "https://api.internal.svc/v1/things",
			want: "api.internal.svc/v1/things",
		},
		{
			name: "control char makes it unparseable",
			url:  "https://example.com/\x7f?token=SECRET",
			want: "<unparseable>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := webURLHostPath(tc.url)
			if got != tc.want {
				t.Fatalf("webURLHostPath(%q) = %q, want %q", tc.url, got, tc.want)
			}
			// Belt-and-suspenders: a secret token must never appear in output.
			if strings.Contains(got, "SECRET") || strings.Contains(got, "p@ss") {
				t.Fatalf("webURLHostPath leaked a credential: %q", got)
			}
		})
	}
}

// TestWebRequestLogLine pins that the one-line request summary logs only the
// method, host+path, and sorted header KEYS, never header VALUES (e.g.
// Authorization bearer tokens) or the query string.
func TestWebRequestLogLine(t *testing.T) {
	line := webRequestLogLine("POST", "https://example.com/login?token=SECRET", map[string]string{
		"Authorization": "Bearer SUPERSECRETTOKEN",
		"Content-Type":  "application/json",
	})

	// Method, host+path, and header keys are present.
	for _, want := range []string{"POST", "example.com/login", "Authorization", "Content-Type"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q missing expected substring %q", line, want)
		}
	}
	// Header values and the query string must NOT be present.
	for _, leak := range []string{"SUPERSECRETTOKEN", "Bearer", "SECRET", "token="} {
		if strings.Contains(line, leak) {
			t.Errorf("log line %q leaked sensitive substring %q", line, leak)
		}
	}

	// Empty method defaults to GET.
	if got := webRequestLogLine("", "https://h/p", nil); !strings.HasPrefix(got, "GET ") {
		t.Errorf("empty method should default to GET, got %q", got)
	}
}
