package main

import (
	"fmt"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------
// Workout adjustment: HOW to change today's planned workout, on top of the
// coarse verdict. The tier is about recovery only; weather advice is added as
// a separate tip. Everything here is pure, so it's easy to test and tune.
// ---------------------------------------------------------------------

const (
	// At or above this readiness score the plan can grow a little.
	pushReadinessThreshold = 85
	// Start of Fitbit's "high" band: the plan stays as written.
	normalReadinessThreshold = 65
	// Below this readiness score (or with no score at all) only low and
	// medium intensity short-workout videos are picked (see
	// selectShortWorkout), and the adjustment drops from "reduced" to "easy".
	lowReadinessThreshold = 50
	// Below this is Fitbit's "low" band: no training.
	restReadinessThreshold = 30

	// Sleep reported in the chat, with the same limits as decide_workout in
	// telegram_bot/services.py: below these the bot says rest / light.
	reportedSleepRestHours  = 4
	reportedSleepLightHours = 6

	// A tier gives up to this many tips, leaving room in the JSON contract's
	// limit of 3 for one optional-extra or weather tip.
	maxTierTips = 2

	reportedPainReason = "you reported being in pain"
)

// adjustmentTier is ordered: a higher value allows more training.
type adjustmentTier int

const (
	tierRest adjustmentTier = iota
	tierEasy
	tierReduced
	tierNormal
	tierPush
)

func (t adjustmentTier) String() string {
	switch t {
	case tierRest:
		return "rest"
	case tierEasy:
		return "easy"
	case tierReduced:
		return "reduced"
	case tierNormal:
		return "normal"
	case tierPush:
		return "push"
	default:
		return "unknown"
	}
}

// workoutKind picks the vocabulary of the wording.
type workoutKind int

const (
	kindGeneric workoutKind = iota
	kindStrength
	kindRun
)

// tierWording is what a tier says for one kind of workout.
type tierWording struct {
	Summary string
	Tips    []string
	// Extra is an optional add-on, offered only on a "go" day.
	Extra string
}

// tierPlan is one row of adjustmentTable.
type tierPlan struct {
	VolumeChangePct int
	MaxIntensity    string
	Wording         map[workoutKind]tierWording
}

// adjustmentTable holds every percentage and all the wording, to tune here.
var adjustmentTable = map[adjustmentTier]tierPlan{
	tierPush: {
		VolumeChangePct: 0,
		MaxIntensity:    "high",
		Wording: map[workoutKind]tierWording{
			kindStrength: {
				Summary: "You look well recovered: do the planned session as written.",
				Tips:    []string{"Keep the planned sets and weights."},
				Extra:   "Optional: add one extra set to a main lift if it feels good.",
			},
			kindRun: {
				Summary: "You look well recovered: do the planned run as written.",
				Tips:    []string{"Keep the planned distance and pace."},
				Extra:   "Optional: add a few easy minutes at the end if it feels good.",
			},
			kindGeneric: {
				Summary: "You look well recovered: follow your plan as written.",
				Tips:    []string{"Stick to what you planned."},
				Extra:   "Optional: add a little extra if it feels good.",
			},
		},
	},
	tierNormal: {
		VolumeChangePct: 0,
		MaxIntensity:    "high",
		Wording: map[workoutKind]tierWording{
			kindStrength: {
				Summary: "Do the planned session as written.",
				Tips:    []string{"Keep the planned sets and weights."},
			},
			kindRun: {
				Summary: "Do the planned run as written.",
				Tips:    []string{"Keep the planned distance and pace."},
			},
			kindGeneric: {
				Summary: "Follow your plan as written.",
				Tips:    []string{"Stick to what you planned."},
			},
		},
	},
	tierReduced: {
		VolumeChangePct: -20,
		MaxIntensity:    "medium",
		Wording: map[workoutKind]tierWording{
			kindStrength: {
				Summary: "Do about 20% less volume today and keep the load moderate.",
				Tips:    []string{"Drop 1 set per exercise.", "Keep weights about 10% lighter, with no all-out sets."},
			},
			kindRun: {
				Summary: "Run about 20% less today at a moderate effort.",
				Tips:    []string{"Cut about 20% of the distance or duration.", "Hold a comfortable pace, with no all-out efforts."},
			},
			kindGeneric: {
				Summary: "Do about 20% less than planned at a moderate effort.",
				Tips:    []string{"Shorten the session a little.", "Skip any all-out efforts."},
			},
		},
	},
	tierEasy: {
		VolumeChangePct: -40,
		MaxIntensity:    "low",
		Wording: map[workoutKind]tierWording{
			kindStrength: {
				Summary: "Keep it easy: about 40% less volume with light weights.",
				Tips:    []string{"Do about 40% fewer sets.", "Use light weights only, with no heavy sets."},
			},
			kindRun: {
				Summary: "Keep it easy: run about 40% less at a gentle pace.",
				Tips:    []string{"Cut about 40% of the distance or duration.", "Keep it in zone 2, with no intervals."},
			},
			kindGeneric: {
				Summary: "Keep it easy: about 40% less than planned at a gentle effort.",
				Tips:    []string{"Keep the session short and gentle.", "Skip anything hard or heavy."},
			},
		},
	},
	tierRest: {
		VolumeChangePct: -100,
		MaxIntensity:    "low",
		Wording: map[workoutKind]tierWording{
			kindStrength: {
				Summary: "No training today: mobility or a walk at most.",
				Tips:    []string{"Skip the sets and weights today."},
			},
			kindRun: {
				Summary: "No training today: mobility or a walk at most.",
				Tips:    []string{"Skip the run today, whatever the pace or distance."},
			},
			kindGeneric: {
				Summary: "No training today: mobility or a walk at most.",
				Tips:    []string{"Let today be a recovery day."},
			},
		},
	},
}

// Adjustment is the "adjustment" object of the -json output.
type Adjustment struct {
	Tier            string   `json:"tier"` // push, normal, reduced, easy or rest
	VolumeChangePct int      `json:"volume_change_pct"`
	MaxIntensity    string   `json:"max_intensity"`    // low, medium or high
	WorkoutCategory string   `json:"workout_category"` // A, B, RUN, gym or generic
	Summary         string   `json:"summary"`
	Tips            []string `json:"tips"`
	// Reason is set when another signal made the tier lower than the score
	// alone would.
	Reason string `json:"reason"`
}

func tierForScore(score int) adjustmentTier {
	switch {
	case score >= pushReadinessThreshold:
		return tierPush
	case score >= normalReadinessThreshold:
		return tierNormal
	case score >= lowReadinessThreshold:
		return tierReduced
	case score >= restReadinessThreshold:
		return tierEasy
	default:
		return tierRest
	}
}

// adjustmentCategory maps a categorizeWorkout result (or "" for an unknown
// title or no workout today) to the JSON value and the wording to use.
func adjustmentCategory(category string) (string, workoutKind) {
	switch category {
	case "A", "B", "gym":
		return category, kindStrength
	case "run":
		return "RUN", kindRun
	default:
		return "generic", kindGeneric
	}
}

// adjustmentUnavailableReason says why no tier can be given today, or returns
// "" when there's a readiness score to base one on.
func adjustmentUnavailableReason(r *RecoveryData) string {
	switch {
	case r.TrackerNotWorn:
		return "the tracker wasn't worn overnight"
	case !r.ReadinessIsPlaceholder:
		return ""
	case r.ReadinessUnavailable != "":
		return "no readiness score today: " + r.ReadinessUnavailable
	default:
		return "no readiness score today"
	}
}

// adjustedTier starts from the readiness score, lowers it one step for pain
// or for fatigue that counts (see fatigueCounts: not on a good score), then
// applies the caps that keep it from saying
// more than the verdict or the bot's own decision. It returns the tier and why
// it ended up lower than the score alone, if it did.
func adjustedTier(r *RecoveryData, report *AgentReport) (adjustmentTier, string) {
	tier := tierForScore(r.ReadinessScore)
	var reasons []string

	if (r.fatigueCounts() || r.InPain) && tier > tierRest {
		tier--
		if r.fatigueCounts() {
			reasons = append(reasons, "you reported feeling tired")
		}
		if r.InPain {
			reasons = append(reasons, reportedPainReason)
		}
	}

	capAt := func(limit adjustmentTier, condition bool, why string) {
		if condition && tier > limit {
			tier = limit
			if !slices.Contains(reasons, why) {
				reasons = append(reasons, why)
			}
		}
	}
	sleep := r.ReportedSleepHours
	// The strictest caps come first, so only they end up in the reason.
	capAt(tierRest, report.Kind == VerdictSkipRecovery || report.RecoverySeverity == Hazardous,
		"your recovery signals are in the red")
	capAt(tierRest, r.FeelingSick, "you reported feeling sick")
	capAt(tierRest, sleep != nil && *sleep < reportedSleepRestHours,
		fmt.Sprintf("you reported under %d hours of sleep", reportedSleepRestHours))
	// Severity rather than only the verdict: a skip_weather verdict hides a
	// recovery caution.
	capAt(tierReduced, report.Kind == VerdictLight || report.RecoverySeverity == Caution,
		"your recovery signals suggest easing off")
	capAt(tierReduced, r.InPain, reportedPainReason)
	capAt(tierReduced, sleep != nil && *sleep < reportedSleepLightHours,
		fmt.Sprintf("you reported under %d hours of sleep", reportedSleepLightHours))

	return tier, strings.Join(reasons, "; ")
}

// weatherTip is the weather advice added to the tips. It never changes the
// tier. Gym sessions (A, B, a bare "Workout") are indoors, so they get none.
func weatherTip(weather Severity, kind workoutKind) string {
	switch {
	case kind == kindRun && weather == Hazardous:
		return "Weather: unsafe for running outside, so move it indoors."
	case kind == kindRun && weather == Caution:
		return "Weather: not ideal, so adjust your pace, clothing and hydration."
	case kind == kindGeneric && weather == Hazardous:
		return "Weather: if you planned to train outside, move it indoors."
	case kind == kindGeneric && weather == Caution:
		return "Weather: if you train outside, adjust your pace, clothing and hydration."
	default:
		return ""
	}
}

// computeAdjustment returns today's adjustment for a workout of the given
// categorizeWorkout category. When no tier can be given it returns nil and a
// short reason instead.
func computeAdjustment(r *RecoveryData, report *AgentReport, category string) (*Adjustment, string) {
	if reason := adjustmentUnavailableReason(r); reason != "" {
		return nil, reason
	}
	tier, reason := adjustedTier(r, report)
	jsonCategory, kind := adjustmentCategory(category)
	plan := adjustmentTable[tier]
	wording := plan.Wording[kind]

	tips := append([]string{}, wording.Tips...)
	if wording.Extra != "" && report.Kind == VerdictGo {
		tips = append(tips, wording.Extra)
	}
	// There's no training to adapt to the weather on a rest day.
	if tip := weatherTip(report.WeatherSeverity, kind); tip != "" && tier != tierRest {
		tips = append(tips, tip)
	}
	return &Adjustment{
		Tier:            tier.String(),
		VolumeChangePct: plan.VolumeChangePct,
		MaxIntensity:    plan.MaxIntensity,
		WorkoutCategory: jsonCategory,
		Summary:         wording.Summary,
		Tips:            tips,
		Reason:          reason,
	}, ""
}
