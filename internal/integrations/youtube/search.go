package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type YouTubeSearchResponse struct {
	Items []struct {
		ID struct {
			VideoID string `json:"videoId"`
		} `json:"id"`
		Snippet struct {
			Title string `json:"title"`
		} `json:"snippet"`
	} `json:"items"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// SearchTrustedRoutine searches YouTube for high-quality, follow-along workouts and discards junk content.
func SearchTrustedRoutine(apiKey, query string, durationMinutes int) (title string, urlStr string, err error) {
	if apiKey == "" {
		return "", "", errors.New("YOUTUBE_API_KEY is not configured")
	}

	enhancedQuery := fmt.Sprintf("%s %d min full workout follow along", query, durationMinutes)
	apiURL := fmt.Sprintf("https://www.googleapis.com/youtube/v3/search?part=snippet&type=video&maxResults=5&q=%s&key=%s",
		url.QueryEscape(enhancedQuery), apiKey)

	resp, err := httpClient.Get(apiURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to reach YouTube API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("youtube api error status %d: %s", resp.StatusCode, string(body))
	}

	var result YouTubeSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("failed to parse youtube response: %w", err)
	}

	denylistTerms := []string{"shorts", "reaction", "fail", "preview", "trailer", "teaser", "podcast", "vlog"}
	for _, item := range result.Items {
		tLower := strings.ToLower(item.Snippet.Title)
		isJunk := false
		for _, term := range denylistTerms {
			if strings.Contains(tLower, term) {
				isJunk = true
				break
			}
		}
		if !isJunk && item.ID.VideoID != "" {
			return item.Snippet.Title, fmt.Sprintf("https://www.youtube.com/watch?v=%s", item.ID.VideoID), nil
		}
	}

	if len(result.Items) == 0 {
		return "", "", errors.New("no matching workout videos found")
	}

	return "", "", errors.New("all YouTube results matched denylist criteria (no reliable workout routine found)")
}
