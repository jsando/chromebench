package main

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

type MotionMarkTest struct {
	// fps overrides MotionMark's detected target frame rate; 0 keeps the detected value.
	fps int
}

func (t *MotionMarkTest) Name() string {
	return "motionmark"
}

func (t *MotionMarkTest) Run(ctx context.Context) (*TestResult, error) {
	result := &TestResult{
		TestName:  t.Name(),
		StartTime: time.Now(),
		Metrics:   make(map[string]interface{}),
	}

	var scoreStr, confidenceStr string
	var subtestNames, subtestScores, subtestConfidences []string
	var targetFPS float64

	actions := []chromedp.Action{
		chromedp.Navigate("https://browserbench.org/MotionMark/"),
		chromedp.WaitVisible(`#intro`),
		// The start button is enabled once frame rate detection completes; starting
		// earlier lets detection overwrite the options mid-run.
		chromedp.WaitEnabled(`#start-button`, chromedp.ByID),
	}
	if t.fps > 0 {
		actions = append(actions, chromedp.Evaluate(fmt.Sprintf(
			`benchmarkController.benchmarkDefaultParameters["frame-rate"] = %d;
			benchmarkController.benchmarkDefaultParameters["system-frame-rate"] = %d;`, t.fps, t.fps), nil))
	}
	actions = append(actions,
		chromedp.Evaluate(`benchmarkController.startBenchmark()`, nil),
		chromedp.WaitVisible(`#results`, chromedp.ByID),

		// Target frame rate the run was scored against
		chromedp.Evaluate(`benchmarkRunnerClient.results._targetFrameRate`, &targetFPS),

		// Extract overall score and confidence
		chromedp.Text(`#results .score-container .score`, &scoreStr, chromedp.NodeVisible),
		chromedp.Text(`#results .score-container .confidence`, &confidenceStr, chromedp.NodeVisible),

		// Extract subtest names, scores, and confidences (skip the first .suites-separator row)
		chromedp.Evaluate(`Array.from(document.querySelectorAll('#results-header td')).slice(1).map(td => td.textContent.trim())`, &subtestNames),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('#results-score td')).slice(1).map(td => td.textContent.trim())`, &subtestScores),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('#results-data td span')).slice(1).map(span => span.textContent.trim())`, &subtestConfidences),
	)

	err := chromedp.Run(ctx, actions...)

	// Parse overall score
	var score float64
	fmt.Sscanf(scoreStr, "%f", &score)

	// Parse subscores
	subscores := make(map[string]interface{})
	for i := 0; i < len(subtestNames) && i < len(subtestScores); i++ {
		var val float64
		fmt.Sscanf(subtestScores[i], "%f", &val)
		subscores[subtestNames[i]] = val
		// Optionally, also store confidence intervals
		if i < len(subtestConfidences) {
			subscores[subtestNames[i]+"_confidence"] = subtestConfidences[i]
		}
	}

	result.EndTime = time.Now()

	if err != nil {
		result.Success = false
		result.Error = err
		return result, err
	}

	result.Success = true
	result.Metrics["overall_score"] = score
	result.Metrics["target_fps"] = targetFPS

	// Add subscores to metrics
	for key, value := range subscores {
		result.Metrics[fmt.Sprintf("subscore_%s", key)] = value
	}

	return result, nil
}
