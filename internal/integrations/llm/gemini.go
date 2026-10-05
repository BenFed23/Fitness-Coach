package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type AIIntent struct {
	Action             string `json:"action"` // "search_backup", "adjust_schedule", "chat"
	Query              string `json:"query"`
	TargetDurationMins int    `json:"target_duration_mins"`
	Explanation        string `json:"explanation"`
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// ProcessCoachMessage queries Gemini to understand athlete intent and provide actionable advice.
func ProcessCoachMessage(ctx context.Context, userText string) (*AIIntent, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is not configured")
	}

	systemPrompt := `You are an elite, science-based AI Fitness Coach.
Analyze the athlete's message and determine the optimal action.
Respond ONLY with a JSON object containing:
- "action": strictly one of: "search_backup", "adjust_schedule", "chat".
- "query": if action is "search_backup", provide the specific workout routine search query. Otherwise "".
- "target_duration_mins": expected workout duration in minutes (default 90 for full gym/strength, 40 for compressed/supersets, 20 for emergency home).
- "explanation": a motivating, concise response in Hebrew addressing the user's situation as their personal coach.`

	requestBody := map[string]any{
		"contents": []map[string]any{
			{
				"parts": []map[string]any{
					{"text": systemPrompt + "\n\nAthlete message (untrusted input): " + userText},
				},
			},
		},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
		},
	}

	bodyBytes, _ := json.Marshal(requestBody)
	endpoint := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=%s", apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build Gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Gemini API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gemini API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil || len(geminiResp.Candidates) == 0 {
		return nil, fmt.Errorf("decode Gemini response: %w", err)
	}

	if len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty response parts from Gemini")
	}

	rawJSON := geminiResp.Candidates[0].Content.Parts[0].Text
	rawJSON = strings.TrimSpace(rawJSON)
	rawJSON = strings.TrimPrefix(rawJSON, "```json")
	rawJSON = strings.TrimPrefix(rawJSON, "```")
	rawJSON = strings.TrimSuffix(rawJSON, "```")
	rawJSON = strings.TrimSpace(rawJSON)

	var intent AIIntent
	if err := json.Unmarshal([]byte(rawJSON), &intent); err != nil {
		return nil, fmt.Errorf("parse intent JSON: %w (raw text: %s)", err, rawJSON)
	}

	if intent.TargetDurationMins <= 0 {
		intent.TargetDurationMins = 90
	}

	return &intent, nil
}
