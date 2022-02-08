package main

import (
	"context"
	"fmt"
	"log"
	"net"
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
	hostTimestamps = map[string]uint64{}
	attemptedIPs = map[string]bool{}
	lastAttempts = map[string]time.Time{}

	// Create a pool of workers
	primaryPool := pool.NewLimited(poolSize)
	defer primaryPool.Close()

	initialBatch := primaryPool.Batch()
	initialBatch.Queue(processPeers("10.0.3.11", nil))
	initialBatch.QueueComplete()
	initialBatch.WaitAll()

	log.Println("initial batch complete")
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

		// Setup progress bar
		bar := progressbar.Default(int64(len(crawling)))
		for _, host := range crawling {
			batch.Queue(processPeers(host, bar))
		}
		lastAttemptsLock.Unlock()
		hostTimestampsLock.Unlock()
		batch.QueueComplete()
		log.Printf("Queue complete. Crawling %d hosts. Skipping %d hosts.\n", len(crawling), len(skippingTooRecent))
		batch.WaitAll()
		stats()
		log.Println("Batch Complete")
	}
}

func stats() {
	last5Days := 0

	hostTimestampsLock.Lock()
	for _, timestamp := range hostTimestamps {
		if int64(timestamp) < time.Now().Add(-5 * time.Hour * 24).Unix() {
			continue
		}
		last5Days++
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
	time.Sleep(5 * time.Second)
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
	// Set truck once we get a handshake
	attemptedIPsLock.Lock()
	attemptedIPs[host] = false
	attemptedIPsLock.Unlock()

	conn, err := protocol.NewConnection(&ip, protocol.WithHandshakeTimeout(2 * time.Second))
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
			time.Sleep(1*time.Second)
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
