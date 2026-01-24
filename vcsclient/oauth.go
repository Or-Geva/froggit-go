package vcsclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jfrog/froggit-go/vcsutils"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

const (
	// GitHubOAuthClientID is the OAuth application client ID for Qodo Desktop
	// This is public and safe to commit (not a secret)
	GitHubOAuthClientID = "Ov23liejYjyvSkk1YDd7"

	// OAuthCallbackPort is the local port for OAuth callback server
	OAuthCallbackPort = 8989

	// OAuthTimeout is the maximum time to wait for OAuth callback
	OAuthTimeout = 2 * time.Minute

	// Default OAuth scopes for read-only PR access
	defaultOAuthScope = "repo"
)

// GitHubOAuthClient handles GitHub OAuth 2.0 authentication flow for desktop applications.
// It implements a localhost callback server to receive the authorization code.
type GitHubOAuthClient struct {
	config         *oauth2.Config
	callbackServer *http.Server
	tokenChan      chan *oauth2.Token
	errChan        chan error
	state          string
	codeVerifier   string // For PKCE
	logger         vcsutils.Log
	mu             sync.Mutex
	serverRunning  bool
}

// NewGitHubOAuthClient creates a new OAuth client for GitHub authentication.
// clientID: OAuth application client ID (use GitHubOAuthClientID constant)
// clientSecret: OAuth application client secret (from GitHub OAuth app settings)
// scopes: OAuth scopes to request (default: ["repo"] for read-only access)
// logger: Logger for debugging (use vcsutils.EmptyLogger{} if not needed)
func NewGitHubOAuthClient(clientID, clientSecret string, scopes []string, logger vcsutils.Log) (*GitHubOAuthClient, error) {
	fmt.Printf("[OAuth] NewGitHubOAuthClient called\n")
	fmt.Printf("[OAuth] Client ID: %s\n", clientID)
	fmt.Printf("[OAuth] Client Secret provided: %v (length: %d)\n", clientSecret != "", len(clientSecret))
	fmt.Printf("[OAuth] Requested scopes: %v\n", scopes)

	if clientID == "" {
		return nil, fmt.Errorf("clientID is required")
	}

	if clientSecret == "" {
		return nil, fmt.Errorf("clientSecret is required for GitHub OAuth")
	}

	if len(scopes) == 0 {
		scopes = []string{defaultOAuthScope}
	}

	if logger == nil {
		logger = vcsutils.EmptyLogger{}
	}

	// Generate CSRF protection state
	state, err := generateRandomString(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate state: %w", err)
	}
	fmt.Printf("[OAuth] Generated state: %s\n", state)

	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/callback", OAuthCallbackPort)
	fmt.Printf("[OAuth] Redirect URL: %s\n", redirectURL)

	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       scopes,
		Endpoint:     github.Endpoint,
		RedirectURL:  redirectURL,
	}

	fmt.Printf("[OAuth] OAuth2 config created successfully\n")
	return &GitHubOAuthClient{
		config:    config,
		tokenChan: make(chan *oauth2.Token, 1),
		errChan:   make(chan error, 1),
		state:     state,
		logger:    logger,
	}, nil
}

// GetAuthorizationURL generates the GitHub authorization URL with PKCE protection.
// Returns the URL that should be opened in the user's browser.
func (c *GitHubOAuthClient) GetAuthorizationURL() (string, error) {
	fmt.Printf("[OAuth] GetAuthorizationURL called\n")

	// Generate PKCE code verifier and challenge
	verifier, err := generateCodeVerifier()
	if err != nil {
		fmt.Printf("[OAuth] ERROR: Failed to generate code verifier: %v\n", err)
		return "", fmt.Errorf("failed to generate code verifier: %w", err)
	}
	c.codeVerifier = verifier
	challenge := generateCodeChallenge(verifier)

	fmt.Printf("[OAuth] Generated PKCE verifier (length: %d)\n", len(verifier))
	fmt.Printf("[OAuth] Generated PKCE challenge: %s\n", challenge)

	c.logger.Info("Generated OAuth authorization URL with PKCE")

	// Build authorization URL with PKCE parameters
	url := c.config.AuthCodeURL(c.state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"))

	fmt.Printf("[OAuth] Authorization URL generated: %s\n", url)
	return url, nil
}

// StartCallbackServer starts a local HTTP server to handle the OAuth callback.
// This server listens on localhost:8989 for the redirect from GitHub.
// The server automatically shuts down after receiving a callback or on error.
func (c *GitHubOAuthClient) StartCallbackServer(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.serverRunning {
		return fmt.Errorf("callback server already running")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", c.handleCallback)

	c.callbackServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", OAuthCallbackPort),
		Handler: mux,
	}

	c.serverRunning = true

	// Start server in goroutine
	go func() {
		c.logger.Info("Starting OAuth callback server on port %d", OAuthCallbackPort)
		if err := c.callbackServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			c.logger.Error("Callback server error: %v", err)
			c.errChan <- fmt.Errorf("callback server error: %w", err)
		}
	}()

	return nil
}

// handleCallback processes the OAuth callback from GitHub.
// It validates the state parameter, exchanges the authorization code for a token,
// validates token scopes, and sends the token through the channel.
func (c *GitHubOAuthClient) handleCallback(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[OAuth] ========== CALLBACK RECEIVED ==========\n")
	fmt.Printf("[OAuth] Request URL: %s\n", r.URL.String())
	fmt.Printf("[OAuth] Request Method: %s\n", r.Method)
	fmt.Printf("[OAuth] Query Parameters: %v\n", r.URL.Query())

	c.logger.Info("Received OAuth callback")

	// Check for error from GitHub
	if errCode := r.URL.Query().Get("error"); errCode != "" {
		errDesc := r.URL.Query().Get("error_description")
		fmt.Printf("[OAuth] ERROR from GitHub: %s - %s\n", errCode, errDesc)
		c.logger.Error("OAuth error: %s - %s", errCode, errDesc)

		c.errChan <- fmt.Errorf("OAuth authorization failed: %s - %s", errCode, errDesc)
		c.renderCallbackPage(w, false, "Authorization failed: "+errDesc)
		return
	}

	// Validate state parameter (CSRF protection)
	receivedState := r.URL.Query().Get("state")
	fmt.Printf("[OAuth] Received state: %s\n", receivedState)
	fmt.Printf("[OAuth] Expected state: %s\n", c.state)
	fmt.Printf("[OAuth] State matches: %v\n", receivedState == c.state)

	if receivedState != c.state {
		fmt.Printf("[OAuth] ERROR: State parameter mismatch! (CSRF attack?)\n")
		c.logger.Error("Invalid state parameter (CSRF attack?)")
		c.errChan <- fmt.Errorf("invalid state parameter - possible CSRF attack")
		c.renderCallbackPage(w, false, "Security validation failed")
		return
	}

	// Get authorization code
	code := r.URL.Query().Get("code")
	fmt.Printf("[OAuth] Authorization code received (length: %d)\n", len(code))
	if code == "" {
		fmt.Printf("[OAuth] ERROR: No authorization code in callback\n")
		c.logger.Error("No authorization code received")
		c.errChan <- fmt.Errorf("no authorization code received")
		c.renderCallbackPage(w, false, "No authorization code received")
		return
	}

	// Exchange code for token with PKCE verifier
	fmt.Printf("[OAuth] Exchanging code for token...\n")
	fmt.Printf("[OAuth] Using code verifier (length: %d)\n", len(c.codeVerifier))
	fmt.Printf("[OAuth] Client ID: %s\n", c.config.ClientID)
	fmt.Printf("[OAuth] Client Secret set: %v\n", c.config.ClientSecret != "" && c.config.ClientSecret != "YOUR_CLIENT_SECRET_HERE")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	token, err := c.config.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", c.codeVerifier))
	if err != nil {
		fmt.Printf("[OAuth] ERROR: Token exchange failed: %v\n", err)
		fmt.Printf("[OAuth] Error type: %T\n", err)
		c.logger.Error("Failed to exchange code for token: %v", err)
		c.errChan <- fmt.Errorf("failed to exchange authorization code: %w", err)
		c.renderCallbackPage(w, false, "Token exchange failed: "+err.Error())
		return
	}

	fmt.Printf("[OAuth] SUCCESS: Token received!\n")
	fmt.Printf("[OAuth] Token type: %s\n", token.TokenType)
	fmt.Printf("[OAuth] Access token length: %d\n", len(token.AccessToken))
	c.logger.Info("Successfully exchanged code for token")

	// Validate token scopes
	fmt.Printf("[OAuth] Validating token scopes...\n")
	if err := c.ValidateTokenScopes(ctx, token.AccessToken); err != nil {
		fmt.Printf("[OAuth] ERROR: Token scope validation failed: %v\n", err)
		c.logger.Error("Token scope validation failed: %v", err)
		c.errChan <- fmt.Errorf("token scope validation failed: %w", err)
		c.renderCallbackPage(w, false, "Token has insufficient permissions")
		return
	}

	fmt.Printf("[OAuth] Token scope validation successful\n")
	c.logger.Info("Token scope validation successful")

	// Send token through channel
	fmt.Printf("[OAuth] Sending token through channel...\n")
	c.tokenChan <- token

	// Render success page
	c.renderCallbackPage(w, true, "")

	fmt.Printf("[OAuth] ========== CALLBACK COMPLETE ==========\n")

	// Schedule server shutdown after a brief delay (allow page to render)
	go func() {
		time.Sleep(1 * time.Second)
		_ = c.StopCallbackServer()
	}()
}

// renderCallbackPage renders a simple HTML page to show OAuth result to user.
func (c *GitHubOAuthClient) renderCallbackPage(w http.ResponseWriter, success bool, errorMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if success {
		w.WriteHeader(http.StatusOK)
		html := `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Authorization Successful</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Arial, sans-serif;
               display: flex; align-items: center; justify-content: center; height: 100vh;
               margin: 0; background: linear-gradient(135deg, #667eea 0%%, #764ba2 100%%); }
        .container { background: white; padding: 40px; border-radius: 10px; box-shadow: 0 4px 6px rgba(0,0,0,0.1);
                     text-align: center; max-width: 400px; }
        h1 { color: #10b981; margin: 0 0 20px 0; }
        p { color: #6b7280; margin: 0; }
        .checkmark { font-size: 48px; color: #10b981; margin-bottom: 20px; }
    </style>
</head>
<body>
    <div class="container">
        <div class="checkmark">✓</div>
        <h1>Authorization Successful!</h1>
        <p>You can close this window and return to the app.</p>
    </div>
</body>
</html>`
		fmt.Fprint(w, html)
	} else {
		w.WriteHeader(http.StatusBadRequest)
		// Build HTML with escaped % in CSS, then use fmt.Sprintf only for errorMsg
		htmlTemplate := `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Authorization Failed</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Arial, sans-serif;
               display: flex; align-items: center; justify-content: center; height: 100vh;
               margin: 0; background: linear-gradient(135deg, #667eea 0%%, #764ba2 100%%); }
        .container { background: white; padding: 40px; border-radius: 10px; box-shadow: 0 4px 6px rgba(0,0,0,0.1);
                     text-align: center; max-width: 400px; }
        h1 { color: #ef4444; margin: 0 0 20px 0; }
        p { color: #6b7280; margin: 0; }
        .error-icon { font-size: 48px; color: #ef4444; margin-bottom: 20px; }
    </style>
</head>
<body>
    <div class="container">
        <div class="error-icon">✗</div>
        <h1>Authorization Failed</h1>
        <p>%s</p>
        <p style="margin-top: 20px;">Please close this window and try again.</p>
    </div>
</body>
</html>`
		html := fmt.Sprintf(htmlTemplate, errorMsg)
		fmt.Fprint(w, html)
	}
}

// WaitForCallback waits for the OAuth callback to complete or timeout.
// Returns the access token on success, or an error on failure/timeout.
// This method blocks until either a token is received, an error occurs, or the timeout expires.
func (c *GitHubOAuthClient) WaitForCallback(ctx context.Context) (*oauth2.Token, error) {
	// Create timeout context if not provided
	timeoutCtx, cancel := context.WithTimeout(ctx, OAuthTimeout)
	defer cancel()

	c.logger.Info("Waiting for OAuth callback (timeout: %v)", OAuthTimeout)

	select {
	case token := <-c.tokenChan:
		c.logger.Info("OAuth callback received successfully")
		return token, nil
	case err := <-c.errChan:
		c.logger.Error("OAuth callback error: %v", err)
		return nil, err
	case <-timeoutCtx.Done():
		c.logger.Error("OAuth callback timeout")
		_ = c.StopCallbackServer()
		return nil, fmt.Errorf("OAuth authorization timeout after %v", OAuthTimeout)
	}
}

// StopCallbackServer gracefully shuts down the OAuth callback server.
func (c *GitHubOAuthClient) StopCallbackServer() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.serverRunning || c.callbackServer == nil {
		return nil
	}

	c.logger.Info("Stopping OAuth callback server")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.callbackServer.Shutdown(ctx); err != nil {
		c.logger.Error("Error shutting down callback server: %v", err)
		return fmt.Errorf("failed to shutdown callback server: %w", err)
	}

	c.serverRunning = false
	c.logger.Info("OAuth callback server stopped")
	return nil
}

// ValidateTokenScopes verifies that the OAuth token has the required scopes.
// For read-only PR access, we require the "repo" scope.
func (c *GitHubOAuthClient) ValidateTokenScopes(ctx context.Context, token string) error {
	// Create a GitHub client with the token
	httpClient := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))

	// Make a simple API call to check scopes via response headers
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/rate_limit", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to validate token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token validation failed with status: %d", resp.StatusCode)
	}

	// Check X-OAuth-Scopes header to verify scopes
	scopesHeader := resp.Header.Get("X-OAuth-Scopes")
	c.logger.Debug("Token scopes: %s", scopesHeader)

	// Verify token has required scope
	// Note: We check for "repo" which provides read access to repositories and PRs
	// For more restrictive access, could check for "public_repo" (public repos only)
	hasRepoScope := false
	for _, scope := range splitScopes(scopesHeader) {
		if scope == "repo" || scope == "public_repo" {
			hasRepoScope = true
			break
		}
	}

	if !hasRepoScope {
		return fmt.Errorf("token missing required 'repo' scope, has: %s", scopesHeader)
	}

	c.logger.Info("Token scope validation successful: %s", scopesHeader)
	return nil
}

// Helper functions

// generateRandomString generates a cryptographically secure random string of given length.
func generateRandomString(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes)[:length], nil
}

// generateCodeVerifier generates a PKCE code verifier (43-128 character random string).
func generateCodeVerifier() (string, error) {
	// Generate 32 random bytes (will be base64url encoded to ~43 chars)
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

// generateCodeChallenge generates a PKCE code challenge from a verifier.
// Uses SHA256 hash and base64url encoding as per RFC 7636.
func generateCodeChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// splitScopes splits a comma-separated scope string into individual scopes.
func splitScopes(scopesHeader string) []string {
	if scopesHeader == "" {
		return []string{}
	}

	scopes := []string{}
	for _, scope := range splitByComma(scopesHeader) {
		trimmed := trimSpace(scope)
		if trimmed != "" {
			scopes = append(scopes, trimmed)
		}
	}
	return scopes
}

// Helper to split string by comma without importing strings package
func splitByComma(s string) []string {
	if s == "" {
		return []string{}
	}

	result := []string{}
	current := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			result = append(result, current)
			current = ""
		} else {
			current += string(s[i])
		}
	}
	result = append(result, current)
	return result
}

// Helper to trim whitespace without importing strings package
func trimSpace(s string) string {
	start := 0
	end := len(s)

	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}

	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}

	return s[start:end]
}
