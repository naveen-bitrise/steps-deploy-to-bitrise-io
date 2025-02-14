package xcresult3

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"howett.net/plist"

	"github.com/bitrise-io/go-utils/fileutil"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-xcode/xcodeproject/serialized"
	"github.com/bitrise-steplib/steps-deploy-to-bitrise-io/test/junit"

	"github.com/shirou/gopsutil/v3/cpu"
)

// Converter ...
type Converter struct {
	xcresultPth string
}

func majorVersion(document serialized.Object) (int, error) {
	version, err := document.Object("version")
	if err != nil {
		return -1, err
	}

	major, err := version.Value("major")
	if err != nil {
		return -1, err
	}
	return int(major.(uint64)), nil
}

func documentMajorVersion(pth string) (int, error) {
	content, err := fileutil.ReadBytesFromFile(pth)
	if err != nil {
		return -1, err
	}

	var info serialized.Object
	if _, err := plist.Unmarshal(content, &info); err != nil {
		return -1, err
	}

	return majorVersion(info)
}

// Detect ...
func (c *Converter) Detect(files []string) bool {
	if !isXcresulttoolAvailable() {
		log.Debugf("xcresult tool is not available")
		return false
	}

	for _, file := range files {
		if filepath.Ext(file) != ".xcresult" {
			continue
		}

		infoPth := filepath.Join(file, "Info.plist")
		if exist, err := pathutil.IsPathExists(infoPth); err != nil {
			log.Debugf("Failed to find Info.plist at %s: %s", infoPth, err)
			continue
		} else if !exist {
			log.Debugf("No Info.plist found at %s", infoPth)
			continue
		}

		version, err := documentMajorVersion(infoPth)
		if err != nil {
			log.Debugf("failed to get document version: %s", err)
			continue
		}

		if version < 3 {
			log.Debugf("version < 3: %d", version)
			continue
		}

		c.xcresultPth = file
		return true
	}
	return false
}

// XML ...
func (c *Converter) XML() (junit.XML, error) {
	var (
		testResultDir = filepath.Dir(c.xcresultPth)
		maxParallel   = runtime.NumCPU() * 2
	)

	log.Debugf("Maximum parallelism: %d.", maxParallel)

	_, summaries, err := Parse(c.xcresultPth)
	if err != nil {
		return junit.XML{}, err
	}

	var xmlData junit.XML
	{
		testSuiteCount := testSuiteCountInSummaries(summaries)
		xmlData.TestSuites = make([]junit.TestSuite, 0, testSuiteCount)
	}

	summariesCount := len(summaries)
	log.Debugf("Summaries Count: %d", summariesCount)

	for _, summary := range summaries {
		testSuiteOrder, testsByName := summary.tests()

		for _, name := range testSuiteOrder {
			tests := testsByName[name]

			testSuite, err := genTestSuite(name, summary, tests, testResultDir, c.xcresultPth, maxParallel)
			if err != nil {
				return junit.XML{}, err
			}

			xmlData.TestSuites = append(xmlData.TestSuites, testSuite)
		}
	}

	return xmlData, nil
}

func testSuiteCountInSummaries(summaries []ActionTestPlanRunSummaries) int {
	testSuiteCount := 0
	for _, summary := range summaries {
		testSuiteOrder, _ := summary.tests()
		testSuiteCount += len(testSuiteOrder)
	}
	return testSuiteCount
}

// GetCPUUsage returns the average CPU usage as a percentage.
func GetCPUUsage() (float64, error) {
	// Get the CPU usage for each core with 0 interval
	cpuPercentages, err := cpu.Percent(0, true)
	if err != nil {
		return 0, err
	}

	var total float64
	for _, percentage := range cpuPercentages {
		total += percentage
	}

	// Calculate average CPU usage
	avgUsage := total / float64(len(cpuPercentages))
	return avgUsage, nil
}

// AdjustMaxParallel adjusts maxParallel based on current CPU usage.
func AdjustMaxParallel() int {
	// Get current CPU load
	cpuLoad, err := GetCPUUsage()
	if err != nil {
		log.Debugf("Error getting CPU usage: %v, falling back to default", err)
		return runtime.NumCPU() * 2 // Fallback to default
	}

	cpuCount := runtime.NumCPU()
	baseMaxParallel := cpuCount * 2

	log.Debugf("Current CPU Usage: %.2f%%, CPU Count: %d, Base parallel: %d",
		cpuLoad, cpuCount, baseMaxParallel)

	// More granular adjustment based on CPU load
	var adjustedParallel int
	switch {
	case cpuLoad >= 90:
		// Very high load - reduce to 1/4
		adjustedParallel = max(1, baseMaxParallel/4)
		log.Debugf("Very high CPU load (%.2f%%), reducing workers to %d",
			cpuLoad, adjustedParallel)

	case cpuLoad >= 80:
		// High load - reduce to 1/2
		adjustedParallel = max(1, baseMaxParallel/2)
		log.Debugf("High CPU load (%.2f%%), reducing workers to %d",
			cpuLoad, adjustedParallel)

	case cpuLoad >= 60:
		// Moderate high load - reduce by 25%
		adjustedParallel = max(1, int(float64(baseMaxParallel)*0.75))
		log.Debugf("Moderately high CPU load (%.2f%%), setting workers to %d",
			cpuLoad, adjustedParallel)

	case cpuLoad <= 20:
		// Very low load - can increase up to 4x
		maxIncrease := cpuCount * 4
		adjustedParallel = min(baseMaxParallel*2, maxIncrease)
		log.Debugf("Very low CPU load (%.2f%%), increasing workers to %d",
			cpuLoad, adjustedParallel)

	case cpuLoad <= 40:
		// Low load - can increase up to 2x
		maxIncrease := cpuCount * 3
		adjustedParallel = min(baseMaxParallel*3/2, maxIncrease)
		log.Debugf("Low CPU load (%.2f%%), increasing workers to %d",
			cpuLoad, adjustedParallel)

	default:
		// Moderate load - keep base parallel
		adjustedParallel = baseMaxParallel
		log.Debugf("Moderate CPU load (%.2f%%), maintaining default workers at %d",
			cpuLoad, adjustedParallel)
	}

	return adjustedParallel
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type testJob struct {
	test ActionTestSummaryGroup
	idx  int
}

type testResult struct {
	testCase junit.TestCase
	idx      int
	err      error
	duration time.Duration
}

// startWorker extracts worker logic for reuse
func startWorker(workerID int,
	jobs <-chan testJob,
	results chan<- testResult,
	currentMaxParallel *atomic.Int32,
	activeWorkers *atomic.Int32,
	xcresultPath string,
	testResultDir string) {

	defer activeWorkers.Add(-1)

	for job := range jobs {
		// Check if we should exit based on reduced worker count
		if int32(workerID) >= currentMaxParallel.Load() {
			log.Debugf("Worker %d stopping due to reduced worker count", workerID)
			break
		}

		start := time.Now()
		log.Debugf("Worker %d starting test: %s", workerID, job.test.Name)

		testCase, err := genTestCase(job.test, xcresultPath, testResultDir)
		duration := time.Since(start)

		if duration > time.Second*10 {
			log.Debugf("Slow test case on worker %d: test %s took %v", workerID, job.test.Name, duration)
		}

		results <- testResult{
			testCase: testCase,
			idx:      job.idx,
			err:      err,
			duration: duration,
		}

		log.Debugf("Worker %d finished test %s in %v", workerID, job.test.Name, duration)
	}
}

func genTestSuite(name string,
	summary ActionTestPlanRunSummaries,
	tests []ActionTestSummaryGroup,
	testResultDir string,
	xcresultPath string,
	maxParallel int,
) (junit.TestSuite, error) {
	var genTestSuiteErr error
	suiteStart := time.Now()

	testSuite := junit.TestSuite{
		Name:      name,
		Tests:     len(tests),
		Failures:  summary.failuresCount(name),
		Skipped:   summary.skippedCount(name),
		Time:      summary.totalTime(name),
		TestCases: make([]junit.TestCase, len(tests)),
	}

	// Initialize atomic worker count
	currentMaxParallel := atomic.Int32{}
	activeWorkers := atomic.Int32{}
	currentMaxParallel.Store(int32(AdjustMaxParallel()))
	log.Debugf("Initial worker count: %d", currentMaxParallel.Load())

	// Create channels
	jobs := make(chan testJob, len(tests))
	results := make(chan testResult, len(tests))

	// Fill jobs channel
	for i, test := range tests {
		jobs <- testJob{test: test, idx: i}
	}
	close(jobs)

	// Start health check and worker management
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			newCount := AdjustMaxParallel()
			oldCount := currentMaxParallel.Swap(int32(newCount))
			if oldCount != int32(newCount) {
				log.Debugf("Adjusting worker count from %d to %d", oldCount, newCount)

				// If we need more workers, start them immediately
				if newCount > int(oldCount) {
					for i := oldCount; i < int32(newCount); i++ {
						activeWorkers.Add(1)
						go startWorker(int(i), jobs, results, &currentMaxParallel, &activeWorkers, xcresultPath, testResultDir)
					}
				}
				// Reduction happens automatically in worker loops
			}
		}
	}()

	// Start initial worker pool
	for i := 0; i < int(currentMaxParallel.Load()); i++ {
		activeWorkers.Add(1)
		go startWorker(i, jobs, results, &currentMaxParallel, &activeWorkers, xcresultPath, testResultDir)
	}

	// Collect results
	for i := 0; i < len(tests); i++ {
		result := <-results
		if result.err != nil {
			genTestSuiteErr = result.err
			log.Debugf("Test failed: %v", result.err)
		}
		testSuite.TestCases[result.idx] = result.testCase
	}

	log.Debugf("Test suite [%s] complete - %d tests in %v", name, len(tests), time.Since(suiteStart))
	return testSuite, genTestSuiteErr
}

func genTestCase(test ActionTestSummaryGroup, xcresultPath, testResultDir string) (junit.TestCase, error) {
	var duartion float64
	if test.Duration.Value != "" {
		var err error
		duartion, err = strconv.ParseFloat(test.Duration.Value, 64)
		if err != nil {
			return junit.TestCase{}, err
		}
	}

	testSummary, err := test.loadActionTestSummary(xcresultPath)
	// Ignoring the SummaryNotFoundError error is on purpose because not having an action summary is a valid use case.
	// For example, failed tests will always have a summary, but successful ones might have it or might not.
	// If they do not have it, then that means that they did not log anything to the console,
	// and they were not executed as device configuration tests.
	if err != nil && !errors.Is(err, ErrSummaryNotFound) {
		return junit.TestCase{}, err
	}

	var failure *junit.Failure
	var skipped *junit.Skipped
	switch test.TestStatus.Value {
	case "Failure":
		failureMessage := ""
		for _, aTestFailureSummary := range testSummary.FailureSummaries.Values {
			file := aTestFailureSummary.FileName.Value
			line := aTestFailureSummary.LineNumber.Value
			message := aTestFailureSummary.Message.Value

			if len(failureMessage) > 0 {
				failureMessage += "\n"
			}
			failureMessage += fmt.Sprintf("%s:%s - %s", file, line, message)
		}

		failure = &junit.Failure{
			Value: failureMessage,
		}
	case "Skipped":
		skipped = &junit.Skipped{}
	}

	if err := test.exportScreenshots(xcresultPath, testResultDir); err != nil {
		return junit.TestCase{}, err
	}

	return junit.TestCase{
		Name:              test.Name.Value,
		ConfigurationHash: testSummary.Configuration.Hash,
		ClassName:         strings.Split(test.Identifier.Value, "/")[0],
		Failure:           failure,
		Skipped:           skipped,
		Time:              duartion,
	}, nil
}
