package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeTokenServer answers Google's token endpoint with a fixed token.
func fakeTokenServer(t *testing.T) *oauth2.Config {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != "good-code" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	return &oauth2.Config{
		ClientID: "id", ClientSecret: "secret", RedirectURL: callbackURL,
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: srv.URL},
	}
}

func TestAuthFlowLinkThenPastedAddress(t *testing.T) {
	t.Chdir(t.TempDir())
	config := fakeTokenServer(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	link, err := startAuth(config, "health", now)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	state := u.Query().Get("state")
	if state == "" || u.Query().Get("access_type") != "offline" {
		t.Fatalf("link should carry a state and ask for offline access: %s", link)
	}

	pasted := "http://localhost:8080/callback?state=" + url.QueryEscape(state) + "&code=good-code&scope=x"
	if err := completeAuth(context.Background(), config, "health", "token_health.json", pasted, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("completeAuth: %v", err)
	}
	token, authorizedAt, err := loadToken("token_health.json")
	if err != nil || token.RefreshToken != "r" || !authorizedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("saved token %+v at %v, err %v", token, authorizedAt, err)
	}

	// The link is single use.
	err = completeAuth(context.Background(), config, "health", "token_health.json", pasted, now.Add(3*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "no authorization link") {
		t.Errorf("reusing the address should fail, got %v", err)
	}
}

func TestAuthFlowRejects(t *testing.T) {
	t.Chdir(t.TempDir())
	config := fakeTokenServer(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	link, err := startAuth(config, "calendar", now)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	state := url.QueryEscape(u.Query().Get("state"))

	tests := []struct {
		name   string
		pasted string
		at     time.Time
		want   string
	}{
		{"not a redirect", "hello", now, "doesn't look like"},
		{"access denied", "http://localhost:8080/callback?error=access_denied&state=" + state, now, "wasn't approved"},
		{"state of another link", "http://localhost:8080/callback?state=other&code=good-code", now, "different"},
		{"link too old", "http://localhost:8080/callback?state=" + state + "&code=good-code", now.Add(20 * time.Minute), "expired"},
		{"bad code", "localhost:8080/callback?state=" + state + "&code=bad", now, "exchange"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := completeAuth(context.Background(), config, "calendar", "token_calendar.json", tt.pasted, tt.at)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("want an error mentioning %q, got %v", tt.want, err)
			}
		})
	}
}
