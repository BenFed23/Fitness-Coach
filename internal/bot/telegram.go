package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"fitness-agent/internal/integrations/llm"
	"fitness-agent/internal/integrations/youtube"
	"fitness-agent/internal/model"
)

type TelegramUpdate struct {
	UpdateID int `json:"update_id"`
	Message  struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
	CallbackQuery struct {
		ID   string `json:"id"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Message struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	} `json:"callback_query"`
}

type TelegramUpdatesResponse struct {
	Ok     bool             `json:"ok"`
	Result []TelegramUpdate `json:"result"`
}

type Bot struct {
	token       string
	chatID      string
	configPath  string
	offsetPath  string
	approvals   *ApprovalManager
	httpClient  *http.Client
	onResched   func(ctx context.Context)
}

func NewBot(configPath, offsetPath string, onResched func(ctx context.Context)) *Bot {
	return &Bot{
		token:      os.Getenv("TELEGRAM_BOT_TOKEN"),
		chatID:     os.Getenv("TELEGRAM_CHAT_ID"),
		configPath: configPath,
		offsetPath: offsetPath,
		approvals:  NewApprovalManager(1 * time.Hour),
		httpClient: &http.Client{Timeout: 12 * time.Second},
		onResched:  onResched,
	}
}

func (b *Bot) IsAuthorized(senderID int64) bool {
	if b.chatID == "" {
		return false
	}
	expectedID, err := strconv.ParseInt(b.chatID, 10, 64)
	if err != nil {
		return false
	}
	return senderID == expectedID
}

func (b *Bot) SendMessage(text string) error {
	if b.token == "" || b.chatID == "" {
		return errors.New("TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID missing")
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	data := url.Values{
		"chat_id": {b.chatID},
		"text":    {text},
	}

	resp, err := b.httpClient.PostForm(apiURL, data)
	if err != nil {
		return fmt.Errorf("failed to send telegram message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API error status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

func (b *Bot) SendApprovalRequest(requestID, title, videoURL string, durationMins int) error {
	if b.token == "" || b.chatID == "" {
		return errors.New("TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID missing")
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	replyMarkup := map[string]any{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ אשר אימון חלופי", "callback_data": fmt.Sprintf("approve_backup:%s", requestID)},
				{"text": "🔄 חפש סרטון אחר", "callback_data": fmt.Sprintf("reject_backup:%s", requestID)},
			},
		},
	}
	markupBytes, _ := json.Marshal(replyMarkup)

	msgText := fmt.Sprintf("🔍 *אימון חלופי נמצא ועבר סינון אמינות*\n\n📌 *%s*\n⏱ משך משוער: %d דקות\n🔗 %s\n\nהאם לאשר את השיבוץ שלו בלוח הזמנים?",
		title, durationMins, videoURL)

	data := url.Values{
		"chat_id":      {b.chatID},
		"text":         {msgText},
		"parse_mode":   {"Markdown"},
		"reply_markup": {string(markupBytes)},
	}

	resp, err := b.httpClient.PostForm(apiURL, data)
	if err != nil {
		return fmt.Errorf("failed to send approval request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API error status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

func (b *Bot) handleCallbackQuery(callbackID, data string, senderID int64) {
	if !b.IsAuthorized(senderID) {
		return
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", b.token)
	formData := url.Values{"callback_query_id": {callbackID}}
	if resp, err := b.httpClient.PostForm(apiURL, formData); err == nil && resp != nil {
		_ = resp.Body.Close()
	}

	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return
	}
	action := parts[0]
	requestID := parts[1]

	routine, exists := b.approvals.Pop(requestID)

	if action == "approve_backup" {
		if !exists {
			_ = b.SendMessage("⚠️ בקשת האישור פגה או אינה קיימת.")
			return
		}
		cfg, err := model.LoadConfig(b.configPath)
		if err != nil {
			_ = b.SendMessage("⚠️ שגיאה בטעינת קובץ ההגדרות.")
			return
		}
		cfg.FallbackRoutines["default"] = routine
		if err := model.SaveConfig(b.configPath, cfg); err != nil {
			_ = b.SendMessage("⚠️ שגיאה בשמירת קובץ ההגדרות.")
			return
		}
		_ = b.SendMessage(fmt.Sprintf("✅ אישור התקבל!\nהגדרתי בהצלחה את אימון הגיבוי החדש:\n📌 *%s* (%d דקות)",
			routine.Title, routine.DurationMinutes))
	} else if action == "reject_backup" {
		_ = b.SendMessage("❌ האימון נדחה. תוכל לבקש חיפוש חלופי בכל עת.")
	}
}

func (b *Bot) processMessage(ctx context.Context, text string) string {
	intent, err := llm.ProcessCoachMessage(ctx, text)
	if err != nil {
		return fmt.Sprintf("⚠️ מאמן ה-AI נתקל בשגיאה: %v", err)
	}

	switch intent.Action {
	case "search_backup":
		q := intent.Query
		if q == "" {
			q = "Full body strength workout"
		}
		duration := intent.TargetDurationMins
		if duration <= 0 {
			duration = 90
		}
		title, videoURL, err := youtube.SearchTrustedRoutine(os.Getenv("YOUTUBE_API_KEY"), q, duration)
		if err != nil {
			return fmt.Sprintf("⚠️ נכשלתי באיתור אימון ביוטיוב: %v", err)
		}

		routine := model.RoutineConfig{
			Title:           title,
			URL:             videoURL,
			DurationMinutes: duration,
			WorkoutType:     model.WorkoutTypeHomeBackup,
		}
		reqID := b.approvals.RegisterRoutine(routine)

		if err := b.SendApprovalRequest(reqID, title, videoURL, duration); err != nil {
			return fmt.Sprintf("⚠️ שגיאה בשליחת בקשת אישור לטלגרם: %v", err)
		}
		return "⏳ מצאתי אימון מתאים. מחכה לאישור שלך בטלגרם..."

	case "adjust_schedule":
		if b.onResched != nil {
			go func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				b.onResched(bgCtx)
			}()
		}
		return fmt.Sprintf("🧠 %s\n\n🔄 סרקתי את הלו\"ז ובדקתי חלון של 90 דקות (או 40 דק' לאימון מקוצר). האירוע מתעדכן!", intent.Explanation)

	case "chat":
		if intent.Explanation == "" {
			return "הבנתי אותך. קדימה לאימון!"
		}
		return intent.Explanation

	default:
		return "פעולה לא מזוהה מה-AI."
	}
}

func (b *Bot) StartListener(ctx context.Context) {
	if b.token == "" {
		return
	}

	offset := model.LoadTelegramOffset(b.offsetPath)
	fmt.Println("🤖 Telegram Coach Bot listener started (Fail-Closed, Single-User Security)...")

	for {
		select {
		case <-ctx.Done():
			return
		default:
			apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", b.token, offset)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
					continue
				}
			}

			resp, err := b.httpClient.Do(req)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
					continue
				}
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
					continue
				}
			}

			var tResp TelegramUpdatesResponse
			if err := json.NewDecoder(resp.Body).Decode(&tResp); err != nil {
				resp.Body.Close()
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
					continue
				}
			}
			resp.Body.Close()

			for _, update := range tResp.Result {
				offset = update.UpdateID + 1
				model.SaveTelegramOffset(b.offsetPath, offset)

				if update.CallbackQuery.ID != "" {
					b.handleCallbackQuery(update.CallbackQuery.ID, update.CallbackQuery.Data, update.CallbackQuery.From.ID)
				} else if update.Message.Text != "" {
					if !b.IsAuthorized(update.Message.From.ID) {
						continue
					}
					reply := b.processMessage(ctx, update.Message.Text)
					if !strings.HasPrefix(reply, "⏳ מחכה לאישור") {
						_ = b.SendMessage(reply)
					}
				}
			}
		}
	}
}
