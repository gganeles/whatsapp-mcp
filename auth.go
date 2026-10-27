package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// AuthServer is a minimal OAuth 2.1 authorization server implementing what MCP clients
// such as claude.ai custom connectors need: authorization server and protected resource
// metadata, dynamic client registration, the authorization code flow with mandatory PKCE,
// and rotating refresh tokens.
//
// Consent is gated by an account's phone number and password, and every code and token
// is bound to that one account. Only hashes of codes and tokens are stored, so a leaked
// database can't be replayed.
type AuthServer struct {
	*AccountStore
	base      *url.URL // public base URL, e.g. https://wa.example.com
	redirects RedirectPolicy
	limiter   *loginLimiter

	mu      sync.Mutex
	pending map[string]*pendingAuth // consent pages that haven't been submitted yet
}

const (
	scopeAll         = "whatsapp"
	accessTokenTTL   = time.Hour
	refreshTokenTTL  = 90 * 24 * time.Hour
	authCodeTTL      = 2 * time.Minute
	consentTTL       = 10 * time.Minute
	maxClients       = 200
	minPasswordChars = 12
)

const authSchema = `
CREATE TABLE IF NOT EXISTS oauth_clients (
	client_id     TEXT PRIMARY KEY,
	client_name   TEXT NOT NULL DEFAULT '',
	redirect_uris TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
	id            TEXT PRIMARY KEY, -- phone number, digits only
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_codes (
	code_hash      TEXT PRIMARY KEY,
	account_id     TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
	client_id      TEXT NOT NULL,
	redirect_uri   TEXT NOT NULL,
	code_challenge TEXT NOT NULL,
	scope          TEXT NOT NULL,
	resource       TEXT NOT NULL,
	expires_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_tokens (
	token_hash TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
	kind       TEXT NOT NULL, -- access or refresh
	client_id  TEXT NOT NULL,
	scope      TEXT NOT NULL,
	resource   TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS oauth_tokens_expiry ON oauth_tokens (expires_at);
CREATE INDEX IF NOT EXISTS oauth_tokens_account ON oauth_tokens (account_id);
`

type pendingAuth struct {
	clientID, clientName, redirectURI, state, challenge, scope, resource string
	expires                                                              time.Time
}

// dummyHash is compared against when an unknown phone number signs in, so the response
// time doesn't reveal which numbers have accounts.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not a real password"), 12)

// RedirectPolicy decides which OAuth redirect URIs clients may register.
type RedirectPolicy struct {
	Exact    []string // exact URIs, e.g. the claude.ai callback
	Prefixes []string // extra allowed prefixes from --allow-redirect
	Loopback bool     // allow http://localhost:*/ and http://127.0.0.1:*/ (native apps, RFC 8252)
}

var defaultRedirects = RedirectPolicy{
	Exact: []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://claude.com/api/mcp/auth_callback",
	},
	Loopback: true,
}

func (p RedirectPolicy) Allowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil {
		return false
	}
	for _, e := range p.Exact {
		if raw == e {
			return true
		}
	}
	for _, prefix := range p.Prefixes {
		if strings.HasPrefix(raw, prefix) {
			return true
		}
	}
	if p.Loopback && u.Scheme == "http" {
		host := u.Hostname()
		if host == "localhost" || net.ParseIP(host).IsLoopback() {
			return true
		}
	}
	return false
}

func NewAuthServer(accounts *AccountStore, publicURL string, redirects RedirectPolicy) (*AuthServer, error) {
	base, err := url.Parse(strings.TrimRight(publicURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && !isLoopbackHost(base.Hostname())) {
		return nil, fmt.Errorf("public URL must be an https:// URL (or http://localhost for testing), got %q", publicURL)
	}
	a := &AuthServer{
		AccountStore: accounts,
		base:         base,
		redirects:    redirects,
		limiter:      newLoginLimiter(),
		pending:      make(map[string]*pendingAuth),
	}
	go a.cleanupLoop()
	return a, nil
}

func (a *AuthServer) url(path string) string { return a.base.String() + path }

// ResourceURL is the canonical URL of the protected MCP endpoint.
func (a *AuthServer) ResourceURL() string { return a.url("/mcp") }

func (a *AuthServer) cleanupLoop() {
	for range time.Tick(10 * time.Minute) {
		now := time.Now().Unix()
		_, _ = a.db.Exec(`DELETE FROM oauth_codes WHERE expires_at < ?`, now)
		_, _ = a.db.Exec(`DELETE FROM oauth_tokens WHERE expires_at < ?`, now)
		a.mu.Lock()
		for id, p := range a.pending {
			if time.Now().After(p.expires) {
				delete(a.pending, id)
			}
		}
		a.mu.Unlock()
	}
}

// Register mounts the OAuth endpoints on mux.
func (a *AuthServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", a.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.authServerMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", a.authServerMetadata)
	mux.HandleFunc("POST /register", a.register)
	mux.HandleFunc("GET /authorize", a.authorizePage)
	mux.HandleFunc("POST /authorize", a.authorizeSubmit)
	mux.HandleFunc("POST /token", a.token)
	mux.HandleFunc("POST /revoke", a.revoke)
}

type accountKey struct{}

// AccountFromContext returns the account a request was authenticated as.
func AccountFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(accountKey{}).(string)
	return id, ok && id != ""
}

// Protect wraps the MCP handler so that only requests with a valid access token get through,
// and records which account the token belongs to in the request context.
func (a *AuthServer) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		account := ""
		if ok && token != "" {
			account = a.accessTokenAccount(r.Context(), token)
		}
		if account == "" {
			challenge := fmt.Sprintf(`Bearer resource_metadata="%s", scope="%s"`, a.url("/.well-known/oauth-protected-resource/mcp"), scopeAll)
			if ok {
				challenge += `, error="invalid_token"`
			}
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey{}, account)))
	})
}

// accessTokenAccount returns the account a valid access token belongs to, or "".
func (a *AuthServer) accessTokenAccount(ctx context.Context, token string) string {
	var expires int64
	var resource, account string
	err := a.db.QueryRowContext(ctx, `SELECT t.expires_at, t.resource, t.account_id FROM oauth_tokens t
		JOIN accounts a ON a.id = t.account_id WHERE t.token_hash = ? AND t.kind = 'access'`,
		hashSecret(token)).Scan(&expires, &resource, &account)
	if err != nil || time.Now().Unix() >= expires || resource != a.ResourceURL() {
		return ""
	}
	return account
}

func (a *AuthServer) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 a.ResourceURL(),
		"authorization_servers":    []string{a.base.String()},
		"scopes_supported":         []string{scopeAll},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "WhatsApp MCP",
	})
}

func (a *AuthServer) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         a.base.String(),
		"authorization_endpoint":                         a.url("/authorize"),
		"token_endpoint":                                 a.url("/token"),
		"registration_endpoint":                          a.url("/register"),
		"revocation_endpoint":                            a.url("/revoke"),
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"scopes_supported":                               []string{scopeAll},
		"authorization_response_iss_parameter_supported": true,
	})
}

// register implements RFC 7591 dynamic client registration for public clients.
func (a *AuthServer) register(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.allowRegistration() {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registrations, try again later")
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "between 1 and 10 redirect_uris are required")
		return
	}
	for _, uri := range req.RedirectURIs {
		if !a.redirects.Allowed(uri) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect URI not allowed by this server: "+uri+" (the owner can allow it with --allow-redirect)")
			return
		}
	}
	var count int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM oauth_clients`).Scan(&count)
	if count >= maxClients {
		// Drop the oldest clients that have no live tokens to keep the table bounded.
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM oauth_clients WHERE client_id IN (
			SELECT client_id FROM oauth_clients WHERE client_id NOT IN (SELECT client_id FROM oauth_tokens)
			ORDER BY created_at LIMIT 50)`)
	}
	name := truncate(strings.TrimSpace(req.ClientName), 100)
	clientID := randomToken(16)
	uris, _ := json.Marshal(req.RedirectURIs)
	now := time.Now()
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO oauth_clients (client_id, client_name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		clientID, name, string(uris), now.Unix()); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "failed to register client")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        now.Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      scopeAll,
	})
}

func (a *AuthServer) lookupClient(ctx context.Context, clientID string) (name string, redirectURIs []string, err error) {
	var raw string
	err = a.db.QueryRowContext(ctx, `SELECT client_name, redirect_uris FROM oauth_clients WHERE client_id = ?`, clientID).Scan(&name, &raw)
	if err != nil {
		return "", nil, err
	}
	err = json.Unmarshal([]byte(raw), &redirectURIs)
	return
}

func (a *AuthServer) authorizePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID, redirectURI := q.Get("client_id"), q.Get("redirect_uri")
	name, uris, err := a.lookupClient(r.Context(), clientID)
	if err != nil {
		// Never redirect to an unverified URI; show the error instead.
		renderMessage(w, http.StatusBadRequest, "Unknown client", "This app isn't registered with this server. Try adding the connector again.")
		return
	}
	if redirectURI == "" && len(uris) == 1 {
		redirectURI = uris[0]
	}
	if !contains(uris, redirectURI) {
		renderMessage(w, http.StatusBadRequest, "Invalid redirect", "The redirect URI doesn't match the one this app registered.")
		return
	}
	fail := func(code, desc string) {
		redirectWithParams(w, r, redirectURI, url.Values{"error": {code}, "error_description": {desc}, "state": {q.Get("state")}, "iss": {a.base.String()}})
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	resource := q.Get("resource")
	if resource == "" {
		resource = a.ResourceURL()
	}
	if !a.validResource(resource) {
		fail("invalid_target", "unknown resource")
		return
	}
	id := randomToken(24)
	p := &pendingAuth{
		clientID: clientID, clientName: name, redirectURI: redirectURI, state: q.Get("state"),
		challenge: q.Get("code_challenge"), scope: scopeAll, resource: a.ResourceURL(),
		expires: time.Now().Add(consentTTL),
	}
	a.mu.Lock()
	if len(a.pending) >= 1000 {
		a.mu.Unlock()
		renderMessage(w, http.StatusTooManyRequests, "Too many requests", "Too many sign-in requests are open. Try again in a few minutes.")
		return
	}
	a.pending[id] = p
	a.mu.Unlock()
	a.renderConsent(w, r, http.StatusOK, id, p, "")
}

func (a *AuthServer) validResource(resource string) bool {
	resource = strings.TrimRight(resource, "/")
	return resource == a.ResourceURL() || resource == a.base.String()
}

func (a *AuthServer) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		renderMessage(w, http.StatusBadRequest, "Bad request", "The form couldn't be read.")
		return
	}
	id := r.PostForm.Get("request")
	a.mu.Lock()
	p := a.pending[id]
	a.mu.Unlock()
	if p == nil || time.Now().After(p.expires) {
		renderMessage(w, http.StatusBadRequest, "Request expired", "This sign-in request expired. Start again from your app.")
		return
	}
	if r.PostForm.Get("action") == "deny" {
		a.dropPending(id)
		redirectWithParams(w, r, p.redirectURI, url.Values{"error": {"access_denied"}, "state": {p.state}, "iss": {a.base.String()}})
		return
	}
	account, err := NormalizePhone(r.PostForm.Get("phone"))
	if err != nil {
		a.renderConsent(w, r, http.StatusBadRequest, id, p, "Enter your WhatsApp number in international format, e.g. +14155551234.")
		return
	}
	if wait := a.limiter.blockedFor(account); wait > 0 {
		a.renderConsent(w, r, http.StatusTooManyRequests, id, p,
			fmt.Sprintf("Too many failed sign-ins. Try again in %d minutes.", int(wait.Minutes())+1))
		return
	}
	hash, err := a.PasswordHash(r.Context(), account)
	if err != nil {
		hash = dummyHash
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(r.PostForm.Get("password"))) != nil || err != nil {
		a.limiter.fail(account)
		a.renderConsent(w, r, http.StatusUnauthorized, id, p, "Wrong phone number or password.")
		return
	}
	a.limiter.succeed(account)
	a.dropPending(id)

	code := randomToken(32)
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO oauth_codes (code_hash, account_id, client_id, redirect_uri, code_challenge, scope, resource, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, hashSecret(code), account, p.clientID, p.redirectURI, p.challenge, p.scope, p.resource,
		time.Now().Add(authCodeTTL).Unix()); err != nil {
		renderMessage(w, http.StatusInternalServerError, "Server error", "Couldn't complete sign-in.")
		return
	}
	redirectWithParams(w, r, p.redirectURI, url.Values{"code": {code}, "state": {p.state}, "iss": {a.base.String()}})
}

func (a *AuthServer) dropPending(id string) {
	a.mu.Lock()
	delete(a.pending, id)
	a.mu.Unlock()
}

func (a *AuthServer) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	f := r.PostForm
	switch f.Get("grant_type") {
	case "authorization_code":
		a.exchangeCode(w, r, f)
	case "refresh_token":
		a.refresh(w, r, f)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

func (a *AuthServer) exchangeCode(w http.ResponseWriter, r *http.Request, f url.Values) {
	ctx := r.Context()
	var account, clientID, redirectURI, challenge, scope, resource string
	var expires int64
	// Codes are single use: delete and read in one statement so a replay finds nothing.
	err := a.db.QueryRowContext(ctx, `DELETE FROM oauth_codes WHERE code_hash = ?
		RETURNING account_id, client_id, redirect_uri, code_challenge, scope, resource, expires_at`, hashSecret(f.Get("code"))).
		Scan(&account, &clientID, &redirectURI, &challenge, &scope, &resource, &expires)
	if err != nil || time.Now().Unix() > expires {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if f.Get("client_id") != clientID || f.Get("redirect_uri") != redirectURI {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri doesn't match the authorization request")
		return
	}
	if res := f.Get("resource"); res != "" && !a.validResource(res) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}
	if !verifyPKCE(f.Get("code_verifier"), challenge) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	a.issueTokens(w, r, account, clientID, scope, resource)
}

func (a *AuthServer) refresh(w http.ResponseWriter, r *http.Request, f url.Values) {
	ctx := r.Context()
	var account, clientID, scope, resource string
	var expires int64
	// Refresh tokens rotate: the presented one is consumed and a new one issued.
	err := a.db.QueryRowContext(ctx, `DELETE FROM oauth_tokens WHERE token_hash = ? AND kind = 'refresh'
		RETURNING account_id, client_id, scope, resource, expires_at`, hashSecret(f.Get("refresh_token"))).
		Scan(&account, &clientID, &scope, &resource, &expires)
	if err != nil || time.Now().Unix() > expires {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
		return
	}
	if id := f.Get("client_id"); id != "" && id != clientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token was issued to another client")
		return
	}
	a.issueTokens(w, r, account, clientID, scope, resource)
}

func (a *AuthServer) issueTokens(w http.ResponseWriter, r *http.Request, account, clientID, scope, resource string) {
	access, refresh := randomToken(32), randomToken(32)
	now := time.Now()
	err := withTx(r.Context(), a.db, func(tx *sql.Tx) error {
		const q = `INSERT INTO oauth_tokens (token_hash, account_id, kind, client_id, scope, resource, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.Exec(q, hashSecret(access), account, "access", clientID, scope, resource, now.Add(accessTokenTTL).Unix()); err != nil {
			return err
		}
		_, err := tx.Exec(q, hashSecret(refresh), account, "refresh", clientID, scope, resource, now.Add(refreshTokenTTL).Unix())
		return err
	})
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "failed to issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

// revoke implements RFC 7009. It always answers 200 so it can't be used to probe tokens.
func (a *AuthServer) revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err == nil {
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM oauth_tokens WHERE token_hash = ?`, hashSecret(r.PostForm.Get("token")))
	}
	w.WriteHeader(http.StatusOK)
}

func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GeneratePassword returns a random, easy-to-type password with about 130 bits of entropy.
func GeneratePassword() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	var parts []string
	for len(s) > 0 {
		n := min(5, len(s))
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return strings.Join(parts, "-")
}

func HashPassword(password string) ([]byte, error) {
	if len([]rune(password)) < minPasswordChars {
		return nil, fmt.Errorf("password must be at least %d characters", minPasswordChars)
	}
	if len(password) > 72 {
		return nil, errors.New("password must be at most 72 bytes")
	}
	return bcrypt.GenerateFromPassword([]byte(password), 12)
}

func withTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeJSON(w, status, body)
}

func redirectWithParams(w http.ResponseWriter, r *http.Request, target string, params url.Values) {
	u, err := url.Parse(target)
	if err != nil {
		renderMessage(w, http.StatusBadRequest, "Invalid redirect", "")
		return
	}
	q := u.Query()
	for k, v := range params {
		if len(v) > 0 && v[0] != "" {
			q.Set(k, v[0])
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// loginLimiter slows down password guessing. Each account locks for 15 minutes after 5
// failures in 15 minutes, and all sign-ins lock after 30 failures across accounts, which
// stops spraying one password over many numbers. Limits aren't per IP because behind a
// tunnel every request comes from the same address.
type loginLimiter struct {
	mu            sync.Mutex
	failures      map[string][]time.Time // per account; "" holds all failures
	lockedUntil   map[string]time.Time
	registrations []time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: make(map[string][]time.Time), lockedUntil: make(map[string]time.Time)}
}

func (l *loginLimiter) blockedFor(account string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(time.Until(l.lockedUntil[account]), time.Until(l.lockedUntil[""]))
}

func (l *loginLimiter) fail(account string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for key, limit := range map[string]int{account: 5, "": 30} {
		l.failures[key] = append(pruneBefore(l.failures[key], now.Add(-15*time.Minute)), now)
		if len(l.failures[key]) >= limit {
			l.lockedUntil[key] = now.Add(15 * time.Minute)
			delete(l.failures, key)
		}
	}
	// Keep the maps from growing without bound when many numbers are tried.
	if len(l.failures) > 10000 {
		l.failures = map[string][]time.Time{"": l.failures[""]}
	}
}

func (l *loginLimiter) succeed(account string) {
	l.mu.Lock()
	delete(l.failures, account)
	l.mu.Unlock()
}

// allowRegistration caps dynamic client registrations at 30 per hour.
func (l *loginLimiter) allowRegistration() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.registrations = pruneBefore(l.registrations, now.Add(-time.Hour))
	if len(l.registrations) >= 30 {
		return false
	}
	l.registrations = append(l.registrations, now)
	return true
}

func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --bg: #f6f6f4; --card: #fff; --fg: #1d1d1b; --muted: #6b6b66; --accent: #1f7a4d; --err: #b42318; --line: #ddddd8; }
@media (prefers-color-scheme: dark) { :root { --bg: #161615; --card: #20201f; --fg: #ececea; --muted: #a2a29c; --accent: #3fae78; --err: #f97066; --line: #3a3a37; } }
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg);
  font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; padding: 16px; }
main { width: 100%; max-width: 400px; background: var(--card); border: 1px solid var(--line); border-radius: 14px; padding: 28px; }
h1 { font-size: 20px; margin: 0 0 8px; }
p { margin: 0 0 16px; color: var(--muted); }
strong { color: var(--fg); }
label { display: block; font-weight: 600; margin-bottom: 6px; }
input[type=password], input[type=tel] { width: 100%; padding: 12px; margin-bottom: 14px; font: inherit; border: 1px solid var(--line); border-radius: 8px; background: transparent; color: inherit; }
.row { display: flex; gap: 10px; margin-top: 18px; }
button { flex: 1; padding: 12px; font: inherit; font-weight: 600; border-radius: 8px; border: 1px solid var(--line); background: transparent; color: inherit; cursor: pointer; }
button.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
.error { color: var(--err); margin: 12px 0 0; }
</style></head><body><main>
<h1>{{.Title}}</h1>
{{if .Request}}
<p><strong>{{.ClientName}}</strong> wants to read and send messages on your WhatsApp account through this server.</p>
<p>It will return to <strong>{{.RedirectHost}}</strong>.</p>
<form method="post" action="/authorize">
<input type="hidden" name="request" value="{{.Request}}">
<label for="phone">WhatsApp number</label>
<input type="tel" id="phone" name="phone" autocomplete="username" placeholder="+14155551234" value="{{.Phone}}" autofocus>
<label for="password">Password</label>
<input type="password" id="password" name="password" autocomplete="current-password">
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<div class="row"><button type="submit" name="action" value="deny">Deny</button><button class="primary" type="submit" name="action" value="allow">Allow</button></div>
</form>
{{else}}<p>{{.Message}}</p>{{end}}
</main></body></html>`))

func (a *AuthServer) renderConsent(w http.ResponseWriter, r *http.Request, status int, id string, p *pendingAuth, errMsg string) {
	redirect, _ := url.Parse(p.redirectURI)
	name := p.clientName
	if name == "" {
		name = "An app"
	}
	// Chrome applies form-action to the redirect after submitting, so the client's origin must be allowed.
	setPageHeaders(w, redirect.Scheme+"://"+redirect.Host)
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, map[string]string{
		"Title": "Connect WhatsApp", "Request": id, "ClientName": name, "RedirectHost": redirect.Host, "Error": errMsg,
		"Phone": r.PostFormValue("phone"),
	})
}

func renderMessage(w http.ResponseWriter, status int, title, msg string) {
	setPageHeaders(w, "")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, map[string]string{"Title": title, "Message": msg})
}

func setPageHeaders(w http.ResponseWriter, formTarget string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' "+formTarget+"; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
}

func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}
