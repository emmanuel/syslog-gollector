package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/emmanuel/syslog-gollector/input"
	"github.com/emmanuel/syslog-gollector/output"

	"log"
)

// Program parameters
var adminIface string
var tcpIface string
var udpIface string
var kBrokers string
var kBatch int
var kTopic string
var kBufferTime int
var kBufferBytes int
var pEnabled bool
var cCapacity int

// Program resources
var tcpServer *input.TcpServer
var udpServer *input.UdpServer
var parser *input.Rfc5424Parser
var kafka *output.KafkaProducer

// Diagnostic data
var startTime time.Time

// Types
const (
	adminHost        = "localhost:8080"
	connTcpHost      = "localhost:514"
	connUdpHost      = "localhost:514"
	connType         = "tcp"
	kafkaBatch       = 10
	kafkaBrokers     = "localhost:9092"
	kafkaTopic       = "logs"
	kafkaBufferTime  = 1000
	kafkaBufferBytes = 512 * 1024
	parseEnabled     = true
	chanCapacity     = 0
)

func init() {
	flag.StringVar(&adminIface, "admin", adminHost, "Admin interface")
	flag.StringVar(&tcpIface, "tcp", connTcpHost, "TCP bind interface")
	flag.StringVar(&udpIface, "udp", connUdpHost, "UDP interface")
	flag.StringVar(&kBrokers, "broker", kafkaBrokers, "comma-delimited kafka brokers")
	flag.StringVar(&kTopic, "topic", kafkaTopic, "kafka topic")
	flag.IntVar(&kBatch, "batch", kafkaBatch, "Kafka batch size")
	flag.IntVar(&kBufferTime, "maxbuff", kafkaBufferTime, "Kafka client buffer max time (ms)")
	flag.IntVar(&kBufferBytes, "maxbytes", kafkaBufferBytes, "Kafka client buffer max bytes")
	flag.BoolVar(&pEnabled, "parse", parseEnabled, "enable syslog header parsing")
	flag.IntVar(&cCapacity, "chancap", chanCapacity, "channel buffering capacity")
}

// isPretty returns whether the HTTP response body should be pretty-printed.
func isPretty(req *http.Request) (bool, error) {
	err := req.ParseForm()
	if err != nil {
		return false, err
	}
	if _, ok := req.Form["pretty"]; ok {
		return true, nil
	}
	return false, nil
}

// ServeStatistics returns the statistics for the program
func ServeStatistics(w http.ResponseWriter, req *http.Request) {
	statistics := make(map[string]interface{})
	resources := map[string]input.Statistics{"tcp": tcpServer, "udp": udpServer, "parser": parser, "kafka": kafka}
	for k, v := range resources {
		s, err := v.GetStatistics()
		if err != nil {
			log.Println("failed to get " + k + " stats")
			http.Error(w, "failed to get "+k+" stats", http.StatusInternalServerError)
			return
		}
		statistics[k] = s
	}

	var b []byte
	var err error
	pretty, _ := isPretty(req)
	if pretty {
		b, err = json.MarshalIndent(statistics, "", "    ")
	} else {
		b, err = json.Marshal(statistics)
	}
	if err != nil {
		log.Println("failed to JSON marshal statistics map")
		http.Error(w, "failed to JSON marshal statistics map", http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func ServeDiagnostics(w http.ResponseWriter, req *http.Request) {
	diagnostics := make(map[string]string)
	diagnostics["started"] = startTime.String()
	diagnostics["uptime"] = time.Since(startTime).String()
	var b []byte
	pretty, _ := isPretty(req)
	if pretty {
		b, _ = json.MarshalIndent(diagnostics, "", "    ")
	} else {
		b, _ = json.Marshal(diagnostics)
	}
	w.Write(b)
}

func main() {
	flag.Parse()

	startTime = time.Now()

	hostname, err := os.Hostname()
	if err != nil {
		log.Println("unable to determine hostname -- aborting")
		os.Exit(1)
	}
	log.Printf("syslog server starting on %s, PID %d", hostname, os.Getpid())
	log.Printf("machine has %d cores", runtime.NumCPU())

	// Log config
	log.Printf("Admin server: %s", adminIface)
	log.Printf("kafka brokers: %s", kBrokers)
	log.Printf("kafka topic: %s", kTopic)
	log.Printf("kafka batch size: %d", kBatch)
	log.Printf("kafka buffer time: %dms", kBufferTime)
	log.Printf("kafka buffer bytes: %d", kBufferBytes)
	log.Printf("parsing enabled: %t", pEnabled)
	log.Printf("channel buffering capacity: %d", cCapacity)

	// Prep the channels
	rawChan := make(chan string, cCapacity)
	prodChan := make(chan string, cCapacity)

	if pEnabled {
		// Feed the input through the Parser stage
		parser = input.NewRfc5424Parser()
		prodChan, err = parser.StreamingParse(rawChan)
	} else {
		// work around issue in ServeStatistics when parser is nil
		parser = input.NewRfc5424Parser()
		// Pass the input directly to the output
		prodChan = rawChan
	}

	// Configure and start the Admin server
	http.HandleFunc("/statistics", ServeStatistics)
	http.HandleFunc("/diagnostics", ServeDiagnostics)
	go func() {
		err = http.ListenAndServe(adminIface, nil)
		if err != nil {
			fmt.Println("Failed to start admin server", err.Error())
			os.Exit(1)
		} else {
			log.Println("Admin server started")
		}
	}()

	// Connect to Kafka
	kafka, err = output.NewKafkaProducer(prodChan, strings.Split(kBrokers, ","), kTopic, kBufferTime, kBufferBytes)
	if err != nil {
		fmt.Println("Failed to create Kafka producer", err.Error())
		os.Exit(1)
	}
	log.Printf("connected to kafka at %s", kBrokers)
	go kafka.Start()

	// Start the event servers
	tcpServer = input.NewTcpServer(tcpIface)
	err = tcpServer.StartChan(rawChan)
	if err != nil {
		fmt.Println("Failed to start TCP server", err.Error())
		os.Exit(1)
	}
	log.Printf("listening on %s for TCP connections", tcpIface)

	udpServer = input.NewUdpServer(udpIface)
	err = udpServer.StartChan(rawChan)
	if err != nil {
		fmt.Println("Failed to start UDP server", err.Error())
		os.Exit(1)
	}
	log.Printf("listening on %s for UDP packets", udpIface)

	// Spin forever
	select {}
}
