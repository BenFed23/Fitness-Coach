package gcalendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"fitness-agent/internal/model"
	"golang.org/x/oauth2"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	callbackURL    = "http://localhost:8080/callback"
)

var (
	ErrStaleCalendarEvent = errors.New("calendar event changed since it was fetched (stale ETag)")
)

type CalendarEvent struct {
	ID      string
	ETag    string
	Summary string
	Start   time.Time
	End     time.Time
}

func GetOAuthConfig() (*oauth2.Config, error) {
	clientID := os.Getenv("CLIENT_ID")
	clientSecret := os.Getenv("CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("CLIENT_ID and CLIENT_SECRET must be configured in environment")
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  callbackURL,
		Scopes:       []string{"https://www.googleapis.com/auth/calendar"},
		Endpoint:     oauth2.Endpoint{AuthURL: googleAuthURL, TokenURL: googleTokenURL},
	}, nil
}

func LoadClient(ctx context.Context, config *oauth2.Config, tokenPath string) (*http.Client, error) {
	file, err := os.Open(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("token file not found: %w", err)
	}
	defer file.Close()

	var token oauth2.Token
	if err := json.NewDecoder(file).Decode(&token); err != nil {
		return nil, fmt.Errorf("invalid token json format: %w", err)
	}
	if !token.Valid() && token.RefreshToken == "" {
		return nil, errors.New("oauth token is invalid and has no refresh token")
	}
	return config.Client(ctx, &token), nil
}

// FetchDayEvents queries Google Calendar for all events occurring on a specific date.
func FetchDayEvents(ctx context.Context, client *http.Client, date time.Time, loc *time.Location) ([]model.TimeSlot, []CalendarEvent, error) {
	if loc == nil {
		loc = date.Location()
	}

	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc)
	endOfDay := startOfDay.Add(24 * time.Hour)

	params := url.Values{}
	params.Set("timeMin", startOfDay.Format(time.RFC3339))
	params.Set("timeMax", endOfDay.Format(time.RFC3339))
	params.Set("singleEvents", "true")
	params.Set("orderBy", "startTime")

	endpoint := "https://www.googleapis.com/calendar/v3/calendars/primary/events?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build calendar list request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("execute calendar list request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("calendar API error status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var response struct {
		Items []struct {
			ID      string `json:"id"`
			ETag    string `json:"etag"`
			Summary string `json:"summary"`
			Start   struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"start"`
			End struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"end"`
		} `json:"items"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, nil, fmt.Errorf("decode calendar list response: %w", err)
	}

	var busySlots []model.TimeSlot
	var events []CalendarEvent

	for _, item := range response.Items {
		var start, end time.Time
		if item.Start.DateTime != "" {
			start, _ = time.Parse(time.RFC3339, item.Start.DateTime)
			end, _ = time.Parse(time.RFC3339, item.End.DateTime)
		} else if item.Start.Date != "" {
			// All-day event: treat as full day
			start = startOfDay
			end = endOfDay
		}

		if !start.IsZero() && !end.IsZero() {
			busySlots = append(busySlots, model.TimeSlot{Start: start, End: end})
			events = append(events, CalendarEvent{
				ID:      item.ID,
				ETag:    item.ETag,
				Summary: item.Summary,
				Start:   start,
				End:     end,
			})
		}
	}

	return busySlots, events, nil
}

// RescheduleEvent updates an existing event in Google Calendar with optimistic concurrency check (If-Match ETag).
func RescheduleEvent(ctx context.Context, client *http.Client, event CalendarEvent, newSlot model.TimeSlot, title, description, timeZone string) error {
	payload := map[string]any{
		"summary":     title,
		"description": description,
		"start":       map[string]string{"dateTime": newSlot.Start.Format(time.RFC3339), "timeZone": timeZone},
		"end":         map[string]string{"dateTime": newSlot.End.Format(time.RFC3339), "timeZone": timeZone},
	}
	body, _ := json.Marshal(payload)

	patchEndpoint := "https://www.googleapis.com/calendar/v3/calendars/primary/events/" + url.PathEscape(event.ID)
	patchReq, err := http.NewRequestWithContext(ctx, http.MethodPatch, patchEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build calendar patch request: %w", err)
	}
	patchReq.Header.Set("Content-Type", "application/json")
	if event.ETag != "" {
		patchReq.Header.Set("If-Match", event.ETag)
	}

	patchResp, err := client.Do(patchReq)
	if err != nil {
		return fmt.Errorf("execute calendar patch request: %w", err)
	}
	defer patchResp.Body.Close()

	if patchResp.StatusCode == http.StatusPreconditionFailed {
		return ErrStaleCalendarEvent
	}
	if patchResp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(patchResp.Body)
		return fmt.Errorf("calendar patch API error status %d: %s", patchResp.StatusCode, string(bodyBytes))
	}

	return nil
}

// CreateEvent inserts a new workout event into Google Calendar.
func CreateEvent(ctx context.Context, client *http.Client, slot model.TimeSlot, title, description, timeZone string) error {
	payload := map[string]any{
		"summary":     title,
		"description": description,
		"start":       map[string]string{"dateTime": slot.Start.Format(time.RFC3339), "timeZone": timeZone},
		"end":         map[string]string{"dateTime": slot.End.Format(time.RFC3339), "timeZone": timeZone},
	}
	body, _ := json.Marshal(payload)

	endpoint := "https://www.googleapis.com/calendar/v3/calendars/primary/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build calendar create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute calendar create request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("calendar create API error status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}
