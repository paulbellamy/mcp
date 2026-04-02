package main

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

// localOAuthFlow runs the full OAuth flow with a localhost callback server.
// Used when running locally (not in relay mode).
// The caller must provide an already-listening net.Listener so that the real
// port is known before OAuth client registration.
func localOAuthFlow(
	listener net.Listener,
	name string,
	authMeta *authServerMetadata,
	clientID, clientSecret string,
	codeVerifier, codeChallenge string,
	nonce, resource string,
) error {
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// Build authorization URL
	authURL := buildAuthorizationURL(authMeta, clientID, redirectURI, codeChallenge, nonce, resource)

	// Channel to receive the auth code
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", oauthCallbackHandler(nonce, codeCh, errCh))

	server := &http.Server{Handler: mux}
	go server.Serve(listener)

	// Open browser
	logStderr("opening browser for authorization...")
	logStderr("if the browser doesn't open, visit: %s", authURL)
	openBrowser(authURL)

	// Wait for callback or timeout
	select {
	case code := <-codeCh:
		server.Shutdown(context.Background())

		// Exchange code for tokens
		pending := &PendingAuth{
			Nonce:        nonce,
			CodeVerifier: codeVerifier,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			TokenURL:     authMeta.TokenEndpoint,
			Resource:     resource,
			RedirectURI:  redirectURI,
			ServerName:   name,
		}

		logStderr("exchanging authorization code for tokens...")
		tokens, err := exchangeCode(pending, code)
		if err != nil {
			return fmt.Errorf("token exchange failed: %w", err)
		}

		auth := tokensFromResponse(tokens, clientID, clientSecret, authMeta.TokenEndpoint, resource)

		if err := saveAuth(name, auth); err != nil {
			return fmt.Errorf("save auth: %w", err)
		}

		logStderr("authorization complete for %q", name)
		return outputJSON(authOutput{Status: "complete", Server: name})

	case err := <-errCh:
		server.Shutdown(context.Background())
		return err

	case <-time.After(2 * time.Minute):
		server.Shutdown(context.Background())
		return fmt.Errorf("authorization timed out after 2 minutes")
	}
}

// oauthCallbackHandler returns an HTTP handler that receives the OAuth callback.
func oauthCallbackHandler(nonce string, codeCh chan<- string, errCh chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			desc := r.URL.Query().Get("error_description")
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<html><body><h2>Authorization denied</h2><p>%s: %s</p><p>You can close this window.</p></body></html>", html.EscapeString(errParam), html.EscapeString(desc))
			errCh <- fmt.Errorf("authorization denied: %s — %s", errParam, desc)
			return
		}

		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")

		if code == "" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body><h2>Error</h2><p>Missing authorization code.</p></body></html>")
			errCh <- fmt.Errorf("callback missing code parameter")
			return
		}

		if state != nonce {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body><h2>Error</h2><p>Invalid state parameter.</p></body></html>")
			errCh <- fmt.Errorf("state mismatch")
			return
		}

		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><h2>Connected!</h2><p>Authorization successful. You can close this window.</p></body></html>")
		codeCh <- code
	}
}

// openBrowser opens a URL in the default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
}
