package xcresult3

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"howett.net/plist"

	"github.com/bitrise-io/go-utils/fileutil"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-xcode/xcodeproject/serialized"
	"github.com/bitrise-steplib/steps-deploy-to-bitrise-io/test/junit"

	"github.com/shirou/gopsutil/load"
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

func getOptimalWorkerCount() int {
	cpuCount := runtime.NumCPU()
	log.Debugf("Current CPU count: %d", cpuCount)

	// Get current CPU load averages
	info, err := load.Avg()
	if err != nil {
		log.Debugf("Error getting load info: %v, falling back to CPU count", err)
		return runtime.NumCPU()
	}

	log.Debugf("System load averages - 1min: %.2f, 5min: %.2f, 15min: %.2f",
		info.Load1, info.Load5, info.Load15)

	// Calculate load per CPU
	loadPerCPU := info.Load1 / float64(cpuCount)
	log.Debugf("Current load per CPU: %.2f", loadPerCPU)

	// Determine optimal worker count based on system load
	var workerCount int
	switch {
	case loadPerCPU >= 1.0:
		// System is heavily loaded (more than 1 process per CPU)
		workerCount = max(1, cpuCount/4)
		log.Debugf("System heavily loaded (%.2f load per CPU), reducing workers to %d",
			loadPerCPU, workerCount)

	case loadPerCPU >= 0.7:
		// System is moderately loaded
		workerCount = max(1, cpuCount/2)
		log.Debugf("System moderately loaded (%.2f load per CPU), setting workers to %d",
			loadPerCPU, workerCount)

	default:
		// System has capacity
		// Use up to 75% of available CPUs
		workerCount = max(1, int(float64(cpuCount)*0.75))
		log.Debugf("System lightly loaded (%.2f load per CPU), setting workers to %d",
			loadPerCPU, workerCount)
	}

	// Cap maximum workers
	maxWorkers := runtime.NumCPU() * 2
	if workerCount > maxWorkers {
		log.Debugf("Capping worker count from %d to maximum %d",
			workerCount, maxWorkers)
		return maxWorkers
	}

	return workerCount
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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

	// Use adaptive worker count
	maxParallel = getOptimalWorkerCount()
	log.Debugf("Using %d workers for test suite [%s]", maxParallel, name)

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

	// Create and fill jobs channel
	jobs := make(chan testJob, len(tests))
	for i, test := range tests {
		jobs <- testJob{test: test, idx: i}
	}
	close(jobs)

	results := make(chan testResult, len(tests))

	// Start worker pool
	for i := 0; i < maxParallel; i++ {
		go func(workerID int) {
			for job := range jobs {
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
		}(i)
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
