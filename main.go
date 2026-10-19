package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/systeminfo"
	"github.com/chromedp/chromedp"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type TestResult struct {
	TestName   string
	StartTime  time.Time
	EndTime    time.Time
	Success    bool
	Error      error
	Metrics    map[string]interface{}
	CPUSamples []CPUSample
}

type CPUSample struct {
	Timestamp time.Time
	Usage     float64
}

type Test interface {
	Name() string
	Run(ctx context.Context) (*TestResult, error)
}

type TestHarness struct {
	tests       []Test
	chromeFlags []string
	headless    bool
}

func main() {
	fmt.Printf("\nchromebench %s (%s/%s)\n", version, commit, buildDate)
	var (
		includeTests   = flag.String("include", "", "Comma-separated list of tests to include")
		excludeTests   = flag.String("exclude", "", "Comma-separated list of tests to exclude")
		headless       = flag.Bool("headless", false, "Run Chrome in headless mode")
		listTests      = flag.Bool("list", false, "List available tests")
		downloadVideos = flag.Bool("download-videos", false, "Download test videos and exit")
		motionmarkFPS  = flag.Int("motionmark-fps", 0, "MotionMark target frame rate (0 = use MotionMark's detected rate)")
	)
	flag.Parse()

	if *motionmarkFPS < 0 {
		log.Fatalf("-motionmark-fps must be >= 0, got %d", *motionmarkFPS)
	}

	harness := &TestHarness{
		headless: *headless,
	}

	// Parse Chrome flags after "--"
	harness.chromeFlags = flag.Args()

	// Initialize video cache
	videoCache, err := NewVideoCache()
	if err != nil {
		log.Fatalf("Failed to initialize video cache: %v", err)
	}

	// Register all available tests
	allTests := []Test{
		&MotionMarkTest{fps: *motionmarkFPS},
	}

	// Add video tests with local paths
	for _, videoInfo := range testVideos {
		localPath := videoCache.GetVideoPath(videoInfo)
		allTests = append(allTests, &VideoTest{
			name:       videoInfo.Name,
			videoURL:   "file://" + localPath,
			resolution: videoInfo.Resolution,
		})
	}

	if *listTests {
		fmt.Println("Available tests:")
		for _, test := range allTests {
			fmt.Printf("  - %s\n", test.Name())
		}
		return
	}

	if *downloadVideos {
		if err := videoCache.EnsureAllVideos(); err != nil {
			log.Fatalf("Failed to download test videos: %v", err)
		}
		return
	}

	// Filter tests based on include/exclude
	harness.tests = filterTests(allTests, *includeTests, *excludeTests)

	if len(harness.tests) == 0 {
		log.Fatal("No tests to run")
	}

	// Check if any video tests are included
	hasVideoTests := false
	for _, test := range harness.tests {
		if strings.HasPrefix(test.Name(), "video-") {
			hasVideoTests = true
			break
		}
	}

	// Download videos if needed
	if hasVideoTests {
		if err := videoCache.EnsureAllVideos(); err != nil {
			log.Fatalf("Failed to download test videos: %v", err)
		}
		fmt.Println()
	}

	// Run tests
	results := harness.RunTests()

	// Print summary
	printSummary(results)
}

func filterTests(allTests []Test, include, exclude string) []Test {
	var filtered []Test

	includeMap := make(map[string]bool)
	excludeMap := make(map[string]bool)

	if include != "" {
		for _, name := range strings.Split(include, ",") {
			includeMap[strings.TrimSpace(name)] = true
		}
	}

	if exclude != "" {
		for _, name := range strings.Split(exclude, ",") {
			excludeMap[strings.TrimSpace(name)] = true
		}
	}

	for _, test := range allTests {
		name := test.Name()

		// Skip if in exclude list
		if excludeMap[name] {
			continue
		}

		// Include if no include list or if in include list
		if len(includeMap) == 0 || includeMap[name] {
			filtered = append(filtered, test)
		}
	}

	return filtered
}

func (h *TestHarness) RunTests() []TestResult {
	var results []TestResult

	// Create Chrome options
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", h.headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-sandbox", true),
	)

	// Add custom Chrome flags
	for _, flag := range h.chromeFlags {
		// Remove leading dashes if present
		flag = strings.TrimLeft(flag, "-")

		parts := strings.SplitN(flag, "=", 2)
		if len(parts) == 2 {
			opts = append(opts, chromedp.Flag(parts[0], parts[1]))
		} else {
			opts = append(opts, chromedp.Flag(parts[0], true))
		}
	}

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancel()

	// Get GPU info first
	printGPUInfo(ctx)

	// Run each test
	for _, test := range h.tests {
		fmt.Printf("Running test: %s\n", test.Name())

		// Create a new context for each test with timeout
		testCtx, testCancel := context.WithTimeout(ctx, 20*time.Minute)

		// Start Chrome-specific CPU monitoring
		cpuMonitor := NewChromeCPUMonitor()
		cpuMonitor.Start()

		result, err := test.Run(testCtx)

		// Stop CPU monitoring
		cpuMonitor.Stop()

		if result == nil {
			result = &TestResult{
				TestName:  test.Name(),
				StartTime: time.Now(),
				EndTime:   time.Now(),
				Success:   false,
				Error:     err,
			}
		}

		result.CPUSamples = cpuMonitor.GetSamples()
		results = append(results, *result)

		testCancel()
		fmt.Println()
	}

	return results
}

func printGPUInfo(ctx context.Context) {
	// get gpu information
	product := "unknown"
	revision := "n/a"
	params := []string{}
	var gpu *systeminfo.GPUInfo
	err := chromedp.Run(ctx,
		chromedp.Navigate("about:blank"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			c := chromedp.FromContext(ctx)
			browserCtx := cdp.WithExecutor(ctx, c.Browser)
			var err error
			_, product, revision, _, _, err = browser.GetVersion().Do(browserCtx)
			if err != nil {
				return err
			}
			params, err = browser.GetBrowserCommandLine().Do(browserCtx)
			if err != nil {
				return err
			}
			gpu, _, _, _, err = systeminfo.GetInfo().Do(browserCtx)
			return err
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Browser: %s (%s)\n\n", product, revision)
	fmt.Printf("Commandline: %v\n\n", params)
	fmt.Println("\nGPU Information:")
	if len(gpu.Devices) > 0 {
		fmt.Printf("  GPU Vendor: %s\n", gpu.Devices[0].VendorString)
		fmt.Printf("  GPU Device: %s\n", gpu.Devices[0].DeviceString)
		fmt.Printf("  GPU Driver Version: %s\n", gpu.Devices[0].DriverVersion)
	} else {
		fmt.Printf("  GPU devices not available\n")
	}
	fmt.Println("\nGPU Feature Status:")
	if gpu.FeatureStatus != nil {
		var featureStatus map[string]string
		err := json.Unmarshal(gpu.FeatureStatus, &featureStatus)
		if err == nil {
			// Sort feature keys alphabetically
			keys := make([]string, 0, len(featureStatus))
			for k := range featureStatus {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, feature := range keys {
				fmt.Printf("  %s: %s\n", feature, featureStatus[feature])
			}
		} else {
			fmt.Printf("  Error parsing gpu feature status: %v\n", err)
		}
	}
	fmt.Println()
}

func printSummary(results []TestResult) {
	fmt.Println("=== Test Summary ===")
	fmt.Println()

	for _, result := range results {
		fmt.Printf("Test: %s\n", result.TestName)
		fmt.Printf("  Duration: %v\n", result.EndTime.Sub(result.StartTime))
		fmt.Printf("  Success: %v\n", result.Success)

		if result.Error != nil {
			fmt.Printf("  Error: %v\n", result.Error)
		}

		if len(result.CPUSamples) > 0 {
			avgCPU := calculateAverageCPU(result.CPUSamples)
			fmt.Printf("  Average CPU Usage: %.2f%%\n", avgCPU)
		}

		if len(result.Metrics) > 0 {
			fmt.Println("  Metrics:")
			// Sort keys alphabetically
			keys := make([]string, 0, len(result.Metrics))
			for key := range result.Metrics {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				fmt.Printf("    %s: %v\n", key, result.Metrics[key])
			}
		}

		fmt.Println()
	}
}

func calculateAverageCPU(samples []CPUSample) float64 {
	if len(samples) == 0 {
		return 0
	}

	var total float64
	for _, sample := range samples {
		total += sample.Usage
	}

	return total / float64(len(samples))
}
