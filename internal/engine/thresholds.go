package engine

type EvaluationThresholds struct {
	TempHazardousHighC   float64
	TempHazardousLowC    float64
	TempCautionHighC     float64
	TempCautionLowC      float64
	HumidityHazardousPct float64
	HumidityCautionPct   float64
	RainHazardousMM      float64
	RainCautionMM        float64
	WindHazardousKMH     float64
	WindCautionKMH       float64

	ReadinessHazardousBelow int
	ReadinessCautionBelow   int
	SleepHazardousBelowHrs  float64
	SleepCautionBelowHrs    float64
	RHRDeltaHazardousBPM    int
	RHRDeltaCautionBPM      int
}

func DefaultThresholds() EvaluationThresholds {
	return EvaluationThresholds{
		TempHazardousHighC:   35,
		TempHazardousLowC:    0,
		TempCautionHighC:     30,
		TempCautionLowC:      5,
		HumidityHazardousPct: 90,
		HumidityCautionPct:   75,
		RainHazardousMM:      10,
		RainCautionMM:        0.1,
		WindHazardousKMH:     40,
		WindCautionKMH:       25,

		ReadinessHazardousBelow: 50,
		ReadinessCautionBelow:   70,
		SleepHazardousBelowHrs:  4.5,
		SleepCautionBelowHrs:    6.0,
		RHRDeltaHazardousBPM:    10,
		RHRDeltaCautionBPM:      5,
	}
}
