package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	testPassword = "correct horse battery"
	testPhone    = "14155550001"
)

type oauthTest struct {
	t      *testing.T
	srv    *httptest.Server
	auth   *AuthServer
	client *http.Client
}

func newOAuthTest(t *testing.T) *oauthTest {
	t.Helper()
	s := server.NewMCPServer("test", "0")
	s.AddTool(mcp.NewTool("ping"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("pong"), nil
	})
	store, err := OpenAccountStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.AddAccount(context.Background(), testPhone, testPassword); err != nil {
		t.Fatal(err)
	}
	return newOAuthTestWith(t, store, s)
}

func newOAuthTestWith(t *testing.T, store *AccountStore, s *server.MCPServer) *oauthTest {
	t.Helper()
	ot := &oauthTest{t: t}
	var handler http.Handler
	ot.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(ot.srv.Close)
	var err error
	ot.auth, err = NewAuthServer(store, ot.srv.URL, defaultRedirects)
	if err != nil {
		t.Fatal(err)
	}
	handler = NewHTTPHandler(s, ot.auth, ot.srv.URL, nil)
	ot.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return ot
}

// signIn runs the whole OAuth flow for an account and returns its access token.
func (ot *oauthTest) signIn(phone, password string) string {
	ot.t.Helper()
	_, client := ot.register(claudeRedirect)
	clientID := client["client_id"].(string)
	verifier, challenge := pkce()
	resp := ot.authorizeAs(phone, clientID, claudeRedirect, challenge, password)
	if resp.StatusCode != http.StatusFound {
		ot.t.Fatalf("sign-in as %s failed: %d", phone, resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp, body := ot.postForm("/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {clientID}, "redirect_uri": {claudeRedirect}, "code_verifier": {verifier}})
	if resp.StatusCode != 200 {
		ot.t.Fatalf("token exchange failed: %s", body)
	}
	var tokens map[string]any
	_ = json.Unmarshal([]byte(body), &tokens)
	return tokens["access_token"].(string)
}

func (ot *oauthTest) do(method, path string, body io.Reader, contentType string, headers ...string) (*http.Response, string) {
	ot.t.Helper()
	req, _ := http.NewRequest(method, ot.srv.URL+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := ot.client.Do(req)
	if err != nil {
		ot.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func (ot *oauthTest) postForm(path string, form url.Values) (*http.Response, string) {
	return ot.do("POST", path, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
}

func (ot *oauthTest) register(redirect string) (*http.Response, map[string]any) {
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}, "client_name": "Claude"})
	resp, data := ot.do("POST", "/register", strings.NewReader(string(body)), "application/json")
	var out map[string]any
	_ = json.Unmarshal([]byte(data), &out)
	return resp, out
}

var requestIDPattern = regexp.MustCompile(`name="request" value="([^"]+)"`)

// authorize walks through the consent page and returns the redirect it ends with.
func (ot *oauthTest) authorize(clientID, redirect, challenge, password string) *http.Response {
	return ot.authorizeAs(testPhone, clientID, redirect, challenge, password)
}

func (ot *oauthTest) authorizeAs(phone, clientID, redirect, challenge, password string) *http.Response {
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"}}
	resp, page := ot.do("GET", "/authorize?"+q.Encode(), nil, "")
	if resp.StatusCode != http.StatusOK {
		ot.t.Fatalf("consent page: %d %s", resp.StatusCode, page)
	}
	m := requestIDPattern.FindStringSubmatch(page)
	if m == nil {
		ot.t.Fatalf("no request id in consent page:\n%s", page)
	}
	resp, _ = ot.postForm("/authorize", url.Values{"request": {m[1]}, "phone": {phone}, "password": {password}, "action": {"allow"}})
	return resp
}

func pkce() (verifier, challenge string) {
	verifier = randomToken(48)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

const claudeRedirect = "https://claude.ai/api/mcp/auth_callback"

func TestOAuthFlow(t *testing.T) {
	ot := newOAuthTest(t)
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	mcpPost := func(token string) (*http.Response, string) {
		return ot.do("POST", "/mcp", strings.NewReader(initBody), "application/json",
			"Accept", "application/json, text/event-stream", "Authorization", "Bearer "+token)
	}

	// Unauthenticated requests are challenged with a pointer to the metadata.
	resp, _ := mcpPost("")
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("expected 401 challenge, got %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	resp, body := ot.do("GET", "/.well-known/oauth-authorization-server", nil, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"registration_endpoint"`) || !strings.Contains(body, `"S256"`) {
		t.Fatalf("bad metadata: %d %s", resp.StatusCode, body)
	}

	// Redirects outside the allowlist can't register.
	if resp, _ := ot.register("https://evil.example/callback"); resp.StatusCode != 400 {
		t.Fatalf("evil redirect registered: %d", resp.StatusCode)
	}
	resp, client := ot.register(claudeRedirect)
	if resp.StatusCode != 201 {
		t.Fatalf("register failed: %d %v", resp.StatusCode, client)
	}
	clientID := client["client_id"].(string)

	verifier, challenge := pkce()
	if resp := ot.authorize(clientID, claudeRedirect, challenge, "wrong password!!"); resp.StatusCode != 401 {
		t.Fatalf("wrong password accepted: %d", resp.StatusCode)
	}
	resp = ot.authorize(clientID, claudeRedirect, challenge, testPassword)
	if resp.StatusCode != 302 {
		t.Fatalf("expected redirect after consent, got %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	if !strings.HasPrefix(loc.String(), claudeRedirect) || code == "" || loc.Query().Get("state") != "xyz" {
		t.Fatalf("bad redirect: %s", loc)
	}

	tokenForm := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {claudeRedirect}, "code_verifier": {verifier}}
	resp, body = ot.postForm("/token", tokenForm)
	if resp.StatusCode != 200 {
		t.Fatalf("token exchange failed: %d %s", resp.StatusCode, body)
	}
	var tokens map[string]any
	_ = json.Unmarshal([]byte(body), &tokens)
	access, refresh := tokens["access_token"].(string), tokens["refresh_token"].(string)

	// Codes are single use.
	if resp, _ := ot.postForm("/token", tokenForm); resp.StatusCode != 400 {
		t.Fatalf("code replay accepted: %d", resp.StatusCode)
	}

	resp, body = mcpPost(access)
	if resp.StatusCode != 200 || !strings.Contains(body, `"serverInfo"`) {
		t.Fatalf("MCP call with token failed: %d %s", resp.StatusCode, body)
	}
	if resp, _ := mcpPost(refresh); resp.StatusCode != 401 {
		t.Fatalf("refresh token accepted as access token: %d", resp.StatusCode)
	}

	// Refresh tokens rotate.
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}
	resp, body = ot.postForm("/token", refreshForm)
	if resp.StatusCode != 200 {
		t.Fatalf("refresh failed: %d %s", resp.StatusCode, body)
	}
	if resp, _ := ot.postForm("/token", refreshForm); resp.StatusCode != 400 {
		t.Fatalf("refresh token reuse accepted: %d", resp.StatusCode)
	}

	// Revoking everything signs the client out.
	if _, err := ot.auth.RevokeTokens(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if resp, _ := mcpPost(access); resp.StatusCode != 401 {
		t.Fatalf("revoked token still works: %d", resp.StatusCode)
	}
}

func TestOAuthRejectsBadRequests(t *testing.T) {
	ot := newOAuthTest(t)
	_, client := ot.register(claudeRedirect)
	clientID := client["client_id"].(string)

	// Wrong verifier fails PKCE.
	_, challenge := pkce()
	resp := ot.authorize(clientID, claudeRedirect, challenge, testPassword)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	wrongVerifier, _ := pkce()
	resp, _ = ot.postForm("/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {clientID}, "redirect_uri": {claudeRedirect}, "code_verifier": {wrongVerifier}})
	if resp.StatusCode != 400 {
		t.Fatalf("wrong PKCE verifier accepted: %d", resp.StatusCode)
	}

	// A redirect URI that wasn't registered gets an error page, never a redirect.
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://claude.ai/other"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	if resp, _ := ot.do("GET", "/authorize?"+q.Encode(), nil, ""); resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect not rejected: %d", resp.StatusCode)
	}

	// Plain PKCE isn't accepted.
	q.Set("redirect_uri", claudeRedirect)
	q.Set("code_challenge_method", "plain")
	resp, _ = ot.do("GET", "/authorize?"+q.Encode(), nil, "")
	if loc, _ := url.Parse(resp.Header.Get("Location")); loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("plain PKCE not rejected: %s", resp.Header.Get("Location"))
	}

	// Foreign Host headers are refused.
	req, _ := http.NewRequest("GET", ot.srv.URL+"/healthz", nil)
	req.Host = "evil.example"
	if r2, err := ot.client.Do(req); err != nil || r2.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host accepted")
	}

	// Five wrong passwords lock the consent form, even for the right password.
	_, challenge = pkce()
	for range 5 {
		ot.authorize(clientID, claudeRedirect, challenge, "wrong password!!")
	}
	if resp := ot.authorize(clientID, claudeRedirect, challenge, testPassword); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("lockout not enforced: %d", resp.StatusCode)
	}
}

func TestRedirectPolicy(t *testing.T) {
	p := defaultRedirects
	for uri, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback":       true,
		"https://claude.com/api/mcp/auth_callback":      true,
		"http://localhost:33418/callback":               true,
		"http://127.0.0.1:5000/cb":                      true,
		"https://claude.ai/api/mcp/auth_callback/extra": false,
		"https://claude.ai.evil.example/cb":             false,
		"https://localhost/cb":                          false,
		"http://localhost.evil.example/cb":              false,
		"https://claude.ai/api/mcp/auth_callback#frag":  false,
	} {
		if got := p.Allowed(uri); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", uri, got, want)
		}
	}
	p.Prefixes = []string{"https://my.app/oauth/"}
	if !p.Allowed("https://my.app/oauth/cb") {
		t.Error("configured prefix not allowed")
	}
}
