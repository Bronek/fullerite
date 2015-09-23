package collector

import (
	"fullerite/config"
	"fullerite/metric"

	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	l "github.com/Sirupsen/logrus"
)

// This is a collector that will parse a config file on collection
// that is formatted in the standard nerve way. Then it will query
// all the local endpoints on the given path. The metrics are assumed
// to be in a UWSGI format.
//
// example configuration::
//
// {
//     	"heartbeat_path": "/var/run/nerve/heartbeat",
//		"instance_id": "srv1-devc",
//		"services": {
//	 		"<SERVICE_NAME>.<otherstuff>": {
//				"host": "<IPADDR>",
//      	    "port": ###,
//      	}
// 		}
//     "services": {
//
// Most imporantly is the port, host and service name. The service name is assumed to be formatted like this::
//
type releventNerveConfig struct {
	Services map[string]map[string]interface{}
}

// the assumed format is:
// {
// 	"gauges": {},
// 	"histograms": {},
// 	"version": "xxx",
// 	"timers": {
// 		"pyramid_uwsgi_metrics.tweens.status.metrics": {
// 			"count": ###,
// 			"p98": ###,
// 			...
// 		},
// 		"pyramid_uwsgi_metrics.tweens.lookup": {
// 			"count": ###,
// 			...
// 		}
// 	},
// 	"meters": {
// 		"pyramid_uwsgi_metrics.tweens.XXX": {
//			"count": ###,
//			"mean_rate": ###,
// 			"m1_rate": ###
// 		}
// 	},
// 	"counters": {
//		"myname": {
//			"count": ###,
// 	}
// }
type uwsgiJSONFormat struct {
	Counters   map[string]map[string]interface{}
	Gauges     map[string]map[string]interface{}
	Histograms map[string]map[string]interface{}
	Meters     map[string]map[string]interface{}
	Timers     map[string]map[string]interface{}
}

func newNerveUWSGICollector(channel chan metric.Metric, initialInterval int, log *l.Entry) *nerveUWSGICollector {
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

type nerveUWSGICollector struct {
	baseCollector

	configFilePath string
	queryPath      string
	timeout        int
}

func (inst nerveUWSGICollector) Collect() {
	ips, err := getIps()
	if err != nil {
		inst.log.Warn("Failed to get IPs: ", err)
		return
	}

	rawFileContents, err := ioutil.ReadFile(inst.configFilePath)
	if err != nil {
		inst.log.Warn("Failed to read the contents of file ", inst.configFilePath, " because ", err)
		return
	}

	servicePortMap, err := inst.parseNerveConfig(&rawFileContents, ips)
	if err != nil {
		inst.log.Warn("Failed to parse the nerve config at ", inst.configFilePath, ": ", err)
		return
	}
	inst.log.Debug("Finished parsing Nerve config into ", servicePortMap)

	for serviceName, port := range servicePortMap {

		endpoint := fmt.Sprintf("http://localhost:%d/%s", port, inst.queryPath)
		go inst.queryService(serviceName, endpoint)
	}
}

func (inst *nerveUWSGICollector) queryService(serviceName, endpoint string) {
	serviceLog := inst.log.WithField("service", serviceName)
	serviceLog.Debug("making GET request to ", endpoint)

	rawResponse, err := queryEndpoint(endpoint, inst.timeout)
	if err != nil {
		serviceLog.Warn("Failed to query endpoint ", endpoint, ": ", err)
		return
	}

	metrics, err := inst.parseUWSGIMetrics(&rawResponse)
	if err != nil {
		serviceLog.Warn("Failed to parse response into metrics: ", err)
		return
	}

	metric.AddToAll(&metrics, map[string]string{
		"collector": inst.Name(),
		"service":   serviceName,
	})

	serviceLog.Debug("Sending ", len(metrics), " to channel")
	for _, m := range metrics {
		inst.Channel() <- m
	}
}

func (col *nerveUWSGICollector) Configure(configMap map[string]interface{}) {
	if val, exists := configMap["queryPath"]; exists == true {
		col.queryPath = val.(string)
	}
	if val, exists := configMap["configFilePath"]; exists == true {
		col.configFilePath = val.(string)
	}

	col.configureCommonParams(configMap)
}

// parseNerveConfig is responsible for taking the JSON string coming in into a map of service:port
// it will also filter based on only services runnign on this host
func (inst nerveUWSGICollector) parseNerveConfig(raw *[]byte, ips []string) (map[string]int, error) {
	parsed := new(releventNerveConfig)

	// convert the ips into a map for membership tests
	ipMap := make(map[string]bool)
	for _, val := range ips {
		ipMap[val] = true
	}

	err := json.Unmarshal(*raw, parsed)
	if err != nil {
		return make(map[string]int), err
	}
	results := make(map[string]int)
	for rawServiceName, serviceConfig := range parsed.Services {
		host := strings.TrimSpace(serviceConfig["host"].(string))

		_, exists := ipMap[host]
		if exists == true {
			name := strings.Split(rawServiceName, ".")[0]
			port := config.GetAsInt(serviceConfig["port"], -1)
			if port != -1 {
				results[name] = port
			} else {
				inst.log.Warn("Failed to get port from value ", serviceConfig["port"])
			}
		}
	}

	return results, nil
}

// parseUWSGIMetrics takes the json returned from the endpoint and converts
// it into raw metrics. We first check that the metrics returned have a float value
// otherwise we skip the metric.
func (inst nerveUWSGICollector) parseUWSGIMetrics(raw *[]byte) ([]metric.Metric, error) {
	parsed := new(uwsgiJSONFormat)

	err := json.Unmarshal(*raw, parsed)
	if err != nil {
		return []metric.Metric{}, err
	}

	results := []metric.Metric{}
	appendIt := func(metrics []metric.Metric, typeDimVal string) {
		metric.AddToAll(&metrics, map[string]string{"type": typeDimVal})
		results = append(results, metrics...)
	}

	appendIt(convertToMetrics(&parsed.Gauges, metric.Gauge), "gauge")
	appendIt(convertToMetrics(&parsed.Meters, metric.Gauge), "meter")
	appendIt(convertToMetrics(&parsed.Counters, metric.Counter), "counter")
	appendIt(convertToMetrics(&parsed.Histograms, metric.Gauge), "histogram")
	appendIt(convertToMetrics(&parsed.Timers, metric.Gauge), "timer")

	return results, nil
}

// convertToMetrics takes in data formatted like this::
// "pyramid_uwsgi_metrics.tweens.4xx-response": {
// 		"count":     366116,
//		"mean_rate": 0.2333071157843687,
//		"m15_rate":  0.22693345170298124,
//		"units":     "events/second"
// }
// and outputs a metric for each name:rollup pair where the value is a float/int
// automatiically it appends the dimensions::
//		- rollup: the value in the nested map (e.g. "count", "mean_rate")
//		- collector: this collector's name
func convertToMetrics(metricMap *map[string]map[string]interface{}, metricType string) []metric.Metric {
	results := []metric.Metric{}

	for metricName, metricData := range *metricMap {
		for rollup, value := range metricData {
			m := metric.New(metricName)
			m.MetricType = metricType
			m.AddDimension("rollup", rollup)

			// only add things that have a numeric base
			switch value.(type) {
			case float64:
				m.Value = value.(float64)
				results = append(results, m)
			case int:
				m.Value = float64(value.(int))
				results = append(results, m)
			}
		}
	}

	return results
}

// getIps is responsible for getting all the ips that are associated with this NIC
func getIps() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{}, err
	}

	results := []string{}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			return []string{}, err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			results = append(results, ip.String())
		}
	}

	return results, nil
}

func queryEndpoint(endpoint string, timeout int) ([]byte, error) {
	client := http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}

	rsp, err := client.Get(endpoint)
	if err != nil {
		return []byte{}, err
	}

	txt, err := ioutil.ReadAll(rsp.Body)
	defer rsp.Body.Close()
	if err != nil {
		return []byte{}, err
	}

	return txt, nil
}
