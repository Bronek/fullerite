package collector

import (
	"fullerite/config"
	"fullerite/dropwizard"
	"fullerite/metric"
	"fullerite/util"

	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	l "github.com/Sirupsen/logrus"
)

const (
	// MetricTypeCounter String for counter metric type
	MetricTypeCounter string = "COUNTER"
	// MetricTypeGauge String for Gauge metric type
	MetricTypeGauge string = "GAUGE"
)

type nerveUWSGICollector struct {
	baseCollector

	configFilePath        string
	queryPath             string
	timeout               int
	servicesWhitelist     []string
	workersStatsEnabled   bool
	workersStatsBlacklist []string
}

func init() {
	RegisterCollector("NerveUWSGI", newNerveUWSGI)
}

// Default values of configuration fields
func newNerveUWSGI(channel chan metric.Metric, initialInterval int, log *l.Entry) Collector {
	col := new(nerveUWSGICollector)

	col.log = log
	col.channel = channel
	col.interval = initialInterval

	col.name = "NerveUWSGI"
	col.configFilePath = "/etc/nerve/nerve.conf.json"
	col.queryPath = "status/metrics"
	col.timeout = 2

	return col
}

// Rewrites config variables from the global config
func (n *nerveUWSGICollector) Configure(configMap map[string]interface{}) {
	if val, exists := configMap["queryPath"]; exists {
		n.queryPath = val.(string)
	}
	if val, exists := configMap["configFilePath"]; exists {
		n.configFilePath = val.(string)
	}
	if val, exists := configMap["servicesWhitelist"]; exists {
		n.servicesWhitelist = config.GetAsSlice(val)
	}
	if val, exists := configMap["workersStatsBlacklist"]; exists {
		n.workersStatsBlacklist = config.GetAsSlice(val)
	}
	if val, exists := configMap["workersStatsEnabled"]; exists {
		n.workersStatsEnabled = config.GetAsBool(val, false)
	}
	if val, exists := configMap["http_timeout"]; exists {
		n.timeout = config.GetAsInt(val, 2)
	}

	n.configureCommonParams(configMap)
}

// Parses nerve config from HTTP uWSGI stats endpoints
func (n *nerveUWSGICollector) Collect() {
	rawFileContents, err := ioutil.ReadFile(n.configFilePath)
	if err != nil {
		n.log.Warn("Failed to read the contents of file ", n.configFilePath, " because ", err)
		return
	}

	services, err := util.ParseNerveConfig(&rawFileContents, false)
	if err != nil {
		n.log.Warn("Failed to parse the nerve config at ", n.configFilePath, ": ", err)
		return
	}
	n.log.Debug("Finished parsing Nerve config into ", services)

	for _, service := range services {
		go n.queryService(service.Name, service.Port)
	}
}

// Fetches and computes stats from metrics HTTP endpoint,
// calls an additional endpoint if UWSGI is detected
func (n *nerveUWSGICollector) queryService(serviceName string, port int) {
	serviceLog := n.log.WithField("service", serviceName)

	endpoint := fmt.Sprintf("http://localhost:%d/%s", port, n.queryPath)
	serviceLog.Debug("making GET request to ", endpoint)

	serviceLog.Debug("making GET request to ", endpoint)
	rawResponse, schemaVer, err := queryEndpoint(endpoint, n.timeout)
	if err != nil {
		serviceLog.Warn("Failed to query endpoint ", endpoint, ": ", err)
		return
	}
	metrics, err := dropwizard.Parse(rawResponse, schemaVer, n.serviceInWhitelist(serviceName))
	if err != nil {
		serviceLog.Warn("Failed to parse response into metrics: ", err)
		return
	}
	// If we detect metrics from uwsgi, we try to fetch an additional Workers info
	// If this was a separate collector, there would be no way to figure that out
	// without a costly additional HTTP call.
	// This prevent us from having to maintain a whitelist of services to query
	// or from flooding all non UWSGI services with these requests.
	// We still maintain a blacklist (with wildcard support) just in case
	if strings.Contains(schemaVer, "uwsgi") && n.workersStatsEnabled && !n.serviceInWorkersStatsBlacklist(serviceName) {
		serviceLog.Debug("Trying to fetch workers stats")
		uwsgiWorkerStatsEndpoint := fmt.Sprintf("http://localhost:%d/%s", port, "status/uwsgi")
		uwsgiWorkerStatsMetrics := n.tryFetchUWSGIWorkersStats(serviceName, uwsgiWorkerStatsEndpoint)
		if uwsgiWorkerStatsMetrics != nil {
			// Add the metrics to our existing ones so we get the post process for free.
			serviceLog.Debug("Additional workers metrics collected: ", len(uwsgiWorkerStatsMetrics))
			for k, v := range uwsgiWorkerStatsMetrics {
				metrics[k] = v
			}
		}
	}

	metric.AddToAll(&metrics, map[string]string{
		"service": serviceName,
		"port":    strconv.Itoa(port),
	})
	serviceLog.Debug("Sending ", len(metrics), " to channel")
	for _, m := range metrics {
		if !n.ContainsBlacklistedDimension(m.Dimensions) {
			n.Channel() <- m
		}
	}
}

func queryEndpoint(endpoint string, timeout int) ([]byte, string, error) {
	client := http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}

	rsp, err := client.Get(endpoint)

	if rsp != nil {
		defer func() {
			io.Copy(ioutil.Discard, rsp.Body)
			rsp.Body.Close()
		}()
	}

	if err != nil {
		return []byte{}, "", err
	}

	if rsp != nil && rsp.StatusCode != 200 {
		err := fmt.Errorf("%s returned %d error code", endpoint, rsp.StatusCode)
		return []byte{}, "", err
	}

	schemaVer := rsp.Header.Get("Metrics-Schema")
	if schemaVer == "" {
		schemaVer = "default"
	}

	txt, err := ioutil.ReadAll(rsp.Body)
	if err != nil {
		return []byte{}, "", err
	}

	return txt, schemaVer, nil
}

// serviceInWhitelist returns true if the service name passed as argument
// is found among the ones whitelisted by the user
func (n *nerveUWSGICollector) serviceInWhitelist(service string) bool {
	for _, s := range n.servicesWhitelist {
		if s == service {
			return true
		}
	}
	return false
}

// serviceInWhitelist returns true if the service name passed as argument
// is found among the ones whitelisted by the user
func (n *nerveUWSGICollector) serviceInWorkersStatsBlacklist(service string) bool {
	for _, s := range n.workersStatsBlacklist {
		if s == service {
			return true
		}
	}
	return false
}

// Fetches and computes status stats from an HTTP endpoint
func (n *nerveUWSGICollector) tryFetchUWSGIWorkersStats(serviceName string, endpoint string) []metric.Metric {
	serviceLog := n.log.WithField("service", serviceName)
	serviceLog.Debug("making GET request to ", endpoint)
	rawResponse, _, err := queryEndpoint(endpoint, n.timeout)
	if err != nil {
		serviceLog.Info("Failed to query endpoint ", endpoint, ": ", err)
		return nil
	}
	metrics, err := parseUWSGIWorkersStats(rawResponse)
	if err != nil {
		serviceLog.Info("No workers stats retreived: ", err)
		return nil
	}
	return metrics
}

// Counts workers status stats from JSON content
func parseUWSGIWorkersStats(raw []byte) ([]metric.Metric, error) {
	result := make(map[string]interface{})
	err := json.Unmarshal(raw, &result)
	results := []metric.Metric{}
	if err != nil {
		return results, err
	}
	registry := make(map[string]int)
	registry["IdleWorkers"] = 0
	// Initialize this one to 1 because the collector uses one worker
	registry["BusyWorkers"] = 1

	// Let's not initialize these are they are mostly 0
	// registry["SigWorkers"] = 0
	// registry["PauseWorkers"] = 0
	// registry["CheapWorkers"] = 0
	// registry["UnknownStateWorkers"] = 0
	workers, ok := result["workers"].([]interface{})
	if !ok {
		return results, fmt.Errorf("\"workers\" field not found or not an array")
	}
	for _, worker := range workers {
		workerMap, ok := worker.(map[string]interface{})
		if !ok {
			return results, fmt.Errorf("worker record is not a map")
		}
		status, ok := workerMap["status"].(string)
		if !ok {
			return results, fmt.Errorf("status not found or not a string")
		}
		if strings.Index(status, "sig") == 0 {
			status = "sig"
		}
		metricName := strings.Title(status) + "Workers"
		_, exists := registry[metricName]
		if !exists {
			registry[metricName] = 0
		}
		registry[metricName]++
	}
	for key, value := range registry {
		results = append(results, metric.WithValue(key, float64(value)))
	}
	return results, err
}
