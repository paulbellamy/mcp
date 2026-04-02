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
			json.NewEncoder(w).Encode(meta)
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
			json.NewEncoder(w).Encode(prm)
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
		json.NewDecoder(r.Body).Decode(&req)

		if req.ClientName != "mcp-cli" {
			t.Errorf("expected client name 'mcp-cli', got %q", req.ClientName)
		}
		if len(req.RedirectURIs) != 1 || req.RedirectURIs[0] != "http://localhost:8080/callback" {
			t.Errorf("unexpected redirect URIs: %v", req.RedirectURIs)
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(clientRegistrationResponse{
			ClientID:     "new-client-id",
			ClientSecret: "new-client-secret",
		})
	}))
	defer srv.Close()

	meta := &authServerMetadata{
		RegistrationEndpoint: srv.URL,
	}

	reg, err := registerClient(meta, "http://localhost:8080/callback")
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
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("expected grant_type=refresh_token, got %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "old-rt" {
			t.Errorf("expected refresh_token=old-rt, got %q", r.Form.Get("refresh_token"))
		}

		json.NewEncoder(w).Encode(tokenResponse{
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
		json.NewEncoder(w).Encode(tokenResponse{
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
			json.NewEncoder(w).Encode(authServerMetadata{
				Issuer:                           "https://auth.example.com",
				AuthorizationEndpoint:            "https://auth.example.com/authorize",
				TokenEndpoint:                    "https://auth.example.com/token",
				CodeChallengeMethodsSupported:     []string{"plain"},
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
			json.NewEncoder(w).Encode(authServerMetadata{
				Issuer:                           "https://auth.example.com",
				AuthorizationEndpoint:            "https://auth.example.com/authorize",
				TokenEndpoint:                    "https://auth.example.com/token",
				CodeChallengeMethodsSupported:     []string{"S256", "plain"},
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
			json.NewEncoder(w).Encode(authServerMetadata{
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
		json.NewEncoder(w).Encode(tokenResponse{
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
