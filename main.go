package main

import (
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/chia-network/go-chia-libs/pkg/peerprotocol"
	"github.com/chia-network/go-chia-libs/pkg/protocols"
	wrappedPrometheus "github.com/chia-network/go-modules/pkg/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/schollz/progressbar/v3"
	"gopkg.in/go-playground/pool.v3"
)

const poolSize = 1000
const recrawlAfter = 1 * time.Hour
const handshakeTimeout = 5 * time.Second
const respondPeersTimeout = 5 * time.Second

// Tracks the best "last connected" timestamp from either us or other peers on the network
var hostTimestampsLock sync.Mutex
var hostTimestamps map[string]uint64

// Tracks success/fail status for a given host as of the last attempt
var attemptedIPsLock sync.Mutex
var attemptedIPs map[string]bool

// Tracks the last connection attempt for a host
// Used to avoid trying peers too frequently
var lastAttemptsLock sync.Mutex
var lastAttempts map[string]time.Time

// Prometheus Metrics
// This holds a custom prometheus registry so that only our metrics are exported, and not the default go metrics
var prometheusRegistry *prometheus.Registry
var totalNodes5Days *wrappedPrometheus.LazyGauge
var ipv4Nodes5Days *wrappedPrometheus.LazyGauge
var ipv6Nodes5Days *wrappedPrometheus.LazyGauge

func main() {
	// All IPs we know about, and the best timestamp we've seen (from us or a peer)
	// map[ip]timestamp
	hostTimestamps = map[string]uint64{}

	// Whether we were able to connect to the IP when we tried
	// map[ip]successfulConnection
	attemptedIPs = map[string]bool{}

	// The last time we attempted to connect to this peer
	// map[ip]lastAttemptTime
	lastAttempts = map[string]time.Time{}

	prometheusRegistry = prometheus.NewRegistry()
	totalNodes5Days = newGauge("total_nodes_5_days", "Total number of nodes that have been gossiped around the network with a timestamp in the last 5 days. The crawler did not necessarily connect to all of these peers itself.")
	ipv4Nodes5Days = newGauge("ipv4_nodes_5_days", "Total number of IPv4 nodes that have been gossiped around the network with a timestamp in the last 5 days. The crawler did not necessarily connect to all of these peers itself.")
	ipv6Nodes5Days = newGauge("ipv6_nodes_5_days", "Total number of IPv6 nodes that have been gossiped around the network with a timestamp in the last 5 days. The crawler did not necessarily connect to all of these peers itself.")
	go func() {
		err := StartServer()
		if err != nil {
			log.Printf("Error starting prometheus server: %s\n", err.Error())
		}
	}()

	// Create a pool of workers
	primaryPool := pool.NewLimited(poolSize)
	defer primaryPool.Close()

	initialHost := os.Args[1]

	// Load peers from datafile and show stats (and update prom values) before requesting peers so we have data
	load()
	stats()

	initialBatch := primaryPool.Batch()
	if len(hostTimestamps) == 0 {
		log.Println("No peers in datafile, checking bootstrap peer")
		initialBatch.Queue(processPeers(initialHost, nil))
	} else {
		log.Println("Checking all peers from datafile")
		bar := progressbar.Default(int64(len(hostTimestamps)))
		hostTimestampsLock.Lock()
		lastAttemptsLock.Lock()
		for ip := range hostTimestamps {
			initialBatch.Queue(processPeers(ip, bar))
		}
		lastAttemptsLock.Unlock()
		hostTimestampsLock.Unlock()
	}
	initialBatch.QueueComplete()
	initialBatch.WaitAll()
	log.Println("initial batch complete")
	persist()
	stats()

	for {
		skippingTooRecent := []string{}
		crawling := []string{}

		log.Println()
		log.Println("Starting new batch...")
		batch := primaryPool.Batch()
		hostTimestampsLock.Lock()
		lastAttemptsLock.Lock()
		for host := range hostTimestamps {
			if lastAttempts[host].After(time.Now().Add(-recrawlAfter)) {
				skippingTooRecent = append(skippingTooRecent, host)
				continue
			}
			crawling = append(crawling, host)
		}
		lastAttemptsLock.Unlock()
		hostTimestampsLock.Unlock()

		if len(crawling) == 0 {
			log.Printf("No new peers to crawl (%d too recent). Sleeping for 1 minute...", len(skippingTooRecent))
			stats()
			time.Sleep(1 * time.Minute)
			continue
		}
		// Setup progress bar
		bar := progressbar.Default(int64(len(crawling)))
		for _, host := range crawling {
			batch.Queue(processPeers(host, bar))
		}
		batch.QueueComplete()
		batch.WaitAll()
		stats()
		log.Println("Batch Complete")
		persist()
		log.Println("Done")
	}
}

func stats() {
	last5Days := 0
	v6count := 0
	v4count := 0

	hostTimestampsLock.Lock()
	for ip, timestamp := range hostTimestamps {
		if int64(timestamp) < time.Now().Add(-5*time.Hour*24).Unix() {
			continue
		}
		last5Days++
		if isV6(ip) {
			v6count++
		} else {
			v4count++
		}
	}
	hostTimestampsLock.Unlock()

	attemptedIPsLock.Lock()
	success := 0
	failed := 0
	for _, successful := range attemptedIPs {
		if successful {
			success++
		} else {
			failed++
		}
	}
	attemptedIPsLock.Unlock()
	totalNodes5Days.Set(float64(last5Days))
	ipv4Nodes5Days.Set(float64(v4count))
	ipv6Nodes5Days.Set(float64(v6count))
	log.Printf("Have %d Total Peers. %d with timestamps last 5 days. %d Success. %d Failed.\n", len(hostTimestamps), last5Days, success, failed)
	log.Printf("IPv4 5 days: %d | IPv6 5 days: %d\n", v4count, v6count)
	time.Sleep(5 * time.Second)
}

type persistedRecord struct {
	IP            string
	BestTimestamp uint64
	LastAttempt   time.Time
}

func persist() {
	log.Println("Persisting peers to crawler.dat")
	// Iterate through the hostTimestamps, and for each host that meets the reporting threshold, save all the details about it

	file, err := os.Create("crawler.dat")
	if err != nil {
		log.Printf("Error writing data: %s\n", err.Error())
		return
	}
	enc := gob.NewEncoder(file)

	hostTimestampsLock.Lock()
	attemptedIPsLock.Lock()
	defer hostTimestampsLock.Unlock()
	defer attemptedIPsLock.Unlock()

	for ip, timestamp := range hostTimestamps {
		if int64(timestamp) < time.Now().Add(-5*time.Hour*24).Unix() {
			// Host was last seen over 5 days ago
			// Skipped from writing to the data file and clean up the maps
			delete(hostTimestamps, ip)
			delete(attemptedIPs, ip)
			delete(lastAttempts, ip)
			continue
		}

		record := persistedRecord{
			IP:            ip,
			BestTimestamp: timestamp,
			LastAttempt:   lastAttempts[ip],
		}
		err = enc.Encode(record)
		if err != nil {
			log.Printf("Error encoding: %s\n", err.Error())
		}
	}
}

func load() {
	log.Println("Checking for peers in crawler.dat")
	file, err := os.Open("crawler.dat")
	if err != nil {
		log.Printf("Error opening data file: %s\n", err.Error())
		return
	}
	dec := gob.NewDecoder(file)

	hostTimestampsLock.Lock()
	attemptedIPsLock.Lock()
	defer hostTimestampsLock.Unlock()
	defer attemptedIPsLock.Unlock()

	record := persistedRecord{}
	for {
		err = dec.Decode(&record)
		if err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("Error decoding: %s\n", err.Error())
			return
		}
		hostTimestamps[record.IP] = record.BestTimestamp
		attemptedIPs[record.IP] = false
		lastAttempts[record.IP] = record.LastAttempt
	}
}

func processPeers(host string, bar *progressbar.ProgressBar) pool.WorkFunc {
	return func(wu pool.WorkUnit) (interface{}, error) {
		// Skip canceled work units
		if wu.IsCancelled() {
			return nil, nil
		}

		err := requestPeersFrom(host)
		if bar != nil {
			_ = bar.Add(1)
		}
		return nil, err
	}
}

func requestPeersFrom(host string) error {
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("unable to parse ip for host: %s", host)
	}

	// Track the last time for this host
	lastAttemptsLock.Lock()
	lastAttempts[host] = time.Now()
	lastAttemptsLock.Unlock()

	// Assume failed by default
	// Set true once we get a handshake
	attemptedIPsLock.Lock()
	attemptedIPs[host] = false
	attemptedIPsLock.Unlock()

	conn, err := peerprotocol.NewConnection(&ip, peerprotocol.WithHandshakeTimeout(handshakeTimeout))
	if err != nil {
		return err
	}
	defer conn.Close()

	err = conn.Handshake()
	if err != nil {
		return err
	}

	// Read until we get the handshake ack
	for {
		msg, err := conn.ReadOne(handshakeTimeout)
		if err != nil {
			return err
		}

		if msg.ProtocolMessageType == protocols.ProtocolMessageTypeHandshake {
			// Handshake ack received, set this peer's timestamp to now and continue
			hostTimestampsLock.Lock()
			hostTimestamps[host] = uint64(time.Now().Unix())
			hostTimestampsLock.Unlock()

			attemptedIPsLock.Lock()
			attemptedIPs[host] = true
			attemptedIPsLock.Unlock()
			break
		}
	}

	fullNodeProtocol, err := peerprotocol.NewFullNodeProtocol(conn)
	if err != nil {
		return err
	}
	err = fullNodeProtocol.RequestPeers()
	if err != nil {
		return err
	}

	// Read until we get the respond_peers response
	for {
		msg, err := conn.ReadOne(respondPeersTimeout)
		if err != nil {
			return err
		}

		if msg.ProtocolMessageType == protocols.ProtocolMessageTypeRespondPeers {
			return handlePeers(msg)
		}
	}
}

func handlePeers(msg *protocols.Message) error {
	switch msg.ProtocolMessageType {
	case protocols.ProtocolMessageTypeRespondPeers:
		rp := &protocols.RespondPeers{}
		err := msg.DecodeData(rp)
		if err != nil {
			return err
		}

		hostTimestampsLock.Lock()
		for _, peer := range rp.PeerList {
			if _, ok := hostTimestamps[peer.Host]; !ok {
				hostTimestamps[peer.Host] = peer.Timestamp
				continue
			}
			if hostTimestamps[peer.Host] < peer.Timestamp {
				hostTimestamps[peer.Host] = peer.Timestamp
			}
		}
		hostTimestampsLock.Unlock()
	}

	return nil
}

func isV6(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.':
			return false
		case ':':
			return true
		}
	}
	return false
}

// newGauge returns a lazy gauge that follows naming conventions
func newGauge(name string, help string) *wrappedPrometheus.LazyGauge {
	opts := prometheus.GaugeOpts{
		Namespace: "chia",
		Subsystem: "crawler",
		Name:      name,
		Help:      help,
	}

	gm := prometheus.NewGauge(opts)

	lg := &wrappedPrometheus.LazyGauge{
		Gauge:    gm,
		Registry: prometheusRegistry,
	}

	return lg
}

// StartServer starts the metrics server
func StartServer() error {
	log.Printf("Starting metrics server on port %d", 9914)

	http.Handle("/metrics", promhttp.HandlerFor(prometheusRegistry, promhttp.HandlerOpts{}))
	return http.ListenAndServe(fmt.Sprintf(":%d", 9914), nil)
}
