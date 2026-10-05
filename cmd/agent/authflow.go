package main

// ---------------------------------------------------------------------
// Re-authorization without a browser on this machine (-auth).
//
// The normal flow (authorizeGoogle) opens a browser here and catches
// Google's redirect on localhost:8080 - impossible on a server. Instead:
//
//  1. -auth health|calendar        prints an authorization link and saves
//                                   its one-time state in oauthPendingFile;
//  2. the user approves on any device. Google then redirects to
//     http://localhost:8080/callback?state=...&code=..., which fails to
//     load there - and the user copies that address from the address bar;
//  3. -auth health|calendar -auth-code '<that address>'
//                                   checks the state, exchanges the code
//                                   and saves the token.
//
// The Telegram bot drives this so tokens can be renewed from the phone.
// ---------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	// oauthPendingFile holds the state of authorization links not completed
	// yet, per target. It lives next to the tokens and is in .gitignore.
	oauthPendingFile = "oauth_pending.json"
	// A link older than this is refused; Google's codes expire quickly too.
	oauthPendingMaxAge = 15 * time.Minute
)

// authTarget is one Google token the agent uses.
type authTarget struct {
	scopes    []string
	tokenFile string
}

var authTargets = map[string]authTarget{
	"health":   {googleHealthScopes, healthTokenFile},
	"calendar": {googleCalendarScopes, calendarTokenFile},
}

type pendingAuth struct {
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// authResult is the outcome of -auth: a link to open, or a saved token.
type authResult struct {
	Target       string
	URL          string    // set by step 1
	AuthorizedAt time.Time // set when a token was saved (step 3)
	Err          error
}

func runAuthFlow(target, pasted string, now time.Time) authResult {
	result := authResult{Target: target}
	t, ok := authTargets[target]
	if !ok {
		result.Err = fmt.Errorf("unknown -auth target %q (use health or calendar)", target)
		return result
	}
	config, err := googleOAuthConfig(t.scopes...)
	if err != nil {
		result.Err = err
		return result
	}
	if strings.TrimSpace(pasted) == "" {
		result.URL, result.Err = startAuth(config, target, now)
		return result
	}
	result.Err = completeAuth(context.Background(), config, target, t.tokenFile, pasted, now)
	if result.Err == nil {
		result.AuthorizedAt = now
	}
	return result
}

// startAuth returns a new authorization link for target and remembers its
// state, replacing any earlier unfinished link for the same target.
func startAuth(config *oauth2.Config, target string, now time.Time) (string, error) {
	state, err := oauthState()
	if err != nil {
		return "", err
	}
	pending, err := loadPendingAuth()
	if err != nil {
		return "", err
	}
	pending[target] = pendingAuth{State: state, CreatedAt: now}
	if err := savePendingAuth(pending); err != nil {
		return "", err
	}
	return config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce), nil
}

// completeAuth takes the address Google redirected to (or just its query),
// checks that its state belongs to a recent link for target, and exchanges
// the code for a token saved in tokenFile.
func completeAuth(ctx context.Context, config *oauth2.Config, target, tokenFile, pasted string, now time.Time) error {
	query, err := parseRedirect(pasted)
	if err != nil {
		return err
	}
	if denied := query.Get("error"); denied != "" {
		return fmt.Errorf("Google reported %q - the access wasn't approved", denied)
	}

	pending, err := loadPendingAuth()
	if err != nil {
		return err
	}
	want, ok := pending[target]
	switch {
	case !ok:
		return fmt.Errorf("no authorization link was created for %s - ask for a new one", target)
	case query.Get("state") != want.State:
		return errors.New("this address belongs to a different (or older) authorization link - use the latest link")
	case now.Sub(want.CreatedAt) > oauthPendingMaxAge:
		return errors.New("the authorization link expired - ask for a new one")
	}

	token, err := config.Exchange(ctx, query.Get("code"))
	if err != nil {
		return fmt.Errorf("exchange the authorization code: %w", err)
	}
	if err := saveToken(tokenFile, token, now); err != nil {
		return err
	}
	delete(pending, target)
	if err := savePendingAuth(pending); err != nil {
		fmt.Printf("Warning: %v\n", err) // the token is saved; a stale entry is harmless
	}
	return nil
}

// parseRedirect accepts the full redirect address, with or without the
// scheme, and returns its query. It needs a code (or an error) in it.
func parseRedirect(pasted string) (url.Values, error) {
	s := strings.TrimSpace(pasted)
	if i := strings.Index(s, "?"); i >= 0 {
		s = s[i+1:]
	}
	query, err := url.ParseQuery(s)
	if err != nil || (query.Get("code") == "" && query.Get("error") == "") {
		return nil, errors.New("that doesn't look like the address Google redirected to - copy the full address that starts with http://localhost:8080/callback")
	}
	return query, nil
}

func loadPendingAuth() (map[string]pendingAuth, error) {
	pending := map[string]pendingAuth{}
	data, err := os.ReadFile(oauthPendingFile)
	if errors.Is(err, os.ErrNotExist) {
		return pending, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", oauthPendingFile, err)
	}
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("decode %s: %w", oauthPendingFile, err)
	}
	return pending, nil
}

func savePendingAuth(pending map[string]pendingAuth) error {
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pending authorizations: %w", err)
	}
	if err := os.WriteFile(oauthPendingFile, data, 0o600); err != nil {
		return fmt.Errorf("save %s: %w", oauthPendingFile, err)
	}
	return nil
}

func printAuthResult(r authResult) {
	switch {
	case r.Err != nil:
		fmt.Printf("Authorization for %s failed: %v\n", r.Target, r.Err)
	case r.URL != "":
		fmt.Printf("Open this link and approve access (%s):\n%s\n\n", r.Target, r.URL)
		fmt.Println("The browser then lands on a page that fails to load (localhost). Copy that page's full address and run:")
		fmt.Printf("  fitness-agent -auth %s -auth-code \"<the address>\"\n", r.Target)
	default:
		fmt.Printf("Authorized %s; the token was saved to %s.\n", r.Target, authTargets[r.Target].tokenFile)
	}
}

type authJSON struct {
	OK           bool       `json:"ok"`
	Target       string     `json:"target"`
	URL          string     `json:"url,omitempty"`
	AuthorizedAt *time.Time `json:"authorized_at,omitempty"`
	Error        string     `json:"error,omitempty"`
}

func writeAuthJSON(w io.Writer, r authResult) error {
	out := authJSON{OK: r.Err == nil, Target: r.Target, URL: r.URL}
	if r.Err != nil {
		out.Error = r.Err.Error()
	}
	if !r.AuthorizedAt.IsZero() {
		out.AuthorizedAt = ptr(r.AuthorizedAt)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
