package dropwizard

import (
	"fullerite/metric"
	"regexp"

	l "github.com/Sirupsen/logrus"
)

var defaultLog = l.WithFields(l.Fields{"app": "fullerite", "pkg": "dropwizard"})

const (
	// MetricTypeCounter String for counter metric type
	MetricTypeCounter string = "COUNTER"
	// MetricTypeGauge String for Gauge metric type
	MetricTypeGauge string = "GAUGE"
)

// Parser is an interface for dropwizard parsers
type Parser interface {
	Parse() ([]metric.Metric, error)
	// take actual value and convert it to metric object
	createMetricFromDatam(string, interface{}, string, string) (metric.Metric, bool)
	// take map of data and extract metrics
	metricFromMap(map[string]interface{}, string, string) []metric.Metric
	// take map of maps and extract metrics, this is like first level of parsing
	parseMapOfMap(map[string]map[string]interface{}, string) []metric.Metric
	// is Cumulative Counter enabled for this metric
	isCCEnabled() bool
}

// Format defines format in which dropwizard metrics are emitted
//
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
type Format struct {
	ServiceDims map[string]interface{} `json:"service_dims"`
	Counters    map[string]map[string]interface{}
	Gauges      map[string]map[string]interface{}
	Histograms  map[string]map[string]interface{}
	Meters      map[string]map[string]interface{}
	Timers      map[string]map[string]interface{}
}

// BaseParser is a base struct for real parsers
type BaseParser struct {
	data      []byte
	log       *l.Entry
	ccEnabled bool // Enable cumulative counters
	schemaVer string
}

// Parse can be called from collector code to parse results
func Parse(raw []byte, schemaVer string, ccEnabled bool) ([]metric.Metric, error) {
	var parser Parser
	if schemaVer == "uwsgi.1.0" || schemaVer == "uwsgi.1.1" {
		parser = NewUWSGIMetric(raw, schemaVer, ccEnabled)
	} else if schemaVer == "java-1.1" {
		parser = NewJavaMetric(raw, schemaVer, ccEnabled)
	} else {
		parser = NewLegacyMetric(raw, schemaVer, ccEnabled)
	}
	return parser.Parse()
}

// metricFromMap takes in flattened maps formatted like this::
// {
//    "count":      3443,
//    "mean_rate": 100
// }
// and metricname and metrictype and returns metrics for each name:rollup pair
func (parser *BaseParser) metricFromMap(metricMap map[string]interface{},
	metricName string,
	metricType string) []metric.Metric {
	results := []metric.Metric{}
	dims := make(map[string]string)

	for rollup, value := range metricMap {
		// First check for dimension set if present
		// See uwsgi_metric.go:68 for explanation on the range over value
		if rollup == "dimensions" {
			for dimName, dimVal := range value.(map[string]interface{}) {
				// Handle nil valued dimensions
				if strVal, ok := dimVal.(string); ok {
					dims[dimName] = strVal
				} else {
					dims[dimName] = "null"
				}
			}
			continue
		}

		mName := metricName
		mType := metricType
		matched, _ := regexp.MatchString("m[0-9]+_rate", rollup)

		// If cumulCounterEnabled is true:
		//		1. change metric type meter.count and timer.count moving them to cumulative counter
		//		2. don't send back metered metrics (rollup == 'mXX_rate')
		if parser.ccEnabled && matched {
			continue
		}
		if parser.ccEnabled && rollup != "value" {
			mName = metricName + "." + rollup
			if rollup == "count" {
				mType = metric.CumulativeCounter
			}
		}
		tempMetric, ok := parser.createMetricFromDatam(rollup, value, mName, mType)
		if ok {
			results = append(results, tempMetric)
		}
	}

	metric.AddToAll(&results, dims)
	return results
}

func (parser *BaseParser) isCCEnabled() bool {
	return parser.ccEnabled
}

// createMetricFromDatam takes in rollup, value, metricName, metricType and returns metric only if
// value was numeric
func (parser *BaseParser) createMetricFromDatam(rollup string,
	value interface{},
	metricName string, metricType string) (metric.Metric, bool) {
	m := metric.New(metricName)
	m.MetricType = metricType
	m.AddDimension("rollup", rollup)

	// only add things that have a numeric base
	switch value.(type) {
	case float64:
		m.Value = value.(float64)
	case int:
		m.Value = float64(value.(int))
	default:
		return m, false
	}
	return m, true
}

func extractParsedMetric(parser Parser, parsed *Format) []metric.Metric {
	results := []metric.Metric{}
	appendIt := func(metrics []metric.Metric, typeDimVal string) {
		if !parser.isCCEnabled() {
			metric.AddToAll(&metrics, map[string]string{"type": typeDimVal})
		}
		results = append(results, metrics...)
	}

	appendIt(parser.parseMapOfMap(parsed.Gauges, metric.Gauge), "gauge")
	appendIt(parser.parseMapOfMap(parsed.Counters, metric.Counter), "counter")
	appendIt(parser.parseMapOfMap(parsed.Histograms, metric.Gauge), "histogram")
	appendIt(parser.parseMapOfMap(parsed.Meters, metric.Gauge), "meter")
	appendIt(parser.parseMapOfMap(parsed.Timers, metric.Gauge), "timer")

	return results
}

func (parser *BaseParser) parseMapOfMap(
	metricMap map[string]map[string]interface{},
	metricType string) []metric.Metric {
	return []metric.Metric{}
}

// Parse is just a placehoder function
func (parser *BaseParser) Parse() ([]metric.Metric, error) {
	return []metric.Metric{}, nil
}
