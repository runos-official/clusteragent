package instructions

import (
	"strings"
	"testing"
)

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
