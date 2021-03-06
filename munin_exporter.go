package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	proto           = "tcp"
	retryInterval   = 1
	version_num     = "0.2"
	version_string  = "munin_exporter, version 0.2"
)

var (
	listeningAddress    = flag.String("listeningAddress", ":8080", "Address on which to expose Prometheus metrics.")
	listeningPath       = flag.String("listeningPath", "/metrics", "Path on which to expose Prometheus metrics.")
	metricPrefix        = flag.String("metricPrefix", "", "Metric prefix.")
	muninAddress        = flag.String("muninAddress", "localhost:4949", "munin-node address.")
	muninIgnore         = flag.String("muninIgnore", "", "List of plugin prefixes to ignore, comma separated.")
	muninScrapeInterval = flag.Int("muninScrapeInterval", 60, "Interval in seconds between scrapes.")
	version             = flag.Bool("version", false, "Show application version.")
	globalConn          net.Conn
	hostname            string
	graphs              []string
	gaugePerMetric      map[string]*prometheus.GaugeVec
	counterPerMetric    map[string]*muninCounter
	muninBanner         *regexp.Regexp
)

type muninCounter struct {
	counterDesc   *prometheus.Desc
	value         float64
	currentLabels []string

}

func (c *muninCounter) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.counterDesc
}

func (c *muninCounter) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		c.counterDesc,
		prometheus.CounterValue,
		c.value,
		c.currentLabels...,
	)
}

func (c *muninCounter) UpdateLabels(currentLabels []string, value float64) {
	c.value = value
	c.currentLabels = currentLabels
}

func newMuninCounter(metricName string, desc string, variableLabels []string, constlabels prometheus.Labels) *muninCounter {
	return &muninCounter{
		counterDesc: prometheus.NewDesc(
			metricName,
			desc,
			variableLabels,
			constlabels,
		),
	}
}

func init() {
	flag.Parse()
	if *version {
		fmt.Println(version_string)
		os.Exit(0)
	}
	var err error
	gaugePerMetric = map[string]*prometheus.GaugeVec{}
	counterPerMetric = map[string]*muninCounter{}
	muninBanner = regexp.MustCompile(`# munin node at (.*)`)

	err = connect()
	if err != nil {
		log.Fatalf("Could not connect to %s: %s", *muninAddress, err)
	}
}

func serveStatus() {
	http.Handle(*listeningPath, promhttp.Handler())
	http.ListenAndServe(*listeningAddress, nil)
}

func connect() (err error) {
	log.Printf("Connecting...")
	globalConn, err = net.Dial(proto, *muninAddress)
	if err != nil {
		return
	}
	log.Printf("connected!")

	reader := bufio.NewReader(globalConn)
	head, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	matches := muninBanner.FindStringSubmatch(head)
	if len(matches) != 2 { // expect: # munin node at <hostname>
		return fmt.Errorf("Unexpected line: %s", head)
	}
	hostname = matches[1]
	log.Printf("Found hostname: %s", hostname)
	return
}

func muninCommand(cmd string) (reader *bufio.Reader, err error) {
	reader = bufio.NewReader(globalConn)

	fmt.Fprintf(globalConn, cmd+"\n")

	_, err = reader.Peek(1)
	switch err {
	case io.EOF:
		log.Printf("not connected anymore, closing connection")
		globalConn.Close()
		for {
			err = connect()
			if err == nil {
				break
			}
			log.Printf("Couldn't reconnect: %s", err)
			time.Sleep(retryInterval * time.Second)
		}

		return muninCommand(cmd)
	case nil: //no error
		break
	default:
		log.Fatalf("Unexpected error: %s", err)
	}

	return
}

func muninList() (items []string, err error) {
	munin, err := muninCommand("list")
	if err != nil {
		log.Printf("couldn't get list")
		return
	}

	response, err := munin.ReadString('\n') // we are only interested in the first line
	if err != nil {
		log.Printf("couldn't read response")
		return
	}

	if response[0] == '#' { // # not expected here
		err = fmt.Errorf("Error getting items: %s", response)
		return
	}
	items = strings.Fields(strings.TrimRight(response, "\n"))
	return
}

func muninConfig(name string) (config map[string]map[string]string, graphConfig map[string]string, err error) {
	graphConfig = make(map[string]string)
	config = make(map[string]map[string]string)

	resp, err := muninCommand("config " + name)
	if err != nil {
		log.Printf("couldn't get config for %s", name)
		return
	}

	for {
		line, err := resp.ReadString('\n')
		if err == io.EOF {
			log.Fatalf("unexpected EOF, retrying")
			return muninConfig(name)
		}
		if err != nil {
			return nil, nil, err
		}
		if line == ".\n" { // munin end marker
			break
		}
		if line[0] == '#' { // here it's just a comment, so ignore it
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return nil, nil, fmt.Errorf("Line unexpected: %s", line)
		}
		key, value := parts[0], strings.TrimRight(strings.Join(parts[1:], " "), "\n")

		keyParts := strings.Split(key, ".")
		if len(keyParts) > 1 { // it's a metric config (metric.label etc)
			if _, ok := config[keyParts[0]]; !ok { //FIXME: is there no better way?
				config[keyParts[0]] = make(map[string]string)
			}
			config[keyParts[0]][keyParts[1]] = value
		} else {
			graphConfig[keyParts[0]] = value
		}
	}
	return
}

func registerMetrics() (err error) {
	items, err := muninList()
	if err != nil {
		return
	}

	ignoreList := strings.Split(*muninIgnore, ",")
	var skip bool

	for _, name := range items {
		skip = false
		for _, prefix := range ignoreList {
			if strings.HasPrefix(name, prefix) {
				skip = true
			}
		}

		if skip { continue }

		graphs = append(graphs, name)
		configs, graphConfig, err := muninConfig(name)
		if err != nil {
			return err
		}

		for metric, config := range configs {
			metricParts := []string{name, metric}
			if *metricPrefix != "" {
				metricParts = append([]string{*metricPrefix}, metricParts...)
			}
			metricName := strings.Replace(strings.Join(metricParts, "_"), "-", "_", -1)
			desc := graphConfig["graph_title"] + ": " + config["label"]
			if config["info"] != "" {
				desc = desc + ", " + config["info"]
			}
			muninType := strings.ToLower(config["type"])
			// muninType can be empty and defaults to gauge
			if muninType == "counter" || muninType == "derive" {
				gv := newMuninCounter(metricName, desc, []string{"hostname", "graphname", "muninlabel"}, prometheus.Labels{"type": muninType})
				log.Printf("Registered counter %s: %s", metricName, desc)
				counterPerMetric[metricName] = gv
				prometheus.Register(gv)

			} else {
				gv := prometheus.NewGaugeVec(
					prometheus.GaugeOpts{
						Name:        metricName,
						Help:        desc,
						ConstLabels: prometheus.Labels{"type": "gauge"},
					},
					[]string{"hostname", "graphname", "muninlabel"},
				)
				log.Printf("Registered gauge %s: %s", metricName, desc)
				gaugePerMetric[metricName] = gv
				prometheus.Register(gv)
			}
		}
	}
	// Built-in metrics
	buildInfoMetric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "munin_exporter_build_info",
			Help:        "Munin exporter build info",
			ConstLabels: prometheus.Labels{"type": "gauge"},
		},
		[]string{"goversion", "version_num"},
	)
	buildInfoMetric.WithLabelValues(runtime.Version(), version_num).Set(1)
	prometheus.Register(buildInfoMetric)
	muninMetricName := "munin_exporter_fetch_time"
	gv := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:	muninMetricName,
			Help:	"Time taken to fetch data from all registered munin plugins",
			ConstLabels: prometheus.Labels{"type": "gauge"},
		},
		[]string{"hostname"},
	)
	gaugePerMetric[muninMetricName] = gv
	prometheus.Register(gv)
	return nil
}

func fetchMetrics() (err error) {
	start := time.Now()
	for _, graph := range graphs {
		munin, err := muninCommand("fetch " + graph)
		if err != nil {
			return err
		}

		for {
			line, err := munin.ReadString('\n')
			line = strings.TrimRight(line, "\n")
			if err == io.EOF {
				log.Fatalf("unexpected EOF, retrying")
				return fetchMetrics()
			}
			if err != nil {
				return err
			}
			if len(line) == 1 && line[0] == '.' {
				log.Printf("End of list")
				break
			}

			parts := strings.Fields(line)
			if len(parts) != 2 {
				log.Printf("unexpected line: %s", line)
				continue
			}
			key, valueString := strings.Split(parts[0], ".")[0], parts[1]
			value, err := strconv.ParseFloat(valueString, 64)
			if err != nil {
				log.Printf("Couldn't parse value in line %s, malformed?", line)
				continue
			}
			metricParts := []string{graph, key}
			if *metricPrefix != "" {
				metricParts = append([]string{*metricPrefix}, metricParts...)
			}
			name := strings.Replace(strings.Join(metricParts, "_"), "-", "_", -1)
			log.Printf("%s: %f\n", name, value)
			_, isGauge := gaugePerMetric[name]
			if isGauge {
				gaugePerMetric[name].WithLabelValues(hostname, graph, key).Set(value)
				continue
			}
			_, isCounter := counterPerMetric[name]
			if isCounter {
				counterPerMetric[name].UpdateLabels([]string{hostname, graph, key}, value)
				continue
			}
		}
	}
	muninMetricName := "munin_exporter_fetch_time"
	gaugePerMetric[muninMetricName].WithLabelValues(hostname).Set(time.Since(start).Seconds())
	return
}

func main() {
	flag.Parse()
	err := registerMetrics()
	if err != nil {
		log.Fatalf("Could not register metrics: %s", err)
	}

	go serveStatus()

	func() {
		for {
			log.Printf("Scraping")
			err := fetchMetrics()
			if err != nil {
				log.Printf("Error occured when trying to fetch metrics: %s", err)
			}
			time.Sleep(time.Duration(*muninScrapeInterval) * time.Second)
		}
	}()
}
