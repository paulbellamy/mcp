package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var authHTTPClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: checkSafeRedirect,
}

func validateEndpointURL(endpoint, label string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid %s URL: %w", label, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		return nil
	}
	return fmt.Errorf("%s requires HTTPS (got %s)", label, endpoint)
}

// checkSafeRedirect re-validates the URL of each redirect hop so that a
// server cannot redirect a request originally aimed at a validated
// endpoint to an arbitrary internal address (SSRF).
func checkSafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return validateEndpointURL(req.URL.String(), "redirect target")
}

// OAuth discovery and authorization types

type protectedResourceMetadata struct {
	AuthorizationServers []string `json:"authorization_servers"`
	Resource             string   `json:"resource"`
}

type authServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported,omitempty"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
}

// chooseTokenAuthMethod picks a supported method from the server's advertised
// list. An absent list defaults to client_secret_basic per RFC 8414.
func chooseTokenAuthMethod(supported []string) (string, error) {
	if len(supported) == 0 {
		return "client_secret_basic", nil
	}
	for _, pref := range []string{"client_secret_basic", "client_secret_post", "none"} {
		for _, m := range supported {
			if m == pref {
				return pref, nil
			}
		}
	}
	return "", fmt.Errorf("no supported token endpoint auth method (server supports: %v)", supported)
}

type clientRegistrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type clientRegistrationResponse struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret,omitempty"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type tokenErrorResponse struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// cmdAuth handles the `mcp auth <name> [flags]` command.
func cmdAuth(args []string) error {
	cleanupExpiredPendingAuth()

	if len(args) < 1 {
		return fmt.Errorf("usage: mcp auth <name> [--callback-url <url>] [--start-url <url>]")
	}

	name := args[0]
	if err := validateServerName(name); err != nil {
		return err
	}

	var callbackURL string
	var startURL string

	// Parse flags
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--callback-url":
			if i+1 >= len(args) {
				return fmt.Errorf("--callback-url requires a value")
			}
			i++
			callbackURL = args[i]
		case "--start-url":
			if i+1 >= len(args) {
				return fmt.Errorf("--start-url requires a value")
			}
			i++
			startURL = args[i]
		default:
			return fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	// Read secrets from environment only
	token := os.Getenv("MCP_AUTH_TOKEN")
	clientID := os.Getenv("MCP_CLIENT_ID")
	clientSecret := os.Getenv("MCP_CLIENT_SECRET")

	// Env var fallback for flags
	if callbackURL == "" {
		callbackURL = os.Getenv("MCP_CALLBACK_URL")
	}
	if startURL == "" {
		startURL = os.Getenv("MCP_AUTH_START_URL")
	}

	// Manual token mode
	if token != "" {
		if err := saveAuth(name, &AuthTokens{AccessToken: token}); err != nil {
			return err
		}
		logStderr("token saved for %q", name)
		return outputJSON(authOutput{Status: "complete", Server: name})
	}

	// OAuth flow
	server, err := getServerConfig(name)
	if err != nil {
		return err
	}

	if server.Transport != "streamable-http" {
		return fmt.Errorf("OAuth is only supported for streamable-http servers (server %q uses %s)", name, server.Transport)
	}

	if err := validateEndpointURL(server.URL, "MCP server"); err != nil {
		return err
	}

	// Step 1: Discover OAuth server
	logStderr("discovering OAuth server for %s...", server.URL)
	resource, authMeta, err := discoverOAuth(server.URL)
	if err != nil {
		return fmt.Errorf("OAuth discovery failed: %w", err)
	}

	authMethod, err := chooseTokenAuthMethod(authMeta.TokenEndpointAuthMethodsSupported)
	if err != nil {
		return err
	}

	// Step 2: Compute redirect URI and PKCE up front
	nonce := generateNonce()
	codeVerifier := generateCodeVerifier()
	codeChallenge := computeCodeChallenge(codeVerifier)

	var redirectURI string
	var localListener net.Listener
	var relayTimestamp int64
	if callbackURL != "" {
		if err := validateEndpointURL(callbackURL, "callback URL"); err != nil {
			return err
		}
		if startURL != "" {
			if err := validateEndpointURL(startURL, "start URL"); err != nil {
				return err
			}
		}
		relayTimestamp = time.Now().Unix()
		redirectURI = buildRelayRedirectURIAt(callbackURL, nonce, relayTimestamp)
	} else {
		// Start listener now so we know the real port for registration
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("start localhost server: %w", err)
		}
		localListener = ln
		port := ln.Addr().(*net.TCPAddr).Port
		redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	}

	// Step 3: Get client credentials (dynamic registration or static)
	var regClientID, regClientSecret string

	if clientID != "" {
		regClientID = clientID
		regClientSecret = clientSecret
		logStderr("using static client credentials")
	} else {
		if authMeta.RegistrationEndpoint == "" {
			if localListener != nil {
				_ = localListener.Close()
			}
			return fmt.Errorf("server does not support dynamic client registration; set MCP_CLIENT_ID and MCP_CLIENT_SECRET env vars")
		}

		logStderr("registering client dynamically...")
		reg, err := registerClient(authMeta, redirectURI, authMethod)
		if err != nil {
			if localListener != nil {
				_ = localListener.Close()
			}
			return fmt.Errorf("client registration failed: %w", err)
		}
		regClientID = reg.ClientID
		regClientSecret = reg.ClientSecret
		// RFC 7591: the server's assigned method wins over the one we requested.
		if reg.TokenEndpointAuthMethod != "" {
			authMethod = reg.TokenEndpointAuthMethod
		}
	}

	// Step 4: Build authorization URL
	if callbackURL != "" {
		// Relay mode: save pending state and exit
		pending := &PendingAuth{
			Nonce:                   nonce,
			CodeVerifier:            codeVerifier,
			ClientID:                regClientID,
			ClientSecret:            regClientSecret,
			TokenURL:                authMeta.TokenEndpoint,
			Resource:                resource,
			RedirectURI:             redirectURI,
			ServerName:              name,
			CreatedAt:               time.Now().Unix(),
			TokenEndpointAuthMethod: authMethod,
		}

		if err := savePendingAuth(name, pending); err != nil {
			return fmt.Errorf("save pending auth: %w", err)
		}

		authURL := buildAuthorizationURL(authMeta, regClientID, redirectURI, codeChallenge, nonce, resource)
		if startURL != "" {
			authURL = buildStartHandoffURL(startURL, nonce, relayTimestamp, authURL)
		}

		// Auth URL is returned via JSON stdout; don't duplicate to stderr
		// where it could be captured in logs along with the nonce/state.
		return outputJSON(authOutput{
			AuthURL: authURL,
			Nonce:   nonce,
			Status:  "pending",
			Server:  name,
		})
	}

	// Local mode: hand off the already-listening socket
	return localOAuthFlow(localListener, name, authMeta, regClientID, regClientSecret, codeVerifier, codeChallenge, nonce, resource, authMethod)
}

// cmdAuthCallback handles `mcp auth-callback --nonce <nonce> --code <code>`.
// Called after receiving the OAuth callback in relay mode.
func cmdAuthCallback(args []string) error {
	cleanupExpiredPendingAuth()

	var nonce string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--nonce":
			if i+1 >= len(args) {
				return fmt.Errorf("--nonce requires a value")
			}
			i++
			nonce = args[i]
		default:
			return fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	code := os.Getenv("MCP_AUTH_CODE")

	if nonce == "" || code == "" {
		return fmt.Errorf("usage: mcp auth-callback --nonce <nonce> (set MCP_AUTH_CODE env var)")
	}

	// Find the pending auth by nonce
	pending, pendingPath, err := findPendingAuthByNonce(nonce)
	if err != nil {
		return fmt.Errorf("find pending auth: %w", err)
	}
	if pending == nil {
		return fmt.Errorf("no pending auth found for nonce %q", nonce)
	}

	// Exchange code for tokens
	logStderr("exchanging authorization code for tokens...")
	tokens, err := exchangeCode(pending, code)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}

	// Save tokens
	auth := tokensFromResponse(tokens, pending.ClientID, pending.ClientSecret, pending.TokenURL, pending.Resource, pending.TokenEndpointAuthMethod)

	if err := saveAuth(pending.ServerName, auth); err != nil {
		return fmt.Errorf("save auth: %w", err)
	}

	// Clean up pending state
	_ = os.Remove(pendingPath)

	logStderr("authorization complete for %q", pending.ServerName)
	return outputJSON(authOutput{
		Status: "complete",
		Server: pending.ServerName,
	})
}

// discoverOAuth performs RFC 9728 + 8414 discovery.
// Returns the resource URL and auth server metadata.
func discoverOAuth(mcpServerURL string) (string, *authServerMetadata, error) {
	client := authHTTPClient

	// Try RFC 9728: Protected Resource Metadata
	parsedURL, err := url.Parse(mcpServerURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse URL: %w", err)
	}

	resource := parsedURL.String()

	// Try /.well-known/oauth-protected-resource (with path suffix per RFC 8615,
	// then fall back to root if the server doesn't use path-based discovery).
	body, err := fetchWellKnown(client, buildWellKnownURL(parsedURL, "oauth-protected-resource"))
	if err != nil && parsedURL.Path != "" && parsedURL.Path != "/" {
		rootURL := *parsedURL
		rootURL.Path = ""
		body, err = fetchWellKnown(client, buildWellKnownURL(&rootURL, "oauth-protected-resource"))
	}
	if err != nil {
		return "", nil, fmt.Errorf("protected resource metadata: %w", err)
	}

	var prm protectedResourceMetadata
	if err := json.Unmarshal(body, &prm); err != nil {
		return "", nil, fmt.Errorf("parse protected resource metadata: %w", err)
	}

	if len(prm.AuthorizationServers) == 0 {
		return "", nil, fmt.Errorf("no authorization servers listed in protected resource metadata")
	}

	if prm.Resource != "" {
		resource = prm.Resource
	}

	authServerURL := prm.AuthorizationServers[0]

	if err := validateEndpointURL(authServerURL, "authorization server"); err != nil {
		return "", nil, err
	}

	// Fetch authorization server metadata (RFC 8414)
	asMeta, err := fetchAuthServerMetadata(client, authServerURL)
	if err != nil {
		return "", nil, err
	}

	return resource, asMeta, nil
}

func fetchAuthServerMetadata(client *http.Client, authServerURL string) (*authServerMetadata, error) {
	parsed, err := url.Parse(authServerURL)
	if err != nil {
		return nil, fmt.Errorf("parse auth server URL: %w", err)
	}

	body, err := fetchWellKnown(client, buildWellKnownURL(parsed, "oauth-authorization-server"))
	if err != nil && parsed.Path != "" && parsed.Path != "/" {
		rootURL := *parsed
		rootURL.Path = ""
		body, err = fetchWellKnown(client, buildWellKnownURL(&rootURL, "oauth-authorization-server"))
	}
	if err != nil {
		return nil, fmt.Errorf("auth server metadata: %w", err)
	}

	var meta authServerMetadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("parse auth server metadata: %w", err)
	}

	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return nil, fmt.Errorf("auth server metadata missing required endpoints")
	}

	if err := validateEndpointURL(meta.AuthorizationEndpoint, "authorization endpoint"); err != nil {
		return nil, err
	}
	if err := validateEndpointURL(meta.TokenEndpoint, "token endpoint"); err != nil {
		return nil, err
	}
	if meta.RegistrationEndpoint != "" {
		if err := validateEndpointURL(meta.RegistrationEndpoint, "registration endpoint"); err != nil {
			return nil, err
		}
	}

	// Validate S256 PKCE support if server advertises supported methods
	if len(meta.CodeChallengeMethodsSupported) > 0 {
		s256Found := false
		for _, m := range meta.CodeChallengeMethodsSupported {
			if m == "S256" {
				s256Found = true
				break
			}
		}
		if !s256Found {
			return nil, fmt.Errorf("auth server does not support S256 code challenge method (supports: %v)", meta.CodeChallengeMethodsSupported)
		}
	}

	return &meta, nil
}

// registerClient performs RFC 7591 Dynamic Client Registration.
func registerClient(meta *authServerMetadata, redirectURI, authMethod string) (*clientRegistrationResponse, error) {
	reqBody := clientRegistrationRequest{
		RedirectURIs:            []string{redirectURI},
		ClientName:              "mcp-cli",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: authMethod,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal registration request: %w", err)
	}

	client := authHTTPClient
	resp, err := client.Post(meta.RegistrationEndpoint, "application/json", strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("registration request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("registration failed (%d): %s", resp.StatusCode, string(body))
	}

	var reg clientRegistrationResponse
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, fmt.Errorf("parse registration response: %w", err)
	}

	if reg.ClientID == "" {
		return nil, fmt.Errorf("registration response missing client_id")
	}

	return &reg, nil
}

// buildAuthorizationURL constructs the OAuth authorization URL with PKCE.
func buildAuthorizationURL(meta *authServerMetadata, clientID, redirectURI, codeChallenge, state, resource string) string {
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}

	if resource != "" {
		params.Set("resource", resource)
	}

	if len(meta.ScopesSupported) > 0 {
		params.Set("scope", strings.Join(meta.ScopesSupported, " "))
	}

	return meta.AuthorizationEndpoint + "?" + params.Encode()
}

// doTokenRequest sends a POST to a token endpoint with the given form params,
// authenticating the client per the negotiated token endpoint auth method.
func doTokenRequest(tokenURL string, params url.Values, clientID, clientSecret, authMethod string) (*tokenResponse, error) {
	useBasic := false
	switch authMethod {
	case "none":
	case "client_secret_post":
		if clientSecret != "" {
			params.Set("client_secret", clientSecret)
		}
	default: // client_secret_basic, or empty (legacy tokens)
		useBasic = clientSecret != ""
	}

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if useBasic {
		req.SetBasicAuth(clientID, clientSecret)
	}

	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var tokenErr tokenErrorResponse
		if json.Unmarshal(body, &tokenErr) == nil && tokenErr.Error != "" {
			return nil, fmt.Errorf("%s: %s", tokenErr.Error, tokenErr.Description)
		}
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	return &tokens, nil
}

// tokensFromResponse converts a tokenResponse into an AuthTokens struct.
func tokensFromResponse(resp *tokenResponse, clientID, clientSecret, tokenURL, resource, authMethod string) *AuthTokens {
	auth := &AuthTokens{
		AccessToken:             resp.AccessToken,
		RefreshToken:            resp.RefreshToken,
		ClientID:                clientID,
		ClientSecret:            clientSecret,
		TokenURL:                tokenURL,
		Resource:                resource,
		TokenEndpointAuthMethod: authMethod,
	}
	if resp.ExpiresIn > 0 {
		auth.ExpiresAt = time.Now().Unix() + resp.ExpiresIn
	}
	return auth
}

// exchangeCode exchanges an authorization code for tokens.
func exchangeCode(pending *PendingAuth, code string) (*tokenResponse, error) {
	params := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {pending.RedirectURI},
		"code_verifier": {pending.CodeVerifier},
		"client_id":     {pending.ClientID},
	}

	if pending.Resource != "" {
		params.Set("resource", pending.Resource)
	}

	return doTokenRequest(pending.TokenURL, params, pending.ClientID, pending.ClientSecret, pending.TokenEndpointAuthMethod)
}

// refreshOAuthToken refreshes an expired OAuth token.
func refreshOAuthToken(tokens *AuthTokens) (*AuthTokens, error) {
	params := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tokens.RefreshToken},
		"client_id":     {tokens.ClientID},
	}

	if tokens.Resource != "" {
		params.Set("resource", tokens.Resource)
	}

	tokenResp, err := doTokenRequest(tokens.TokenURL, params, tokens.ClientID, tokens.ClientSecret, tokens.TokenEndpointAuthMethod)
	if err != nil {
		return nil, err
	}

	refreshed := tokensFromResponse(tokenResp, tokens.ClientID, tokens.ClientSecret, tokens.TokenURL, tokens.Resource, tokens.TokenEndpointAuthMethod)

	// Keep old refresh token if new one not provided
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tokens.RefreshToken
	}

	return refreshed, nil
}

// buildRelayRedirectURI constructs the relay OAuth callback URL.
// Appends nonce and timestamp as query parameters to the user's callback URL.
func buildRelayRedirectURI(callbackURL, nonce string) string {
	return buildRelayRedirectURIAt(callbackURL, nonce, time.Now().Unix())
}

// buildRelayRedirectURIAt is like buildRelayRedirectURI but uses the caller's
// timestamp so the same value can be cross-embedded in other URLs (e.g. the
// gateway start-handoff URL).
func buildRelayRedirectURIAt(callbackURL, nonce string, t int64) string {
	u, _ := url.Parse(callbackURL) // guaranteed valid by validateEndpointURL
	q := u.Query()
	q.Set("nonce", nonce)
	q.Set("t", fmt.Sprintf("%d", t))
	u.RawQuery = q.Encode()
	return u.String()
}

// buildStartHandoffURL wraps an upstream OAuth authorization URL in a
// gateway-controlled /start redirect. The browser hits the gateway first,
// which can set an HttpOnly cookie binding the click to (nonce, agentId)
// before 302'ing to the upstream. This defeats URL-leak replay attacks
// where an attacker reads the printed auth URL and authorizes in their
// own browser to bind their upstream tokens to the victim's agent.
//
// The CLI knows nothing about the start URL's structure beyond:
//   - it's an HTTPS endpoint chosen by the gateway,
//   - it accepts nonce, t, and destination query parameters.
//
// Any gateway that follows the same handoff convention can opt in by
// setting MCP_AUTH_START_URL.
//
// Param ORDER matters, even though /start parses them order-agnostically. This
// URL is commonly relayed to the user by an LLM agent that regenerates it
// token-by-token. The `destination` value is a ~500-char OAuth authorize URL
// whose own trailing param is `state=<nonce>` — a natural "end of URL" the model
// stops at, silently dropping anything after it. With nonce/t emitted after
// destination (as url.Values.Encode()'s alphabetical sort would do, since
// destination < nonce < t), the relay-required params get truncated and /start
// rejects the link with "Invalid start URL". Pin the short nonce/t params first
// and `destination` LAST so the model's stopping point is the real end of the
// URL and the required params survive.
func buildStartHandoffURL(startURL, nonce string, t int64, upstreamURL string) string {
	u, _ := url.Parse(startURL) // guaranteed valid by validateEndpointURL

	lead := u.Query() // any params already on the start URL (e.g. a tenant id)
	lead.Set("nonce", nonce)
	lead.Set("t", fmt.Sprintf("%d", t))

	tail := url.Values{"destination": {upstreamURL}}
	u.RawQuery = lead.Encode() + "&" + tail.Encode()
	return u.String()
}

// fetchWellKnown GETs a well-known URL and returns the body on 200, or an error.
func fetchWellKnown(client *http.Client, wellKnownURL string) ([]byte, error) {
	resp, err := client.Get(wellKnownURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", wellKnownURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%s returned %d", wellKnownURL, resp.StatusCode)
	}
	return readResponseBody(resp.Body)
}

// buildWellKnownURL constructs a well-known URL per RFC 8615.
func buildWellKnownURL(parsed *url.URL, wellKnownPath string) string {
	u := fmt.Sprintf("%s://%s/.well-known/%s", parsed.Scheme, parsed.Host, wellKnownPath)
	if parsed.Path != "" && parsed.Path != "/" {
		u += parsed.Path
	}
	return u
}

// PKCE helpers

func generateRandomBase64(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func generateCodeVerifier() string { return generateRandomBase64(32) }
func generateNonce() string        { return generateRandomBase64(16) }

func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
