package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	healthTokenFile   = "token_health.json"
	calendarTokenFile = "token_calendar.json"
	callbackURL       = "http://localhost:8080/callback"

	// Optional development fallbacks. Prefer CLIENT_ID and CLIENT_SECRET.
	defaultClientID     = ""
	defaultClientSecret = ""
)

// The Google Health API rejects any token that also carries scopes of other
// APIs (403 DISALLOWED_OAUTH_SCOPES), so Health and Calendar each get their
// own consent and their own token file.
var (
	googleHealthScopes = []string{
		"https://www.googleapis.com/auth/googlehealth.sleep.readonly",
		"https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly",
		"https://www.googleapis.com/auth/googlehealth.activity_and_fitness.readonly",
	}
	googleCalendarScopes = []string{"https://www.googleapis.com/auth/calendar"}
)

func googleOAuthConfig(scopes ...string) (*oauth2.Config, error) {
	clientID := firstNonEmpty(os.Getenv("CLIENT_ID"), defaultClientID)
	clientSecret := firstNonEmpty(os.Getenv("CLIENT_SECRET"), defaultClientSecret)
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("CLIENT_ID and CLIENT_SECRET must be configured for Google sync")
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  callbackURL,
		Scopes:       scopes,
		Endpoint:     oauth2.Endpoint{AuthURL: googleAuthURL, TokenURL: googleTokenURL},
	}, nil
}

// googleClient loads the token stored in tokenPath, or completes the local
// callback OAuth flow and saves it there. oauth2.Config.Client automatically
// refreshes an expired token when its refresh token is available.
//
// An expired token is refreshed up front, so a token Google will never accept
// again (revoked access, deleted OAuth client...) is discarded and replaced
// by a fresh authorization instead of failing every API call.
func googleClient(ctx context.Context, config *oauth2.Config, tokenPath string) (*http.Client, error) {
	token, authorizedAt, err := loadToken(tokenPath)
	if err == nil && !token.Valid() {
		refreshed, refreshErr := config.TokenSource(ctx, token).Token()
		switch {
		case refreshErr == nil:
			token = refreshed
			if err := saveToken(tokenPath, token, authorizedAt); err != nil {
				return nil, err
			}
		case isPermanentTokenError(refreshErr):
			fmt.Printf("Stored Google token in %s is no longer valid (%v); authorizing again.\n", tokenPath, refreshErr)
			if err := os.Remove(tokenPath); err != nil {
				return nil, fmt.Errorf("remove invalid token %s: %w", tokenPath, err)
			}
			token, err = nil, os.ErrNotExist
		default:
			// Likely a network problem - keep the token for the next run.
			return nil, fmt.Errorf("refresh Google token: %w", refreshErr)
		}
	}
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if !interactiveAuth {
			return nil, fmt.Errorf("%w: no valid Google token in %s; run the agent once without -json to authorize in the browser",
				errNeedsAuthorization, tokenPath)
		}
		token, err = authorizeGoogle(ctx, config)
		if err != nil {
			return nil, err
		}
		if err := saveToken(tokenPath, token, time.Now()); err != nil {
			return nil, err
		}
	}
	return config.Client(ctx, token), nil
}

// errNeedsAuthorization means the user must approve access in the browser
// again - the token is missing, expired or revoked.
var errNeedsAuthorization = errors.New("Google authorization needed")

// isPermanentTokenError reports whether Google refused a token refresh in a
// way that retrying with the same token can never fix.
func isPermanentTokenError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		return false
	}
	switch retrieveErr.ErrorCode {
	case "invalid_grant", "invalid_client", "deleted_client", "unauthorized_client":
		return true
	}
	return false
}

func authorizeGoogle(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	state, err := oauthState()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback server: %w", err)
	}
	defer listener.Close()

	codeCh := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != state || r.URL.Query().Get("error") != "" {
			http.Error(w, "Authorization was denied or did not match this request.", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Authorization code missing.", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Authorization complete. You can close this browser window and return to the Fitness Agent.")
		select {
		case codeCh <- code:
		default:
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())

	// Request a refresh token and force Google's consent screen on every new
	// authorization flow, so newly added Health/Calendar scopes are granted.
	authURL := config.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
	fmt.Printf("Open this URL to authorize Google (%s):\n%s\n", strings.Join(config.Scopes, ", "), authURL)
	if err := openBrowser(authURL); err != nil {
		fmt.Printf("Could not open a browser automatically: %v\n", err)
	}

	select {
	case code := <-codeCh:
		token, err := config.Exchange(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("exchange OAuth authorization code: %w", err)
		}
		return token, nil
	case <-time.After(5 * time.Minute):
		return nil, errors.New("timed out waiting for Google authorization callback")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// storedToken is the token file: the OAuth token plus when the user approved
// it in the browser. While the OAuth app is in "Testing", Google revokes the
// refresh token 7 days after that, so the Telegram bot reads authorized_at to
// remind the user to re-authorize in time. Refreshing keeps authorized_at.
type storedToken struct {
	*oauth2.Token
	AuthorizedAt *time.Time `json:"authorized_at,omitempty"`
}

// loadToken returns the token in filename and when it was authorized (zero
// if the file predates that field).
func loadToken(filename string) (*oauth2.Token, time.Time, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer file.Close()
	stored := storedToken{Token: &oauth2.Token{}}
	if err := json.NewDecoder(file).Decode(&stored); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode %s: %w", filename, err)
	}
	var authorizedAt time.Time
	if stored.AuthorizedAt != nil {
		authorizedAt = *stored.AuthorizedAt
	}
	return stored.Token, authorizedAt, nil
}

func saveToken(filename string, token *oauth2.Token, authorizedAt time.Time) error {
	stored := storedToken{Token: token}
	if !authorizedAt.IsZero() {
		stored.AuthorizedAt = &authorizedAt
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		return fmt.Errorf("save %s: %w", filename, err)
	}
	return nil
}

func oauthState() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
