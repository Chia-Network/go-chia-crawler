package main

import (
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/cmmarslender/go-chia-lib/pkg/protocols"
	"github.com/cmmarslender/go-chia-protocol/pkg/protocol"
	"github.com/schollz/progressbar/v3"
	"gopkg.in/go-playground/pool.v3"
)

const poolSize = 1000
const recrawlAfter = 1 * time.Hour
const respondPeersTimeout = 10 * time.Second

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

func main() {
	// init the timestamp map

	// All IPs we know about, and the best timestamp we've seen (from us or a peer)
	// map[ip]timestamp
	hostTimestamps = map[string]uint64{}

	// Whether we were able to connect to the IP when we tried
	// map[ip]successfulConnection
	attemptedIPs = map[string]bool{}

	// The last time we attempted to connect to this peer
	// map[ip]lastAttemptTime
	lastAttempts = map[string]time.Time{}

	// Create a pool of workers
	primaryPool := pool.NewLimited(poolSize)
	defer primaryPool.Close()

	initialHost := os.Args[1]

	load()

	initialBatch := primaryPool.Batch()
	if len(hostTimestamps) == 0 {
		log.Println("No peers in datafile, checking bootstrap peer")
		initialBatch.Queue(processPeers(initialHost, nil))
	} else {
		log.Println("Checking all peers from datafile")
		bar := progressbar.Default(int64(len(hostTimestamps)))
		for ip := range hostTimestamps {
			initialBatch.Queue(processPeers(ip, bar))
		}
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
			if _, ok := attemptedIPs[ip]; ok {
				delete(attemptedIPs, ip)
			}
			if _, ok := lastAttempts[ip]; ok {
				delete(lastAttempts, ip)
			}
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
			bar.Add(1)
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

	conn, err := protocol.NewConnection(&ip, protocol.WithHandshakeTimeout(5*time.Second))
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
		msg, err := conn.ReadOne()
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

	err = conn.RequestPeers()
	if err != nil {
		return err
	}

	// Really hacky way to set a deadline - should use channel instead probably
	ctx, cancel := context.WithTimeout(context.Background(), respondPeersTimeout)
	go func(ctx context.Context) {
		for {
			time.Sleep(1 * time.Second)
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					//log.Println("Closing connection. Took too long.")
					conn.Close()
				}
				return
			}
		}
	}(ctx)
	defer cancel()

	// Read until we get the respond_peers or it gets cancelled
	for {
		msg, err := conn.ReadOne()
		if err != nil {
			return err
		}

		if msg.ProtocolMessageType == protocols.ProtocolMessageTypeRespondPeers {
			return handlePeers(msg)
		}

		// @TODO possibly like.. give up if we dont get a respond peers after so many seconds
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
