package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOAuthCallbackHandler_Error(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	handler := oauthCallbackHandler("nonce", codeCh, errCh)

	req := httptest.NewRequest("GET", "/callback?error=access_denied&error_description=nope", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Authorization denied") {
		t.Errorf("expected denial HTML, got %q", w.Body.String())
	}

	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "access_denied") {
			t.Errorf("expected access_denied in error, got %v", err)
		}
	default:
		t.Fatal("expected error on errCh")
	}
}

func TestOAuthCallbackHandler_MissingCode(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	handler := oauthCallbackHandler("nonce", codeCh, errCh)

	req := httptest.NewRequest("GET", "/callback?state=nonce", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if !strings.Contains(w.Body.String(), "Missing authorization code") {
		t.Errorf("expected missing code HTML, got %q", w.Body.String())
	}

	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "missing code") {
			t.Errorf("expected missing code error, got %v", err)
		}
	default:
		t.Fatal("expected error on errCh")
	}
}

func TestOAuthCallbackHandler_StateMismatch(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	handler := oauthCallbackHandler("correct-nonce", codeCh, errCh)

	req := httptest.NewRequest("GET", "/callback?code=abc&state=wrong", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if !strings.Contains(w.Body.String(), "Invalid state") {
		t.Errorf("expected state mismatch HTML, got %q", w.Body.String())
	}

	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "state mismatch") {
			t.Errorf("expected state mismatch error, got %v", err)
		}
	default:
		t.Fatal("expected error on errCh")
	}
}

func TestOAuthCallbackHandler_Success(t *testing.T) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	handler := oauthCallbackHandler("my-nonce", codeCh, errCh)

	req := httptest.NewRequest("GET", "/callback?code=auth-code-123&state=my-nonce", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if !strings.Contains(w.Body.String(), "Connected!") {
		t.Errorf("expected success HTML, got %q", w.Body.String())
	}

	select {
	case code := <-codeCh:
		if code != "auth-code-123" {
			t.Errorf("expected code 'auth-code-123', got %q", code)
		}
	default:
		t.Fatal("expected code on codeCh")
	}
}
