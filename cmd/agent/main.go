package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
	// Embedded time zone database, so Asia/Jerusalem also loads on a server
	// or container without one installed.
	_ "time/tzdata"
)

func main() {
	reschedule := flag.Bool("reschedule", false,
		"if today's verdict is to skip the workout, try to reschedule it on Google Calendar")
	exhausted := flag.Bool("exhausted", false,
		"self-report: you feel exhausted today (a CAUTION signal, unless the readiness score is 65 or higher)")
	sick := flag.Bool("sick", false,
		"self-report: you feel sick today (workout adjustment only, not the verdict)")
	pain := flag.Bool("pain", false,
		"self-report: you are in pain today (workout adjustment only, not the verdict)")
	reportedSleep := flag.Float64("reported-sleep", -1,
		"self-report: hours slept last night (workout adjustment only, not the verdict)")
	jsonOutput := flag.Bool("json", false,
		"print the data and verdict as JSON on stdout (for the Telegram bot); progress messages go to stderr")
	moveToday := flag.Bool("move-today", false,
		"move today's workout on Google Calendar regardless of the verdict (e.g. you're busy), then exit")
	dryRun := flag.Bool("dry-run", false,
		"with -move-today: only show what would change, without touching the calendar")
	equipment := flag.String("equipment", "",
		"with -move-today: what you have for a short workout: none, dumbbells or gym (default: any video)")
	noReplace := flag.Bool("no-replace", false,
		"with -move-today: only move the workout; if the week is full, fail instead of replacing it with a short workout")
	authFor := flag.String("auth", "",
		"re-authorize Google without a local browser: health or calendar. Prints a link; then pass the address you land on with -auth-code")
	authCode := flag.String("auth-code", "",
		"with -auth: the full address Google redirected to after approving (http://localhost:8080/callback?...)")
	flag.Parse()

	// In JSON mode stdout must carry nothing but the JSON document, so every
	// progress message printed along the way is sent to stderr instead. There
	// is also nobody to complete a browser authorization.
	jsonOut := os.Stdout
	if *jsonOutput {
		os.Stdout = os.Stderr
		interactiveAuth = false
	}

	if *authFor != "" {
		result := runAuthFlow(*authFor, *authCode, time.Now())
		if *jsonOutput {
			if err := writeAuthJSON(jsonOut, result); err != nil {
				fmt.Printf("Could not write JSON output: %v\n", err)
				os.Exit(1)
			}
		} else {
			printAuthResult(result)
		}
		if result.Err != nil && !*jsonOutput {
			os.Exit(1)
		}
		return
	}

	if *moveToday {
		result := moveTodaysWorkout(time.Now(), *dryRun, *equipment, *noReplace)
		if *jsonOutput {
			if err := writeMoveJSON(jsonOut, result); err != nil {
				fmt.Printf("Could not write JSON output: %v\n", err)
				os.Exit(1)
			}
		} else {
			printMoveResult(result)
		}
		if result.Err != nil && !*jsonOutput {
			os.Exit(1)
		}
		return
	}

	loc, err := GetCurrentLocation()
	if err != nil {
		fmt.Printf("Could not determine location automatically: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Location: %s, %s (%.4f, %.4f)\n", loc.City, loc.Country, loc.Latitude, loc.Longitude)

	weatherURL := fmt.Sprintf(weatherAPIBase, loc.Latitude, loc.Longitude)
	weather, err := FetchWeatherData(weatherURL)
	if err != nil {
		fmt.Printf("Could not fetch weather data: %v\n", err)
		os.Exit(1)
	}

	recovery, err := FetchFitbitRecovery()
	if err != nil {
		// FetchFitbitRecovery already falls back internally, so this should
		// only trigger on an unexpected local I/O failure.
		fmt.Printf("Could not obtain recovery data: %v\n", err)
		os.Exit(1)
	}
	// The flag adds to (never clears) feeling_exhausted from recovery.json.
	recovery.FeelingExhausted = recovery.FeelingExhausted || *exhausted
	recovery.FeelingSick = *sick
	recovery.InPain = *pain
	if *reportedSleep >= 0 {
		recovery.ReportedSleepHours = reportedSleep
	}

	report := BuildAgentReport(weather, recovery)
	if *jsonOutput {
		// Today's workout only sets the wording, so skip the calendar when
		// there's no tier to word.
		var category string
		if adjustmentUnavailableReason(recovery) == "" {
			category = lookupTodaysWorkoutCategory(time.Now())
		}
		adjustment, adjustmentReason := computeAdjustment(recovery, report, category)
		if err := writeSnapshotJSON(jsonOut, loc, weather, recovery, report, adjustment, adjustmentReason); err != nil {
			fmt.Printf("Could not write JSON output: %v\n", err)
			os.Exit(1)
		}
	} else {
		printReport(report, recovery.Warnings)
	}

	if !*reschedule {
		return
	}
	var eligible func(category string) bool
	switch report.Kind {
	case VerdictSkipRecovery:
		eligible = nil // the body needs rest: any workout today is affected
	case VerdictSkipWeather:
		eligible = isOutdoorWorkout // unsafe weather: indoor gym sessions stay put
	default:
		return // not a skip verdict - nothing to reschedule
	}

	config, cfgErr := googleOAuthConfig(googleCalendarScopes...)
	if cfgErr != nil {
		fmt.Printf("Could not reschedule workout: %v\n", cfgErr)
		return
	}
	calClient, clientErr := googleClient(context.Background(), config, calendarTokenFile)
	if clientErr != nil {
		fmt.Printf("Could not reschedule workout: %v\n", clientErr)
		return
	}
	if err := RescheduleMissedWorkout(calClient, time.Now(), eligible); err != nil {
		fmt.Printf("Could not reschedule workout: %v\n", err)
	}
}
