package xcresult3

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"howett.net/plist"

	"github.com/bitrise-io/go-utils/fileutil"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-xcode/xcodeproject/serialized"
	"github.com/bitrise-steplib/steps-deploy-to-bitrise-io/test/junit"

	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"
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

// GetMemoryUsage returns the current system memory usage as a percentage.
func GetMemoryUsage() (float64, error) {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return vmStat.UsedPercent, nil
}

// GetTopProcesses logs the top N processes by CPU and memory usage.
func GetTopProcesses(topN int) {
	processes, err := process.Processes()
	if err != nil {
		log.Debugf("Error fetching processes: %v", err)
		return
	}

	var processInfo []struct {
		PID    int32
		Name   string
		CPU    float32
		Memory float32
	}

	// Iterate through processes and collect relevant details
	for _, p := range processes {
		// Get the process name
		name, err := p.Name()
		if err != nil {
			continue
		}

		// Get CPU usage for the process
		cpuPercent, err := p.CPUPercent()
		if err != nil {
			continue
		}

		// Get memory usage for the process
		memoryInfo, err := p.MemoryInfo()
		if err != nil {
			continue
		}

		// Append the process information
		processInfo = append(processInfo, struct {
			PID    int32
			Name   string
			CPU    float32
			Memory float32
		}{
			PID:    p.Pid,
			Name:   name,                                  // Use the process name from Name()
			CPU:    float32(cpuPercent),                   // Cast cpuPercent to float32
			Memory: float32(memoryInfo.RSS) / 1024 / 1024, // Convert memory to MB
		})
	}

	// Sort processes by CPU usage in descending order
	sortedByCPU := make([]struct {
		PID    int32
		Name   string
		CPU    float32
		Memory float32
	}, len(processInfo))
	copy(sortedByCPU, processInfo)
	for i := 0; i < len(sortedByCPU); i++ {
		for j := i + 1; j < len(sortedByCPU); j++ {
			if sortedByCPU[i].CPU < sortedByCPU[j].CPU {
				sortedByCPU[i], sortedByCPU[j] = sortedByCPU[j], sortedByCPU[i]
			}
		}
	}

	// Sort processes by memory usage in descending order
	sortedByMemory := make([]struct {
		PID    int32
		Name   string
		CPU    float32
		Memory float32
	}, len(processInfo))
	copy(sortedByMemory, processInfo)
	for i := 0; i < len(sortedByMemory); i++ {
		for j := i + 1; j < len(sortedByMemory); j++ {
			if sortedByMemory[i].Memory < sortedByMemory[j].Memory {
				sortedByMemory[i], sortedByMemory[j] = sortedByMemory[j], sortedByMemory[i]
			}
		}
	}

	// Log top N processes by CPU usage
	log.Debugf("\nTop processes by CPU usage:")
	for i := 0; i < int(math.Min(float64(topN), float64(len(sortedByCPU)))); i++ {
		log.Debugf("PID: %d, Name: %s, CPU: %.2f%%, Memory: %.2fMB", sortedByCPU[i].PID, sortedByCPU[i].Name, sortedByCPU[i].CPU, sortedByCPU[i].Memory)
	}

	// Log top N processes by Memory usage
	log.Debugf("Top processes by Memory usage:")
	for i := 0; i < int(math.Min(float64(topN), float64(len(sortedByMemory)))); i++ {
		log.Debugf("PID: %d, Name: %s, CPU: %.2f%%, Memory: %.2fMB", sortedByMemory[i].PID, sortedByMemory[i].Name, sortedByMemory[i].CPU, sortedByMemory[i].Memory)
	}
}

//const baselineDuration = 250 * time.Millisecond

// Initialize baseline once at startup
//var baselinePerf time.Duration

var (
	// Package-level variables
	baselinePerf       time.Duration
	initialSetupDone   sync.Once
	currentMaxParallel atomic.Int32
)

func benchmarkSystemPerformance(isInit bool) time.Duration {

	var cpuLoad float64
	var err error

	if isInit {
		cpuLoad, err = GetCPUUsage()

		if err != nil {
			cpuLoad = 50.0 // Default assumption if can't get CPU load
		}
	}

	start := time.Now()

	// Standard benchmark operation
	// For example, compute a hash of a fixed-size byte array
	// More intensive computation
	// More intensive computation
	data := make([]byte, 500000) // 500K array
	for j := 0; j < 20; j++ {    // 20 iterations
		for i := 0; i < 500000; i++ {
			data[i] = byte((i * j) % 256)
		}
		hash := sha256.Sum256(data)
		// Use the hash result to prevent compiler optimization
		data[0] = hash[0]

		// Add some memory allocation/deallocation work
		temp := make([]byte, 100000)
		for k := 0; k < 100000; k++ {
			temp[k] = byte(k ^ int(hash[k%32]))
		}
		data[1] = temp[0] // Prevent optimization
	}

	duration := time.Since(start)

	if isInit {
		/*const targetCpuLoad = 25.0 // Target "normal" CPU load for baseline

		// Avoid extreme adjustments
		if cpuLoad > 10.0 && cpuLoad < 95.0 {
			// Adjust the duration to what it would be at 25% CPU load
			// Use a conservative linear model
			scaleFactor := (100.0 - cpuLoad) / (100.0 - targetCpuLoad)
			adjustedDuration := time.Duration(float64(duration) * scaleFactor)

			log.Debugf("Initial system performance benchmark: %.2f ms (measured at %.2f%% CPU) → %.2f ms (normalized to %.2f%% CPU)",
				float64(duration)/float64(time.Millisecond),
				cpuLoad,
				float64(adjustedDuration)/float64(time.Millisecond),
				targetCpuLoad)

			return adjustedDuration
		} else {} */
		log.Debugf("Initial system performance benchmark: %.2f ms (measured at %.2f%% CPU)",
			float64(duration)/float64(time.Millisecond),
			cpuLoad)

	}

	return duration
}

// AdjustMaxParallel adjusts maxParallel based on performance.
func AdjustMaxParallel(currentWorkers int, maxParallel int) int {

	//avgTestDuration := calculateAverageDuration()
	currentPerf := benchmarkSystemPerformance(false)

	log.Debugf("Current system performance: %v (baseline: %v, ratio: %.2f)",
		currentPerf, baselinePerf, float64(currentPerf)/float64(baselinePerf))

	var adjustedParallel int

	if currentPerf > time.Duration(float64(baselinePerf)*2) { //2x should be indicator of degrading performance
		// Benchmark running significantly faster, can increase workers
		adjustedParallel = max(1, int(float64(currentWorkers)*0.75))
		if adjustedParallel != currentWorkers {
			log.Debugf("System slowdown detected, adjusting workers: %d → %d",
				currentWorkers, adjustedParallel)
		}
		return adjustedParallel
	} else if currentPerf < time.Duration(float64(baselinePerf)*1.4) { //should be indicator of available performance
		// Benchmark running significantly faster, can increase workers
		var numCPU = runtime.NumCPU()
		var increaseFactor = 1.25
		if currentWorkers <= numCPU/2 {
			increaseFactor = 2
		} else if currentWorkers <= numCPU {
			increaseFactor = 1.5
		}
		adjustedParallel = min(int(float64(currentWorkers)*increaseFactor), maxParallel) // Increase by 25%
		if adjustedParallel != currentWorkers {
			log.Debugf("System running faster than baseline, increasing workers: %d → %d",
				currentWorkers, adjustedParallel)
		}
		return adjustedParallel
	}

	return currentWorkers

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
		// Set up tracking for job completion
		jobStarted := true
		jobID := job.idx

		// Defer logging for premature termination - convert Name to string if needed
		defer func(started *bool, id int) {
			if *started {
				log.Debugf("ALERT: Worker %d terminated with job %d in progress",
					workerID, id)
			}
		}(&jobStarted, jobID)

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

		// Mark job as completed
		jobStarted = false
		log.Debugf("Worker %d finished test %s in %v", workerID, job.test.Name, duration)

		// Check if we should exit based on reduced worker count
		if int32(workerID) >= currentMaxParallel.Load() {
			log.Debugf("Worker %d stopping due to reduced worker count", workerID)
			break
		}
	}
}

func AdjustStartMaxParallel(maxParallel int) int {
	cpuLoad, err := GetCPUUsage()

	if err != nil {
		cpuLoad = 50.0 // Default assumption if can't get CPU load
	}

	if cpuLoad > 75 {
		log.Debugf("Adjusting intial worker count from %d to %d becuase of %.2f%% CPU load", maxParallel, max(1, int(float64(maxParallel)*0.5)), cpuLoad)
		return max(1, int(float64(maxParallel)*0.5))
	} else if cpuLoad > 50 {
		log.Debugf("Adjusting intial worker count from %d to %d becuase of %.2f%% CPU load", maxParallel, max(1, int(float64(maxParallel)*0.75)), cpuLoad)
		return max(1, int(float64(maxParallel)*0.75))
	}

	return maxParallel
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

	// Initialize the worker count and baseline performance only once
	initialSetupDone.Do(func() {
		adjustedValue := AdjustStartMaxParallel(maxParallel)
		currentMaxParallel.Store(int32(adjustedValue))
		baselinePerf = benchmarkSystemPerformance(true)
		log.Debugf("Initial setup completed - worker count: %d", currentMaxParallel.Load())
	})

	log.Debugf("Using worker count: %d", currentMaxParallel.Load())

	activeWorkers := atomic.Int32{}

	// Create a stop channel for the health check
	stopHealthCheck := make(chan struct{})
	healthCheckDone := make(chan struct{})

	// Create channels
	jobs := make(chan testJob, len(tests))
	results := make(chan testResult, len(tests))

	// Fill jobs channel
	for i, test := range tests {
		jobs <- testJob{test: test, idx: i}
	}
	close(jobs)

	// Start health check and worker management
	ticker := time.NewTicker(30 * time.Second)

	go func() {
		defer close(healthCheckDone)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				newCount := AdjustMaxParallel(int(activeWorkers.Load()), maxParallel)
				oldCount := int(currentMaxParallel.Load())

				if newCount != oldCount {
					log.Debugf("Adjusting worker count from %d to %d", oldCount, newCount)
					currentMaxParallel.Store(int32(newCount))

					if newCount > oldCount {
						for i := oldCount; i < newCount; i++ {
							activeWorkers.Add(1)
							go startWorker(i, jobs, results, &currentMaxParallel, &activeWorkers, xcresultPath, testResultDir)
						}
					}
				}
			case <-stopHealthCheck:
				log.Debugf("Health check goroutine stopping")
				return
			}
		}
	}()

	// Start initial worker pool
	for i := 0; i < int(currentMaxParallel.Load()); i++ {
		activeWorkers.Add(1)
		go startWorker(i, jobs, results, &currentMaxParallel, &activeWorkers, xcresultPath, testResultDir)
	}

	// Collect results with a safety check
	receivedResults := 0
	resultsComplete := false

	// Keep collecting results until we get all of them or determine it's impossible
	for receivedResults < len(tests) && !resultsComplete {
		select {
		case result := <-results:
			receivedResults++
			if result.err != nil {
				genTestSuiteErr = result.err
				log.Debugf("Test failed: %v", result.err)
			}
			testSuite.TestCases[result.idx] = result.testCase

		case <-time.After(10 * time.Second):
			activeCount := int(activeWorkers.Load())
			log.Debugf("Waiting for results: received %d/%d, active workers: %d",
				receivedResults, len(tests), activeCount)

			// If no active workers and we're still missing results, we have a problem
			if activeCount == 0 && receivedResults < len(tests) {
				log.Errorf("All workers exited but only received %d/%d results",
					receivedResults, len(tests))
				// Set flag to exit the outer loop
				resultsComplete = true
			}
		}
	}

	log.Debugf("Test suite [%s] complete - %d tests in %v", name, len(tests), time.Since(suiteStart))

	// Stop health check goroutine after all tests are done
	close(stopHealthCheck)

	// Wait for health check to finish cleanup
	<-healthCheckDone

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

	var testSummary ActionTestSummary
	var err error
	if test.TestStatus.Value == "Failure" {
		testSummary, err = test.loadActionTestSummary(xcresultPath)
		// Ignoring the SummaryNotFoundError error is on purpose because not having an action summary is a valid use case.
		// For example, failed tests will always have a summary, but successful ones might have it or might not.
		// If they do not have it, then that means that they did not log anything to the console,
		// and they were not executed as device configuration tests.
		if err != nil && !errors.Is(err, ErrSummaryNotFound) {
			return junit.TestCase{}, err
		}
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

	/* Commenting off export screenshots
	if err := test.exportScreenshots(xcresultPath, testResultDir); err != nil {
		return junit.TestCase{}, err
	}
	*/

	return junit.TestCase{
		Name:              test.Name.Value,
		ConfigurationHash: testSummary.Configuration.Hash,
		ClassName:         strings.Split(test.Identifier.Value, "/")[0],
		Failure:           failure,
		Skipped:           skipped,
		Time:              duartion,
	}, nil
}
