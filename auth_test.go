package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPKCE_CodeVerifierFormat(t *testing.T) {
	verifier := generateCodeVerifier()

	// RFC 7636: code verifier is 43-128 characters of [A-Z] / [a-z] / [0-9] / "-" / "." / "_" / "~"
	if len(verifier) < 43 {
		t.Errorf("code verifier too short: %d chars", len(verifier))
	}
	if len(verifier) > 128 {
		t.Errorf("code verifier too long: %d chars", len(verifier))
	}

	// Verify it's base64url-encoded (no padding)
	if _, err := base64.RawURLEncoding.DecodeString(verifier); err != nil {
		t.Errorf("code verifier is not valid base64url: %v", err)
	}
}

func TestPKCE_CodeVerifierUniqueness(t *testing.T) {
	v1 := generateCodeVerifier()
	v2 := generateCodeVerifier()
	if v1 == v2 {
		t.Error("two code verifiers should not be equal")
	}
}

func TestPKCE_S256Challenge(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

	challenge := computeCodeChallenge(verifier)

	// Manually compute expected: SHA256(verifier) → base64url
	h := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(h[:])

	if challenge != expected {
		t.Errorf("expected challenge %q, got %q", expected, challenge)
	}
}

func TestPKCE_NonceFormat(t *testing.T) {
	nonce := generateNonce()
	if len(nonce) == 0 {
		t.Error("nonce should not be empty")
	}
	if _, err := base64.RawURLEncoding.DecodeString(nonce); err != nil {
		t.Errorf("nonce is not valid base64url: %v", err)
	}
}

func TestPKCE_NonceUniqueness(t *testing.T) {
	n1 := generateNonce()
	n2 := generateNonce()
	if n1 == n2 {
		t.Error("two nonces should not be equal")
	}
}

func TestBuildAuthorizationURL(t *testing.T) {
	meta := &authServerMetadata{
		AuthorizationEndpoint: "https://auth.example.com/authorize",
		TokenEndpoint:         "https://auth.example.com/token",
		ScopesSupported:       []string{"openid", "profile"},
	}

	authURL := buildAuthorizationURL(meta, "client-123", "http://localhost:8080/callback", "challenge-abc", "state-xyz", "https://api.example.com")

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}

	q := parsed.Query()
	if q.Get("response_type") != "code" {
		t.Errorf("expected response_type=code, got %q", q.Get("response_type"))
	}
	if q.Get("client_id") != "client-123" {
		t.Errorf("expected client_id=client-123, got %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://localhost:8080/callback" {
		t.Errorf("expected redirect_uri=http://localhost:8080/callback, got %q", q.Get("redirect_uri"))
	}
	if q.Get("code_challenge") != "challenge-abc" {
		t.Errorf("expected code_challenge=challenge-abc, got %q", q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("expected code_challenge_method=S256, got %q", q.Get("code_challenge_method"))
	}
	if q.Get("state") != "state-xyz" {
		t.Errorf("expected state=state-xyz, got %q", q.Get("state"))
	}
	if q.Get("resource") != "https://api.example.com" {
		t.Errorf("expected resource=https://api.example.com, got %q", q.Get("resource"))
	}
	if q.Get("scope") != "openid profile" {
		t.Errorf("expected scope='openid profile', got %q", q.Get("scope"))
	}
}

func TestDiscoverOAuth_Metadata(t *testing.T) {
	// Mock auth server metadata
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			meta := authServerMetadata{
				Issuer:                "https://auth.example.com",
				AuthorizationEndpoint: "https://auth.example.com/authorize",
				TokenEndpoint:         "https://auth.example.com/token",
				RegistrationEndpoint:  "https://auth.example.com/register",
			}
			_ = json.NewEncoder(w).Encode(meta)
			return
		}
		w.WriteHeader(404)
	}))
	defer authSrv.Close()

	// Mock resource server that points to auth server
	resourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			prm := protectedResourceMetadata{
				AuthorizationServers: []string{authSrv.URL},
				Resource:             "https://api.example.com",
			}
			_ = json.NewEncoder(w).Encode(prm)
			return
		}
		w.WriteHeader(404)
	}))
	defer resourceSrv.Close()

	resource, authMeta, err := discoverOAuth(resourceSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resource != "https://api.example.com" {
		t.Errorf("expected resource 'https://api.example.com', got %q", resource)
	}
	if authMeta.TokenEndpoint != "https://auth.example.com/token" {
		t.Errorf("expected token endpoint 'https://auth.example.com/token', got %q", authMeta.TokenEndpoint)
	}
	if authMeta.RegistrationEndpoint != "https://auth.example.com/register" {
		t.Errorf("expected registration endpoint, got %q", authMeta.RegistrationEndpoint)
	}
}

func TestRegisterClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req clientRegistrationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.ClientName != "mcp-cli" {
			t.Errorf("expected client name 'mcp-cli', got %q", req.ClientName)
		}
		if len(req.RedirectURIs) != 1 || req.RedirectURIs[0] != "http://localhost:8080/callback" {
			t.Errorf("unexpected redirect URIs: %v", req.RedirectURIs)
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(clientRegistrationResponse{
			ClientID:     "new-client-id",
			ClientSecret: "new-client-secret",
		})
	}))
	defer srv.Close()

	meta := &authServerMetadata{
		RegistrationEndpoint: srv.URL,
	}

	reg, err := registerClient(meta, "http://localhost:8080/callback", "client_secret_basic")
	if err != nil {
		t.Fatal(err)
	}
	if reg.ClientID != "new-client-id" {
		t.Errorf("expected client ID 'new-client-id', got %q", reg.ClientID)
	}
	if reg.ClientSecret != "new-client-secret" {
		t.Errorf("expected client secret 'new-client-secret', got %q", reg.ClientSecret)
	}
}

func TestRefreshOAuthToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("expected grant_type=refresh_token, got %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "old-rt" {
			t.Errorf("expected refresh_token=old-rt, got %q", r.Form.Get("refresh_token"))
		}

		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "new-at",
			RefreshToken: "new-rt",
			ExpiresIn:    3600,
		})
	}))
	defer srv.Close()

	tokens := &AuthTokens{
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ClientID:     "cid",
		TokenURL:     srv.URL,
	}

	refreshed, err := refreshOAuthToken(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "new-at" {
		t.Errorf("expected access token 'new-at', got %q", refreshed.AccessToken)
	}
	if refreshed.RefreshToken != "new-rt" {
		t.Errorf("expected refresh token 'new-rt', got %q", refreshed.RefreshToken)
	}
	if refreshed.ExpiresAt == 0 {
		t.Error("expected non-zero ExpiresAt")
	}
}

func TestRefreshOAuthToken_ClientSecretPost(t *testing.T) {
	var gotBasic, gotBodySecret bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, gotBasic = r.BasicAuth()
		_ = r.ParseForm()
		gotBodySecret = r.Form.Get("client_secret") == "sec"
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-at", RefreshToken: "new-rt"})
	}))
	defer srv.Close()

	tokens := &AuthTokens{
		RefreshToken:            "old-rt",
		ClientID:                "cid",
		ClientSecret:            "sec",
		TokenURL:                srv.URL,
		TokenEndpointAuthMethod: "client_secret_post",
	}

	refreshed, err := refreshOAuthToken(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if gotBasic {
		t.Error("client_secret_post must not use Basic auth")
	}
	if !gotBodySecret {
		t.Error("client_secret_post must send client_secret in the form body")
	}
	if refreshed.TokenEndpointAuthMethod != "client_secret_post" {
		t.Errorf("refreshed token dropped auth method: got %q", refreshed.TokenEndpointAuthMethod)
	}
}

func TestChooseTokenAuthMethod(t *testing.T) {
	tests := []struct {
		name      string
		supported []string
		want      string
		wantErr   bool
	}{
		{"absent defaults to basic (RFC 8414)", nil, "client_secret_basic", false},
		{"notion: all three prefers basic", []string{"client_secret_basic", "client_secret_post", "none"}, "client_secret_basic", false},
		{"context7: post only", []string{"client_secret_post"}, "client_secret_post", false},
		{"sprites: none and post prefers post", []string{"none", "client_secret_post"}, "client_secret_post", false},
		{"none only", []string{"none"}, "none", false},
		{"unsupported method", []string{"private_key_jwt", "tls_client_auth"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chooseTokenAuthMethod(tt.supported)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDoTokenRequest_AuthMethods(t *testing.T) {
	tests := []struct {
		name           string
		authMethod     string
		clientSecret   string
		wantBasic      bool
		wantBodySecret bool
	}{
		{"basic uses Authorization header", "client_secret_basic", "sec", true, false},
		{"post puts secret in body", "client_secret_post", "sec", false, true},
		{"none sends no secret", "none", "", false, false},
		{"empty legacy falls back to basic", "", "sec", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBasic, gotBodySecret bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _, gotBasic = r.BasicAuth()
				_ = r.ParseForm()
				gotBodySecret = r.Form.Get("client_secret") != ""
				_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "at"})
			}))
			defer srv.Close()

			params := url.Values{"grant_type": {"authorization_code"}, "client_id": {"cid"}}
			if _, err := doTokenRequest(srv.URL, params, "cid", tt.clientSecret, tt.authMethod); err != nil {
				t.Fatal(err)
			}
			if gotBasic != tt.wantBasic {
				t.Errorf("basic auth: got %v, want %v", gotBasic, tt.wantBasic)
			}
			if gotBodySecret != tt.wantBodySecret {
				t.Errorf("body client_secret: got %v, want %v", gotBodySecret, tt.wantBodySecret)
			}
		})
	}
}

func TestCmdAuth_TLSEnforcement(t *testing.T) {
	setupTestConfigDir(t)

	server := ServerConfig{
		Name:      "insecure",
		Transport: "streamable-http",
		URL:       "http://evil.example.com/mcp",
	}
	if err := addServerConfig(server); err != nil {
		t.Fatal(err)
	}

	err := cmdAuth([]string{"insecure"})
	if err == nil {
		t.Fatal("expected error for non-HTTPS server")
	}
	if !strings.Contains(err.Error(), "requires HTTPS") {
		t.Errorf("expected TLS enforcement error, got: %v", err)
	}

	// localhost should be allowed
	localServer := ServerConfig{
		Name:      "local",
		Transport: "streamable-http",
		URL:       "http://localhost:8080/mcp",
	}
	if err := addServerConfig(localServer); err != nil {
		t.Fatal(err)
	}

	// This will fail at OAuth discovery (no server running), not at TLS check
	err = cmdAuth([]string{"local"})
	if err != nil && strings.Contains(err.Error(), "requires HTTPS") {
		t.Errorf("localhost should be exempt from TLS requirement, got: %v", err)
	}
}

func TestBuildRelayRedirectURI_IncludesNonceAndTimestamp(t *testing.T) {
	before := time.Now().Unix()
	uri := buildRelayRedirectURI("https://example.com/callback", "nonce-abc")
	after := time.Now().Unix()

	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}

	// Path should be unchanged from the callback URL
	if parsed.Path != "/callback" {
		t.Errorf("unexpected path: %s", parsed.Path)
	}

	// Check nonce param
	if parsed.Query().Get("nonce") != "nonce-abc" {
		t.Errorf("expected nonce=nonce-abc, got %q", parsed.Query().Get("nonce"))
	}

	// Check timestamp param
	tStr := parsed.Query().Get("t")
	if tStr == "" {
		t.Fatal("expected 't' query parameter")
	}
	tVal, err := strconv.ParseInt(tStr, 10, 64)
	if err != nil {
		t.Fatalf("invalid timestamp: %v", err)
	}
	if tVal < before || tVal > after {
		t.Errorf("timestamp %d not within expected range [%d, %d]", tVal, before, after)
	}
}

func TestBuildStartHandoffURL_RoundTrip(t *testing.T) {
	upstream := "https://auth.example.com/authorize?client_id=cid&redirect_uri=https%3A%2F%2Fgw.example.com%2Fapi%2Foauth%2Frelay%2Fagent-1%2Fcallback%3Fnonce%3Dn-abc%26t%3D1700000000&state=n-abc&code_challenge=cc"

	wrapped := buildStartHandoffURL(
		"https://gw.example.com/api/oauth/relay/agent-1/start",
		"n-abc", 1700000000, upstream,
	)

	parsed, err := url.Parse(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "gw.example.com" {
		t.Errorf("expected host gw.example.com, got %q", parsed.Host)
	}
	if parsed.Path != "/api/oauth/relay/agent-1/start" {
		t.Errorf("expected /api/oauth/relay/agent-1/start path, got %q", parsed.Path)
	}

	q := parsed.Query()
	if q.Get("nonce") != "n-abc" {
		t.Errorf("expected nonce=n-abc, got %q", q.Get("nonce"))
	}
	if q.Get("t") != "1700000000" {
		t.Errorf("expected t=1700000000, got %q", q.Get("t"))
	}
	if q.Get("destination") != upstream {
		t.Errorf("destination did not round-trip\n got: %s\nwant: %s", q.Get("destination"), upstream)
	}
}

// The relay-required nonce/t params must appear BEFORE the long `destination`
// blob in the raw query. An LLM relaying this URL tends to stop at
// destination's trailing `state=<nonce>`; with destination last, nothing after
// it can be truncated. See buildStartHandoffURL's doc comment.
func TestBuildStartHandoffURL_DestinationIsLast(t *testing.T) {
	upstream := "https://auth.example.com/authorize?client_id=cid&redirect_uri=https%3A%2F%2Fgw.example.com%2Fcb%3Fnonce%3Dn-abc%26t%3D1700000000&state=n-abc"

	wrapped := buildStartHandoffURL(
		"https://gw.example.com/api/oauth/relay/agent-1/start",
		"n-abc", 1700000000, upstream,
	)

	parsed, err := url.Parse(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	raw := parsed.RawQuery
	destIdx := strings.Index(raw, "destination=")
	nonceIdx := strings.Index(raw, "nonce=")
	tIdx := strings.Index(raw, "t=")
	if destIdx < 0 || nonceIdx < 0 || tIdx < 0 {
		t.Fatalf("missing params in raw query: %s", raw)
	}
	if destIdx < nonceIdx || destIdx < tIdx {
		t.Errorf("destination must be last; got order: %s", raw)
	}
}

func TestBuildShortStartURL(t *testing.T) {
	got := buildShortStartURL(
		"https://gw.example.com/api/oauth/relay/agent-1/start",
		"n-abc", 1700000000,
	)
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/api/oauth/relay/agent-1/start" {
		t.Errorf("unexpected path %q", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("nonce") != "n-abc" {
		t.Errorf("nonce=%q", q.Get("nonce"))
	}
	if q.Get("t") != "1700000000" {
		t.Errorf("t=%q", q.Get("t"))
	}
	if q.Get("destination") != "" {
		t.Errorf("short URL must carry no destination, got %q", q.Get("destination"))
	}
}

func TestRegisterRelayDestination_SendsAuthedPayload(t *testing.T) {
	var gotAuth, gotCT string
	payload := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	dest := "https://up.example.com/authorize?x=1"
	if err := registerRelayDestination(srv.URL, "tok123", "n-abc", 1700000000, dest); err != nil {
		t.Fatalf("register: %v", err)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if payload["nonce"] != "n-abc" || payload["t"] != "1700000000" || payload["destination"] != dest {
		t.Errorf("unexpected payload: %v", payload)
	}
}

func TestRegisterRelayDestination_ErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if err := registerRelayDestination(srv.URL, "tok", "n", 1700000000, "https://u.example.com/a"); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestCmdAuth_RelayMode_ShortURL(t *testing.T) {
	setupTestConfigDir(t)
	resourceURL := setupRelayAuthTestServer(t)

	if err := addServerConfig(ServerConfig{
		Name:      "test",
		Transport: "streamable-http",
		URL:       resourceURL,
	}); err != nil {
		t.Fatal(err)
	}

	registered := map[string]string{}
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer regtoken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&registered)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer regSrv.Close()

	out := runCmdAuthRelay(t,
		[]string{
			"test",
			"--callback-url", "http://localhost:9999/cb",
			"--start-url", "http://localhost:9999/start",
		},
		map[string]string{
			"MCP_CLIENT_ID":           "cid",
			"MCP_CLIENT_SECRET":       "secret",
			"MCP_AUTH_REGISTER_URL":   regSrv.URL,
			"MCP_AUTH_REGISTER_TOKEN": "regtoken",
		},
	)

	parsed, err := url.Parse(out.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/start" {
		t.Errorf("expected short /start path, got %q", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("nonce") != out.Nonce {
		t.Errorf("nonce=%q want %q", q.Get("nonce"), out.Nonce)
	}
	if q.Get("t") == "" {
		t.Error("missing t")
	}
	if q.Get("destination") != "" {
		t.Errorf("short URL must not carry destination, got %q", q.Get("destination"))
	}

	// The destination was registered out-of-band, not put in the URL.
	if registered["nonce"] != out.Nonce {
		t.Errorf("register nonce=%q want %q", registered["nonce"], out.Nonce)
	}
	dest := registered["destination"]
	dp, err := url.Parse(dest)
	if err != nil {
		t.Fatalf("parse registered destination: %v", err)
	}
	if !strings.HasSuffix(dp.Path, "/authorize") {
		t.Errorf("registered destination not upstream /authorize: %q", dest)
	}
	if dp.Query().Get("state") != out.Nonce {
		t.Errorf("registered destination state != nonce")
	}
}

func TestCmdAuth_RelayMode_ShortURLFallsBackOnRegisterError(t *testing.T) {
	setupTestConfigDir(t)
	resourceURL := setupRelayAuthTestServer(t)

	if err := addServerConfig(ServerConfig{
		Name:      "test",
		Transport: "streamable-http",
		URL:       resourceURL,
	}); err != nil {
		t.Fatal(err)
	}

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer regSrv.Close()

	out := runCmdAuthRelay(t,
		[]string{
			"test",
			"--callback-url", "http://localhost:9999/cb",
			"--start-url", "http://localhost:9999/start",
		},
		map[string]string{
			"MCP_CLIENT_ID":           "cid",
			"MCP_CLIENT_SECRET":       "secret",
			"MCP_AUTH_REGISTER_URL":   regSrv.URL,
			"MCP_AUTH_REGISTER_TOKEN": "regtoken",
		},
	)

	// Register failed → fall back to the inline handoff URL (carries destination).
	parsed, err := url.Parse(out.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("destination") == "" {
		t.Errorf("expected fallback inline URL to carry destination, got %q", out.AuthURL)
	}
}

func TestBuildStartHandoffURL_PreservesExistingQuery(t *testing.T) {
	wrapped := buildStartHandoffURL(
		"https://gw.example.com/start?tenant=acme",
		"n-1", 1700000000, "https://auth.example.com/authorize?x=1",
	)

	parsed, err := url.Parse(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("tenant") != "acme" {
		t.Errorf("pre-existing query param dropped, got tenant=%q", q.Get("tenant"))
	}
	if q.Get("nonce") != "n-1" || q.Get("t") != "1700000000" || q.Get("destination") == "" {
		t.Errorf("handoff params missing: %v", q)
	}
}

func TestBuildRelayRedirectURIAt_UsesProvidedTimestamp(t *testing.T) {
	uri := buildRelayRedirectURIAt("https://example.com/callback", "n", 1234567890)
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("t") != "1234567890" {
		t.Errorf("expected t=1234567890, got %q", parsed.Query().Get("t"))
	}
	if parsed.Query().Get("nonce") != "n" {
		t.Errorf("expected nonce=n, got %q", parsed.Query().Get("nonce"))
	}
}

// setupRelayAuthTestServer stands up a mock OAuth discovery + registration
// stack on localhost so cmdAuth in relay mode can run to the auth-URL step.
// Returns the resource-server URL to use as the MCP server URL.
func setupRelayAuthTestServer(t *testing.T) string {
	t.Helper()

	var authSrv *httptest.Server
	authSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			_ = json.NewEncoder(w).Encode(authServerMetadata{
				Issuer:                        authSrv.URL,
				AuthorizationEndpoint:         authSrv.URL + "/authorize",
				TokenEndpoint:                 authSrv.URL + "/token",
				CodeChallengeMethodsSupported: []string{"S256"},
			})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(authSrv.Close)

	var resourceSrv *httptest.Server
	resourceSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
				AuthorizationServers: []string{authSrv.URL},
				Resource:             resourceSrv.URL,
			})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(resourceSrv.Close)

	return resourceSrv.URL
}

func runCmdAuthRelay(t *testing.T, args []string, env map[string]string) authOutput {
	t.Helper()

	for k, v := range env {
		t.Setenv(k, v)
	}

	var out authOutput
	stdout := captureStdout(t, func() {
		if err := cmdAuth(args); err != nil {
			t.Fatalf("cmdAuth: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("parse cmdAuth JSON: %v\nraw: %s", err, stdout)
	}
	return out
}

func TestCmdAuth_RelayMode_DynamicRegistration_HonorsServerAuthMethod(t *testing.T) {
	setupTestConfigDir(t)

	// We'd negotiate basic, but the server assigns post; RFC 7591 says its
	// assignment wins.
	var authSrv *httptest.Server
	authSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(authServerMetadata{
				Issuer:                            authSrv.URL,
				AuthorizationEndpoint:             authSrv.URL + "/authorize",
				TokenEndpoint:                     authSrv.URL + "/token",
				RegistrationEndpoint:              authSrv.URL + "/register",
				CodeChallengeMethodsSupported:     []string{"S256"},
				TokenEndpointAuthMethodsSupported: []string{"client_secret_basic", "client_secret_post"},
			})
		case "/register":
			var req clientRegistrationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.TokenEndpointAuthMethod != "client_secret_basic" {
				t.Errorf("expected negotiated request method client_secret_basic, got %q", req.TokenEndpointAuthMethod)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(clientRegistrationResponse{
				ClientID:                "srv-cid",
				ClientSecret:            "srv-secret",
				TokenEndpointAuthMethod: "client_secret_post",
			})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(authSrv.Close)

	var resourceSrv *httptest.Server
	resourceSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
				AuthorizationServers: []string{authSrv.URL},
				Resource:             resourceSrv.URL,
			})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(resourceSrv.Close)

	if err := addServerConfig(ServerConfig{Name: "test", Transport: "streamable-http", URL: resourceSrv.URL}); err != nil {
		t.Fatal(err)
	}

	// No MCP_CLIENT_ID -> dynamic registration path.
	out := runCmdAuthRelay(t, []string{"test", "--callback-url", "http://localhost:9999/cb"}, nil)

	pending, _, err := findPendingAuthByNonce(out.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil {
		t.Fatal("no pending auth persisted")
	}
	if pending.TokenEndpointAuthMethod != "client_secret_post" {
		t.Errorf("persisted method should honor server assignment, got %q", pending.TokenEndpointAuthMethod)
	}
	if pending.ClientID != "srv-cid" || pending.ClientSecret != "srv-secret" {
		t.Errorf("unexpected persisted client creds: %q / %q", pending.ClientID, pending.ClientSecret)
	}
}

func TestCmdAuth_RelayMode_NoStartURL_EmitsRawUpstream(t *testing.T) {
	setupTestConfigDir(t)
	resourceURL := setupRelayAuthTestServer(t)

	if err := addServerConfig(ServerConfig{
		Name:      "test",
		Transport: "streamable-http",
		URL:       resourceURL,
	}); err != nil {
		t.Fatal(err)
	}

	out := runCmdAuthRelay(t,
		[]string{"test", "--callback-url", "http://localhost:9999/cb"},
		map[string]string{"MCP_CLIENT_ID": "cid", "MCP_CLIENT_SECRET": "secret"},
	)

	parsed, err := url.Parse(out.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(parsed.Path, "/authorize") {
		t.Errorf("expected upstream /authorize URL, got %q", out.AuthURL)
	}
	if parsed.Query().Get("state") != out.Nonce {
		t.Errorf("expected state=%q, got %q", out.Nonce, parsed.Query().Get("state"))
	}
	redirectURI := parsed.Query().Get("redirect_uri")
	if redirectURI == "" {
		t.Fatal("missing redirect_uri")
	}
	if !strings.HasPrefix(redirectURI, "http://localhost:9999/cb?") {
		t.Errorf("unexpected redirect_uri prefix: %s", redirectURI)
	}
}

func TestCmdAuth_RelayMode_WithStartURL_WrapsURL(t *testing.T) {
	setupTestConfigDir(t)
	resourceURL := setupRelayAuthTestServer(t)

	if err := addServerConfig(ServerConfig{
		Name:      "test",
		Transport: "streamable-http",
		URL:       resourceURL,
	}); err != nil {
		t.Fatal(err)
	}

	out := runCmdAuthRelay(t,
		[]string{
			"test",
			"--callback-url", "http://localhost:9999/cb",
			"--start-url", "http://localhost:9999/start",
		},
		map[string]string{"MCP_CLIENT_ID": "cid", "MCP_CLIENT_SECRET": "secret"},
	)

	parsed, err := url.Parse(out.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/start" {
		t.Errorf("expected /start path, got %q", parsed.Path)
	}

	q := parsed.Query()
	if q.Get("nonce") != out.Nonce {
		t.Errorf("expected outer nonce=%q, got %q", out.Nonce, q.Get("nonce"))
	}
	outerT := q.Get("t")
	if outerT == "" {
		t.Fatal("missing outer t")
	}
	dest := q.Get("destination")
	if dest == "" {
		t.Fatal("missing destination")
	}

	destParsed, err := url.Parse(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(destParsed.Path, "/authorize") {
		t.Errorf("expected destination to be upstream /authorize, got %q", dest)
	}
	if destParsed.Query().Get("state") != out.Nonce {
		t.Errorf("destination state != outer nonce: %q vs %q", destParsed.Query().Get("state"), out.Nonce)
	}

	rURI := destParsed.Query().Get("redirect_uri")
	rParsed, err := url.Parse(rURI)
	if err != nil {
		t.Fatalf("parse redirect_uri: %v", err)
	}
	if rParsed.Query().Get("nonce") != out.Nonce {
		t.Errorf("redirect_uri nonce != outer nonce")
	}
	if rParsed.Query().Get("t") != outerT {
		t.Errorf("redirect_uri t (%q) != outer t (%q)", rParsed.Query().Get("t"), outerT)
	}
}

func TestCmdAuth_RelayMode_StartURLEnvVarFallback(t *testing.T) {
	setupTestConfigDir(t)
	resourceURL := setupRelayAuthTestServer(t)

	if err := addServerConfig(ServerConfig{
		Name:      "test",
		Transport: "streamable-http",
		URL:       resourceURL,
	}); err != nil {
		t.Fatal(err)
	}

	out := runCmdAuthRelay(t,
		[]string{"test", "--callback-url", "http://localhost:9999/cb"},
		map[string]string{
			"MCP_CLIENT_ID":      "cid",
			"MCP_CLIENT_SECRET":  "secret",
			"MCP_AUTH_START_URL": "http://localhost:9999/start",
		},
	)

	parsed, err := url.Parse(out.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/start" {
		t.Errorf("expected env-var start URL to wrap auth URL, got %q", out.AuthURL)
	}
}

func TestCmdAuth_RelayMode_InvalidStartURL(t *testing.T) {
	setupTestConfigDir(t)
	resourceURL := setupRelayAuthTestServer(t)

	if err := addServerConfig(ServerConfig{
		Name:      "test",
		Transport: "streamable-http",
		URL:       resourceURL,
	}); err != nil {
		t.Fatal(err)
	}

	err := cmdAuth([]string{
		"test",
		"--callback-url", "http://localhost:9999/cb",
		"--start-url", "http://evil.example.com/start",
	})
	if err == nil {
		t.Fatal("expected error for non-HTTPS public start URL")
	}
	if !strings.Contains(err.Error(), "start URL") {
		t.Errorf("expected error to mention 'start URL', got: %v", err)
	}
}

func TestGetAuthToken_ExpiredNoRefresh(t *testing.T) {
	setupTestConfigDir(t)

	// Save auth with expired token, no refresh token
	auth := &AuthTokens{
		AccessToken: "expired-at",
		ExpiresAt:   time.Now().Unix() - 100,
	}
	if err := saveAuth("test-expired", auth); err != nil {
		t.Fatal(err)
	}

	token, err := getAuthToken("test-expired")
	if err != nil {
		t.Fatal(err)
	}
	// Falls through and returns the expired token
	if token != "expired-at" {
		t.Errorf("expected 'expired-at', got %q", token)
	}
}

func TestGetAuthToken_ExpiredWithRefresh(t *testing.T) {
	setupTestConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "refreshed-at",
			ExpiresIn:   3600,
		})
	}))
	defer srv.Close()

	// Save auth with expired token and a refresh token
	auth := &AuthTokens{
		AccessToken:  "old-at",
		RefreshToken: "rt-123",
		ExpiresAt:    time.Now().Unix() - 100,
		ClientID:     "cid",
		TokenURL:     srv.URL,
	}
	if err := saveAuth("test-refresh", auth); err != nil {
		t.Fatal(err)
	}

	token, err := getAuthToken("test-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if token != "refreshed-at" {
		t.Errorf("expected 'refreshed-at', got %q", token)
	}

	// Verify saved auth was updated
	loaded, err := loadAuth("test-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccessToken != "refreshed-at" {
		t.Errorf("expected saved token 'refreshed-at', got %q", loaded.AccessToken)
	}
}

func TestFetchAuthServerMetadata_RejectsUnsupportedS256(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			_ = json.NewEncoder(w).Encode(authServerMetadata{
				Issuer:                        "https://auth.example.com",
				AuthorizationEndpoint:         "https://auth.example.com/authorize",
				TokenEndpoint:                 "https://auth.example.com/token",
				CodeChallengeMethodsSupported: []string{"plain"},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	_, err := fetchAuthServerMetadata(authHTTPClient, srv.URL)
	if err == nil {
		t.Fatal("expected error for server not supporting S256")
	}
	if !strings.Contains(err.Error(), "S256") {
		t.Errorf("expected error about S256, got: %v", err)
	}
}

func TestFetchAuthServerMetadata_AcceptsS256(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			_ = json.NewEncoder(w).Encode(authServerMetadata{
				Issuer:                        "https://auth.example.com",
				AuthorizationEndpoint:         "https://auth.example.com/authorize",
				TokenEndpoint:                 "https://auth.example.com/token",
				CodeChallengeMethodsSupported: []string{"S256", "plain"},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	meta, err := fetchAuthServerMetadata(authHTTPClient, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.TokenEndpoint != "https://auth.example.com/token" {
		t.Errorf("unexpected token endpoint: %s", meta.TokenEndpoint)
	}
}

func TestFetchAuthServerMetadata_AcceptsEmptyMethods(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			_ = json.NewEncoder(w).Encode(authServerMetadata{
				Issuer:                "https://auth.example.com",
				AuthorizationEndpoint: "https://auth.example.com/authorize",
				TokenEndpoint:         "https://auth.example.com/token",
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	meta, err := fetchAuthServerMetadata(authHTTPClient, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.TokenEndpoint != "https://auth.example.com/token" {
		t.Errorf("unexpected token endpoint: %s", meta.TokenEndpoint)
	}
}

func TestPrintUsage_NoAuthCallback(t *testing.T) {
	out := captureStderr(t, func() {
		printUsage()
	})
	if strings.Contains(out, "auth-callback") {
		t.Error("auth-callback should not appear in usage output")
	}
}

func TestRefreshOAuthToken_KeepsOldRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "new-at",
			// No new refresh token
			ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	tokens := &AuthTokens{
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ClientID:     "cid",
		TokenURL:     srv.URL,
	}

	refreshed, err := refreshOAuthToken(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken != "old-rt" {
		t.Errorf("expected old refresh token to be preserved, got %q", refreshed.RefreshToken)
	}
}
