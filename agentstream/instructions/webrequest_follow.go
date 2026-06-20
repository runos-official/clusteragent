package instructions

import (
	"crypto/tls"
	"fmt"
	"github.com/runos-official/clusteragent/commons"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// WebRequestFollowFromServer performs a multi-step HTTP flow that follows redirects,
// maintains cookies, and can auto-submit login forms.
//
// Used for OAuth2 device code flows where the client must:
// 1. Submit a user code verification form
// 2. Follow redirects to a login page
// 3. Submit credentials to the login form
// 4. Follow redirects to the callback
//
// When loginCredentials is provided, the handler will detect HTML login forms
// (by looking for a form with action containing "login") and automatically submit
// the credentials to the form action URL.
func WebRequestFollowFromServer(jsonB64 string) (string, string, error) {
	type requestType struct {
		URL              string            `json:"url"`
		Method           string            `json:"method"`
		FormData         map[string]string `json:"formData"`
		Headers          map[string]string `json:"headers"`
		LoginCredentials *struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"loginCredentials"`
		AllowInsecure bool `json:"allowInsecure"`
	}

	type responseType struct {
		ResponseBody       string `json:"responseBody"`
		ResponseStatusCode string `json:"responseStatusCode"`
	}

	var request requestType
	if err := commons.JsonB64Decode(jsonB64, &request); err != nil {
		log.Printf("Error decoding message: %s", err)
		return "", "", err
	}

	// Metadata only: the request carries arbitrary Headers (e.g. Authorization
	// bearer tokens), form login credentials, and a query string that may
	// contain secrets. Never log the raw payload, header values, the query
	// string, or credentials.
	log.Printf("WebRequestFollowFromServer called - %s", webRequestLogLine(request.Method, request.URL, request.Headers))

	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", "", fmt.Errorf("error creating cookie jar: %w", err)
	}

	client := &http.Client{
		Jar:     jar,
		Timeout: webRequestTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: request.AllowInsecure, //nolint:gosec // AllowInsecure is an explicit opt-in for self-signed in-cluster endpoints
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Helper to execute a request and follow redirects (up to 15 hops)
	doAndFollow := func(req *http.Request) (string, *http.Response, error) {
		var body string
		var resp *http.Response
		current := req

		for hops := 0; hops < 15; hops++ {
			var execErr error
			resp, execErr = client.Do(current)
			if execErr != nil {
				return "", nil, fmt.Errorf("hop %d: %w", hops, execErr)
			}

			bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, webRequestMaxBodyBytes))
			resp.Body.Close()
			if readErr != nil {
				return "", nil, fmt.Errorf("hop %d read: %w", hops, readErr)
			}
			body = string(bodyBytes)

			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				location := resp.Header.Get("Location")
				if location == "" {
					break
				}
				redirectURL, parseErr := current.URL.Parse(location)
				if parseErr != nil {
					break
				}
				// Log host+path only: a redirect Location can carry credentials
				// or tokens in its query string (common in OAuth callbacks).
				log.Printf("  Redirect -> %s", webURLHostPath(redirectURL.String()))
				current, _ = http.NewRequest("GET", redirectURL.String(), nil)
				continue
			}
			break
		}
		return body, resp, nil
	}

	// Step 1: Execute the initial request (e.g., POST verify_code) and follow redirects
	method := request.Method
	if method == "" {
		method = "GET"
	}

	var reqBody io.Reader
	if len(request.FormData) > 0 {
		form := url.Values{}
		for k, v := range request.FormData {
			form.Set(k, v)
		}
		reqBody = strings.NewReader(form.Encode())
	}

	initialReq, err := http.NewRequest(method, request.URL, reqBody)
	if err != nil {
		return "", "", fmt.Errorf("creating initial request: %w", err)
	}
	if len(request.FormData) > 0 {
		initialReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range request.Headers {
		initialReq.Header.Set(k, v)
	}

	log.Printf("Step 1: %s %s", method, webURLHostPath(request.URL))
	body, _, err := doAndFollow(initialReq)
	if err != nil {
		return "", "", fmt.Errorf("step 1: %w", err)
	}

	// Step 2: If loginCredentials provided, detect and submit the login form
	if request.LoginCredentials != nil {
		// Extract form action URL containing "login"
		re := regexp.MustCompile(`action="([^"]*login[^"]*)"`)
		match := re.FindStringSubmatch(body)
		if match == nil {
			return "", "", fmt.Errorf("login form not found in response HTML")
		}

		formAction := strings.ReplaceAll(match[1], "&amp;", "&")
		// Log host+path only: the form action URL can carry tokens in its query.
		log.Printf("Step 2: Found login form action: %s", webURLHostPath(formAction))

		// Resolve relative URL against the initial request URL
		baseURL, _ := url.Parse(request.URL)
		loginURL, err := baseURL.Parse(formAction)
		if err != nil {
			return "", "", fmt.Errorf("parsing login URL: %w", err)
		}

		// Submit login credentials
		loginForm := url.Values{}
		loginForm.Set("login", request.LoginCredentials.Username)
		loginForm.Set("password", request.LoginCredentials.Password)

		loginReq, _ := http.NewRequest("POST", loginURL.String(), strings.NewReader(loginForm.Encode()))
		loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		log.Printf("Step 2: POST %s", webURLHostPath(loginURL.String()))
		body, _, err = doAndFollow(loginReq)
		if err != nil {
			return "", "", fmt.Errorf("step 2 login: %w", err)
		}

		log.Printf("Step 2: Login flow completed")
	}

	responseJsonB64, err := commons.JsonB64Encode(responseType{
		ResponseBody:       body,
		ResponseStatusCode: "200 OK",
	})
	if err != nil {
		return "", "", fmt.Errorf("encoding response: %w", err)
	}

	return "WEB_REQUEST_RESPONSE", responseJsonB64, nil
}
