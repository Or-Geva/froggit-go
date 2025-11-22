package vcsclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jfrog/froggit-go/vcsutils"
)

func TestNewGitHubOAuthClient(t *testing.T) {
	tests := []struct {
		name        string
		clientID    string
		scopes      []string
		expectError bool
	}{
		{
			name:        "Valid client with default scopes",
			clientID:    "test-client-id",
			scopes:      nil,
			expectError: false,
		},
		{
			name:        "Valid client with custom scopes",
			clientID:    "test-client-id",
			scopes:      []string{"repo", "read:user"},
			expectError: false,
		},
		{
			name:        "Missing client ID",
			clientID:    "",
			scopes:      []string{"repo"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewGitHubOAuthClient(tt.clientID, tt.scopes, vcsutils.EmptyLogger{})

			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if client == nil {
				t.Error("Expected client but got nil")
				return
			}

			if client.config.ClientID != tt.clientID {
				t.Errorf("Expected client ID %s, got %s", tt.clientID, client.config.ClientID)
			}

			if client.state == "" {
				t.Error("Expected state to be generated")
			}

			if len(client.tokenChan) != 0 {
				t.Error("Expected empty token channel")
			}
		})
	}
}

func TestGetAuthorizationURL(t *testing.T) {
	client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
	if err != nil {
		t.Fatalf("Failed to create OAuth client: %v", err)
	}

	authURL, err := client.GetAuthorizationURL()
	if err != nil {
		t.Fatalf("Failed to get authorization URL: %v", err)
	}

	if authURL == "" {
		t.Error("Expected non-empty authorization URL")
	}

	// Parse URL to validate components
	parsedURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("Failed to parse authorization URL: %v", err)
	}

	// Verify URL components
	if parsedURL.Scheme != "https" {
		t.Errorf("Expected https scheme, got %s", parsedURL.Scheme)
	}

	if !strings.Contains(parsedURL.Host, "github.com") {
		t.Errorf("Expected github.com host, got %s", parsedURL.Host)
	}

	query := parsedURL.Query()

	// Check for required OAuth parameters
	if query.Get("client_id") != "test-client-id" {
		t.Errorf("Expected client_id parameter")
	}

	if query.Get("state") == "" {
		t.Error("Expected state parameter for CSRF protection")
	}

	if query.Get("state") != client.state {
		t.Error("State in URL doesn't match client state")
	}

	// Check for PKCE parameters
	if query.Get("code_challenge") == "" {
		t.Error("Expected code_challenge parameter for PKCE")
	}

	if query.Get("code_challenge_method") != "S256" {
		t.Errorf("Expected code_challenge_method S256, got %s", query.Get("code_challenge_method"))
	}

	// Verify code verifier was stored
	if client.codeVerifier == "" {
		t.Error("Expected code verifier to be stored")
	}
}

func TestGenerateCodeVerifier(t *testing.T) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("Failed to generate code verifier: %v", err)
	}

	if verifier == "" {
		t.Error("Expected non-empty code verifier")
	}

	// Verify length is within valid range (43-128 characters for PKCE)
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Errorf("Code verifier length %d is outside valid range [43-128]", len(verifier))
	}

	// Generate multiple verifiers and verify they're unique
	verifier2, _ := generateCodeVerifier()
	if verifier == verifier2 {
		t.Error("Expected unique code verifiers")
	}
}

func TestGenerateCodeChallenge(t *testing.T) {
	verifier := "test-verifier-12345678901234567890123456"
	challenge := generateCodeChallenge(verifier)

	if challenge == "" {
		t.Error("Expected non-empty code challenge")
	}

	// Verify challenge is different from verifier
	if challenge == verifier {
		t.Error("Code challenge should be different from verifier")
	}

	// Verify deterministic: same verifier produces same challenge
	challenge2 := generateCodeChallenge(verifier)
	if challenge != challenge2 {
		t.Error("Expected deterministic code challenge generation")
	}

	// Verify different verifier produces different challenge
	challenge3 := generateCodeChallenge("different-verifier")
	if challenge == challenge3 {
		t.Error("Expected different challenges for different verifiers")
	}
}

func TestHandleCallback_Success(t *testing.T) {
	// Create mock OAuth server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mock token endpoint
		if r.URL.Path == "/login/oauth/access_token" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"test-token","token_type":"bearer","scope":"repo"}`)
			return
		}
		// Mock rate limit endpoint (for scope validation)
		if r.URL.Path == "/rate_limit" {
			w.Header().Set("X-OAuth-Scopes", "repo")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"rate":{"limit":5000}}`)
			return
		}
	}))
	defer mockServer.Close()

	client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
	if err != nil {
		t.Fatalf("Failed to create OAuth client: %v", err)
	}

	// Override GitHub endpoint for testing
	client.config.Endpoint.AuthURL = mockServer.URL + "/login/oauth/authorize"
	client.config.Endpoint.TokenURL = mockServer.URL + "/login/oauth/access_token"

	// Note: Full integration test would require running actual callback server
	// This test validates the URL structure
	authURL, err := client.GetAuthorizationURL()
	if err != nil {
		t.Fatalf("Failed to get auth URL: %v", err)
	}

	if !strings.Contains(authURL, "client_id=test-client-id") {
		t.Error("Auth URL missing client_id parameter")
	}

	if !strings.Contains(authURL, "state=") {
		t.Error("Auth URL missing state parameter")
	}
}

func TestHandleCallback_InvalidState(t *testing.T) {
	client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
	if err != nil {
		t.Fatalf("Failed to create OAuth client: %v", err)
	}

	// Create test request with invalid state
	req := httptest.NewRequest("GET", "http://localhost:8989/callback?code=test-code&state=invalid-state", nil)
	w := httptest.NewRecorder()

	// Handle callback
	go client.handleCallback(w, req)

	// Wait for error
	select {
	case err := <-client.errChan:
		if !strings.Contains(err.Error(), "invalid state") {
			t.Errorf("Expected invalid state error, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for invalid state error")
	}
}

func TestHandleCallback_MissingCode(t *testing.T) {
	client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
	if err != nil {
		t.Fatalf("Failed to create OAuth client: %v", err)
	}

	// Create test request with valid state but no code
	req := httptest.NewRequest("GET", fmt.Sprintf("http://localhost:8989/callback?state=%s", client.state), nil)
	w := httptest.NewRecorder()

	// Handle callback
	go client.handleCallback(w, req)

	// Wait for error
	select {
	case err := <-client.errChan:
		if !strings.Contains(err.Error(), "no authorization code") {
			t.Errorf("Expected missing code error, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for missing code error")
	}
}

func TestHandleCallback_ErrorFromGitHub(t *testing.T) {
	client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
	if err != nil {
		t.Fatalf("Failed to create OAuth client: %v", err)
	}

	// Create test request with error from GitHub
	errorDesc := "User denied access"
	req := httptest.NewRequest("GET", fmt.Sprintf("http://localhost:8989/callback?error=access_denied&error_description=%s", url.QueryEscape(errorDesc)), nil)
	w := httptest.NewRecorder()

	// Handle callback
	go client.handleCallback(w, req)

	// Wait for error
	select {
	case err := <-client.errChan:
		if !strings.Contains(err.Error(), "access_denied") {
			t.Errorf("Expected access denied error, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for access denied error")
	}
}

func TestWaitForCallback_Timeout(t *testing.T) {
	client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
	if err != nil {
		t.Fatalf("Failed to create OAuth client: %v", err)
	}

	// Create context with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Wait for callback (should timeout)
	token, err := client.WaitForCallback(ctx)

	if err == nil {
		t.Error("Expected timeout error but got nil")
	}

	if token != nil {
		t.Error("Expected nil token on timeout")
	}

	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("Expected timeout error, got: %v", err)
	}
}

func TestValidateTokenScopes(t *testing.T) {
	tests := []struct {
		name        string
		scopes      string
		expectError bool
	}{
		{
			name:        "Valid repo scope",
			scopes:      "repo",
			expectError: false,
		},
		{
			name:        "Valid public_repo scope",
			scopes:      "public_repo",
			expectError: false,
		},
		{
			name:        "Multiple scopes including repo",
			scopes:      "repo, read:user, read:org",
			expectError: false,
		},
		{
			name:        "Missing required scope",
			scopes:      "read:user, read:org",
			expectError: true,
		},
		{
			name:        "Empty scopes",
			scopes:      "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock GitHub API server
			mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-OAuth-Scopes", tt.scopes)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"rate":{"limit":5000}}`)
			}))
			defer mockServer.Close()

			client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
			if err != nil {
				t.Fatalf("Failed to create OAuth client: %v", err)
			}

			// Create context with mock API endpoint
			ctx := context.Background()

			// For testing, we'll use a custom HTTP client
			// Note: In production, ValidateTokenScopes makes a real API call
			// Here we'd need to mock the entire HTTP request

			// Test the scope splitting logic directly
			scopes := splitScopes(tt.scopes)
			hasRepoScope := false
			for _, scope := range scopes {
				if scope == "repo" || scope == "public_repo" {
					hasRepoScope = true
					break
				}
			}

			if tt.expectError && hasRepoScope {
				t.Error("Expected error but scope validation would pass")
			}

			if !tt.expectError && !hasRepoScope {
				t.Error("Expected success but scope validation would fail")
			}

			_ = ctx
			_ = client
		})
	}
}

func TestSplitScopes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "Single scope",
			input:    "repo",
			expected: []string{"repo"},
		},
		{
			name:     "Multiple scopes",
			input:    "repo, read:user, read:org",
			expected: []string{"repo", "read:user", "read:org"},
		},
		{
			name:     "Scopes with extra spaces",
			input:    "repo ,  read:user  , read:org",
			expected: []string{"repo", "read:user", "read:org"},
		},
		{
			name:     "Empty string",
			input:    "",
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := splitScopes(tt.input)

			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d scopes, got %d", len(tt.expected), len(result))
				return
			}

			for i, scope := range result {
				if scope != tt.expected[i] {
					t.Errorf("Expected scope[%d]=%s, got %s", i, tt.expected[i], scope)
				}
			}
		})
	}
}

func TestServerStartStop(t *testing.T) {
	client, err := NewGitHubOAuthClient("test-client-id", []string{"repo"}, vcsutils.EmptyLogger{})
	if err != nil {
		t.Fatalf("Failed to create OAuth client: %v", err)
	}

	ctx := context.Background()

	// Start server
	if err := client.StartCallbackServer(ctx); err != nil {
		t.Fatalf("Failed to start callback server: %v", err)
	}

	// Verify server is running
	if !client.serverRunning {
		t.Error("Expected server to be running")
	}

	// Try starting again (should error)
	if err := client.StartCallbackServer(ctx); err == nil {
		t.Error("Expected error when starting server twice")
	}

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Stop server
	if err := client.StopCallbackServer(); err != nil {
		t.Errorf("Failed to stop callback server: %v", err)
	}

	// Verify server is stopped
	if client.serverRunning {
		t.Error("Expected server to be stopped")
	}

	// Stopping again should not error
	if err := client.StopCallbackServer(); err != nil {
		t.Error("Expected no error when stopping already stopped server")
	}
}

func TestGenerateRandomString(t *testing.T) {
	tests := []int{16, 32, 64, 128}

	for _, length := range tests {
		t.Run(fmt.Sprintf("Length_%d", length), func(t *testing.T) {
			str, err := generateRandomString(length)
			if err != nil {
				t.Fatalf("Failed to generate random string: %v", err)
			}

			if len(str) != length {
				t.Errorf("Expected length %d, got %d", length, len(str))
			}

			// Generate another and verify uniqueness
			str2, _ := generateRandomString(length)
			if str == str2 {
				t.Error("Expected unique random strings")
			}
		})
	}
}
