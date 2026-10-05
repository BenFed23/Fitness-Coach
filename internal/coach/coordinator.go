package coach

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"fitness-agent/internal/bot"
	"fitness-agent/internal/integrations/gcalendar"
	"fitness-agent/internal/model"
	"fitness-agent/internal/scheduler"
)

type Coordinator struct {
	Config model.AppConfig
	Bot    *bot.Bot
}

func NewCoordinator(cfg model.AppConfig, b *bot.Bot) *Coordinator {
	return &Coordinator{
		Config: cfg,
		Bot:    b,
	}
}

// RescheduleWorkoutFlow intelligently searches for a 90-minute slot (or 40-minute compressed fallback) and updates Google Calendar.
func (c *Coordinator) RescheduleWorkoutFlow(ctx context.Context, client *http.Client, targetDate time.Time) error {
	loc, err := time.LoadLocation(c.Config.CalendarTimeZone)
	if err != nil {
		loc = time.Local
	}

	busySlots, events, err := gcalendar.FetchDayEvents(ctx, client, targetDate, loc)
	if err != nil {
		return fmt.Errorf("failed to fetch calendar events: %w", err)
	}

	// 1. Locate the workout event to reschedule
	var targetEvent *gcalendar.CalendarEvent
	for i := range events {
		for _, kw := range c.Config.WorkoutKeywords {
			if strings.Contains(strings.ToLower(events[i].Summary), strings.ToLower(kw)) {
				targetEvent = &events[i]
				break
			}
		}
		if targetEvent != nil {
			break
		}
	}

	// Filter out the workout itself from busySlots if found, so it doesn't block its own rescheduling!
	var nonWorkoutBusy []model.TimeSlot
	for _, b := range busySlots {
		if targetEvent != nil && b.Start.Equal(targetEvent.Start) && b.End.Equal(targetEvent.End) {
			continue
		}
		nonWorkoutBusy = append(nonWorkoutBusy, b)
	}

	// 2. Setup workout profiles (90-min full session, 40-min compressed session)
	profiles := model.DefaultWorkoutProfiles()
	fullWorkout := profiles[model.WorkoutTypeGymStrength]
	compressedWorkout := profiles[model.WorkoutTypeCompressed]

	// 3. Find optimal slot
	now := time.Now().In(loc)
	proposal := scheduler.FindBestSlot(
		targetDate,
		now,
		c.Config.ActiveHoursStart,
		c.Config.ActiveHoursEnd,
		nonWorkoutBusy,
		fullWorkout,
		compressedWorkout,
		loc,
	)

	if !proposal.Found {
		msg := fmt.Sprintf("⚠️ [מאמן אישי] לא נמצא חלון פנוי היום ביומן עבור אימון (אפילו לא 40 דק' מקוצר).\n💡 המלצה: קח היום יום מנוחה והתאוששות (Active Recovery), ונחזור בעוצמה מחר!")
		fmt.Println(msg)
		if c.Bot != nil {
			_ = c.Bot.SendMessage(msg)
		}
		return nil
	}

	// 4. Update or Create Google Calendar event with optimistic lock
	slotStr := fmt.Sprintf("%02d:%02d - %02d:%02d",
		proposal.Slot.Start.Hour(), proposal.Slot.Start.Minute(),
		proposal.Slot.End.Hour(), proposal.Slot.End.Minute())

	var title string
	var desc string

	if proposal.IsFallback {
		title = "אימון כוח מקוצר (Supersets - יום עמוס)"
		desc = "אימון מותאם על ידי המאמן האישי: 40 דקות בעצימות גבוהה לשמירה על המומנטום."
	} else {
		title = "אימון כוח מלא בחדר כושר (90 דקות)"
		desc = "אימון כוח מלא מתוזמן: 90 דקות אימון + 15 דקות באפר התארגנות."
	}

	if targetEvent != nil {
		if err := gcalendar.RescheduleEvent(ctx, client, *targetEvent, proposal.Slot, title, desc, c.Config.CalendarTimeZone); err != nil {
			return fmt.Errorf("reschedule calendar event: %w", err)
		}
	} else {
		if err := gcalendar.CreateEvent(ctx, client, proposal.Slot, title, desc, c.Config.CalendarTimeZone); err != nil {
			return fmt.Errorf("create calendar event: %w", err)
		}
	}

	var notifyMsg string
	if proposal.IsFallback {
		notifyMsg = fmt.Sprintf("⚡️ *הלו\"ז עודכן: אימון מקוצר!*\n\nלא נמצא חלון רציף של 90 דקות היום, לכן שיבצתי אימון כוח מרוכז (סופר-סטים) של 40 דקות:\n🕒 *שעה:* %s\n📌 *אימון:* %s",
			slotStr, title)
	} else {
		notifyMsg = fmt.Sprintf("✅ *הלו\"ז עודכן: אימון כוח מלא!*\n\nנמצא חלון מעולה של 90 דקות (כולל באפר התארגנות):\n🕒 *שעה:* %s\n📌 *אימון:* %s",
			slotStr, title)
	}

	fmt.Println(notifyMsg)
	if c.Bot != nil {
		_ = c.Bot.SendMessage(notifyMsg)
	}

	return nil
}
