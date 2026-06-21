package instructions

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/runos-official/clusteragent/commons"
)

const (
	// webRequestTimeout bounds the total time of a single outbound HTTP
	// request (connect + read body). AllowInsecure stays supported (legit for
	// self-signed in-cluster endpoints); we only bound time and size so a slow
	// or hostile endpoint can't hang or OOM the agent.
	webRequestTimeout = 30 * time.Second
	// webRequestMaxBodyBytes caps how much of a response body we will buffer.
	webRequestMaxBodyBytes = 32 << 20 // 32 MiB
)

// webRequestLogLine renders a non-sensitive one-line summary of an outbound web
// request for logging. It deliberately omits anything that can carry secrets:
// the raw request payload, the URL query string (may carry tokens), header
// VALUES (e.g. Authorization bearer tokens), and the request body. It logs only
// the method, the URL host + path, and the sorted header KEYS. rawURL that
// fails to parse is reported as "<unparseable>" rather than echoed back, so a
// crafted URL can't smuggle credentials into the log via the error path.
func webRequestLogLine(method, rawURL string, headers map[string]string) string {
	if method == "" {
		method = "GET"
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("%s %s headerKeys=%v", method, webURLHostPath(rawURL), keys)
}

// webURLHostPath returns the host + path of rawURL with the query string and
// any userinfo/credentials stripped, safe to log. A URL that fails to parse is
// reported as "<unparseable>" rather than echoed back, so a crafted URL can't
// smuggle credentials into the log via the error path.
func webURLHostPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable>"
	}
	return u.Host + u.Path
}

func WebRequestFromServer(jsonB64 string) (string, string, error) {
	type requestType struct {
		Url           string            `json:"url"`
		Method        string            `json:"method"`
		PostData      string            `json:"postData"`
		AllowInsecure bool              `json:"allowInsecure"`
		Headers       map[string]string `json:"headers"`
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
	// bearer tokens) and a query string that may contain credentials. Never log
	// the raw payload, header values, or the query string.
	log.Printf("WebRequestFromServer called - %s", webRequestLogLine(request.Method, request.Url, request.Headers))

	// SSRF guard: reject a non-http(s) scheme up front; the transport's dialer
	// additionally refuses loopback/link-local/cloud-metadata IPs on every hop
	// (including redirects) and pins the dial to defeat DNS rebinding.
	if err := validateOutboundScheme(request.Url); err != nil {
		log.Printf("WebRequestFromServer refused: %v", err)
		return "", "", err
	}

	client := &http.Client{
		Timeout:   webRequestTimeout,
		Transport: newGuardedTransport(request.AllowInsecure),
	}

	// Create HTTP request
	var reqBody io.Reader
	if request.PostData != "" {
		reqBody = bytes.NewBufferString(request.PostData)
	}

	method := "GET"
	if request.Method != "" {
		method = request.Method
	}

	req, err := http.NewRequest(method, request.Url, reqBody)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		return "", "", err
	}

	// Add headers
	if request.Headers != nil {
		for key, value := range request.Headers {
			req.Header.Add(key, value)
		}
	}

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error executing request: %v", err)
		return "", "", err
	}
	defer resp.Body.Close()

	// Read response body, bounded to avoid OOM on a hostile/huge response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, webRequestMaxBodyBytes))
	if err != nil {
		log.Printf("Error reading response body: %v", err)
		return "", "", err
	}

	responseJsonB64, err := commons.JsonB64Encode(responseType{
		ResponseBody:       string(body),
		ResponseStatusCode: resp.Status,
	})
	if err != nil {
		log.Printf("Error encoding response: %s", err)
		return "", "", err
	}

	return "WEB_REQUEST_RESPONSE", responseJsonB64, nil
}
