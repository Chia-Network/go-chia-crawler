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
	"gopkg.in/go-playground/pool.v3"
)

var hostTimestampsLock sync.Mutex
var hostTimestamps map[string]uint64

var attemptedIPsLock sync.Mutex
var attemptedIPs map[string]bool

func main() {
	// init the timestamp map
	hostTimestamps = map[string]uint64{}
	attemptedIPs = map[string]bool{}

	// Create a pool of workers
	pool := pool.NewLimited(100)
	defer pool.Close()

	initialBatch := pool.Batch()
	initialBatch.Queue(processPeers("10.0.3.11"))
	initialBatch.QueueComplete()
	initialBatch.WaitAll()

	log.Println("initial batch complete")
	stats()

	for {
		batch := pool.Batch()
		hostTimestampsLock.Lock()
		for host := range hostTimestamps {
			batch.Queue(processPeers(host))
		}
		hostTimestampsLock.Unlock()
		batch.QueueComplete()
		batch.WaitAll()
		log.Println("Batch Complete")
		stats()
		time.Sleep(1*time.Second)
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

func processPeers(host string) pool.WorkFunc {
	return func(wu pool.WorkUnit) (interface{}, error) {
		// Skip canceled work units
		if wu.IsCancelled() {
			return nil, nil
		}

		err := requestPeersFrom(host)
		return nil, err
	}
}

func requestPeersFrom(host string) error {
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("unable to parse ip for host: %s", host)
	}
	conn, err := protocol.NewConnection(&ip, protocol.WithHandshakeTimeout(2 * time.Second))
	if err != nil {
		attemptedIPsLock.Lock()
		attemptedIPs[host] = false
		attemptedIPsLock.Unlock()
		return err
	}
	defer conn.Close()
	attemptedIPsLock.Lock()
	attemptedIPs[host] = true
	attemptedIPsLock.Unlock()

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
			break
		}
	}

	err = conn.RequestPeers()
	if err != nil {
		return err
	}

	// Really hacky way to set a deadline - should use channel instead probably
	ctx, cancel := context.WithTimeout(context.Background(), 5 * time.Second)
	go func(ctx context.Context) {
		for {
			time.Sleep(1*time.Second)
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					log.Println("Closing connection. Took too long.")
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
