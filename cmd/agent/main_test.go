package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestTokenFileKeepsAuthorizedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	authorizedAt := time.Date(2026, 9, 30, 11, 43, 57, 0, time.UTC)
	token := &oauth2.Token{AccessToken: "a", RefreshToken: "r", TokenType: "Bearer", Expiry: authorizedAt.Add(time.Hour)}
	if err := saveToken(path, token, authorizedAt); err != nil {
		t.Fatal(err)
	}
	got, gotAt, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "r" || !got.Expiry.Equal(token.Expiry) || !gotAt.Equal(authorizedAt) {
		t.Errorf("round trip lost data: token %+v, authorized_at %v", got, gotAt)
	}

	// A file written before authorized_at existed still loads.
	if err := os.WriteFile(path, []byte(`{"access_token":"a","refresh_token":"r","expiry":"2026-09-30T12:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, gotAt, err = loadToken(path)
	if err != nil || got.RefreshToken != "r" || !gotAt.IsZero() {
		t.Errorf("old-format file: token %+v, authorized_at %v, err %v", got, gotAt, err)
	}
}

func TestSleepDetail(t *testing.T) {
	start := time.Date(2026, 9, 12, 1, 44, 0, 0, time.Local)
	tests := []struct {
		name string
		r    RecoveryData
		want string
	}{
		{"recorded session", RecoveryData{SleepMinutes: 382, SleepStart: start, SleepEnd: start.Add(445 * time.Minute)}, "6h 22m asleep, 01:44-09:09"},
		{"step estimate", RecoveryData{SleepMinutes: 420, SleepEstimated: true}, "7h 00m asleep (estimated from steps)"},
		{"hours from recovery.json", RecoveryData{SleepHours: 6.5}, "6h 30m asleep"},
	}
	for _, tt := range tests {
		if got := tt.r.sleepDetail(); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}

	f := evaluateSleep(6.37, false, false, "6h 22m asleep, 01:44-09:09")
	if !strings.HasPrefix(f.Note, "6h 22m asleep, 01:44-09:09. ") {
		t.Errorf("sleep note should open with the detail, got %q", f.Note)
	}
}
