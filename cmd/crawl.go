package cmd

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/chia-network/go-chia-libs/pkg/peerprotocol"
	"github.com/chia-network/go-chia-libs/pkg/protocols"
	"github.com/chia-network/go-chia-libs/pkg/types"
	"github.com/chia-network/go-chia-libs/pkg/util"
	wrappedPrometheus "github.com/chia-network/go-modules/pkg/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/go-playground/pool.v3"
)

const poolSize = 1000
const handshakeTimeout = 5 * time.Second

// Prometheus Metrics
// This holds a custom prometheus registry so that only our metrics are exported, and not the default go metrics
var prometheusRegistry *prometheus.Registry
var nonChip13Height *wrappedPrometheus.LazyGauge

// crawlCmd represents the crawl command
var crawlCmd = &cobra.Command{
	Use:   "crawl",
	Short: "Runs the crawler and optional metrics server",
	Run: func(cmd *cobra.Command, args []string) {
		prometheusRegistry = prometheus.NewRegistry()
		nonChip13Height = newGauge("non_chip_13_node_height", "Height of non chip-13 chain")
		if viper.GetBool("metrics") {
			go func() {
				err := StartServer()
				if err != nil {
					log.Printf("Error starting prometheus server: %s\n", err.Error())
				}
			}()
		}

		// Create a pool of workers
		primaryPool := pool.NewLimited(poolSize)
		defer primaryPool.Close()

		initialHost := viper.GetString("bootstrap-peer")

		for {
			batch := primaryPool.Batch()
			batch.Queue(processPeers(initialHost))
			batch.QueueComplete()
			batch.WaitAll()
			log.Println("sleeping for 1 minute before checking again")
			time.Sleep(1 * time.Minute)
		}
	},
}

func init() {
	rootCmd.AddCommand(crawlCmd)
}

func processPeers(host string) pool.WorkFunc {
	return func(wu pool.WorkUnit) (interface{}, error) {
		// Skip canceled work units
		if wu.IsCancelled() {
			return nil, nil
		}

		err := requestHeightFrom(host)
		if err != nil {
			log.Println(err.Error())
		}
		return nil, err
	}
}

func requestHeightFrom(host string) error {
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("unable to parse ip for host: %s", host)
	}

	conn, err := peerprotocol.NewConnection(&ip, peerprotocol.WithHandshakeTimeout(handshakeTimeout))
	if err != nil {
		return err
	}
	defer conn.Close()

	err = conn.Handshake()
	if err != nil {
		return err
	}

	// Read until we get a new_peak
	for {
		msg, err := conn.ReadOne(handshakeTimeout)
		if err != nil {
			return err
		}

		log.Printf("Message type %d received\n", msg.ProtocolMessageType)
		if msg.ProtocolMessageType == protocols.ProtocolMessageTypeHandshake {
			log.Println("Got Handshake ACK")
		}
		if msg.ProtocolMessageType == 20 {
			log.Println("Got new peak")

			data := msg.Data
			_, data, err := util.ShiftNBytes(32, data)
			if err != nil {
				return fmt.Errorf("error shifting first 32 bytes: %s", err.Error())
			}
			heightBytes, data, err := util.ShiftNBytes(4, data)
			if err != nil {
				return fmt.Errorf("error shifting 4 bytes for height: %s", err.Error())
			}
			weightBytes, _, err := util.ShiftNBytes(16, data)
			if err != nil {
				return fmt.Errorf("error shifting 16 bytes for weight: %s", err.Error())
			}

			newInt := util.BytesToUint32(heightBytes)
			log.Printf("Height is %d\n", newInt)
			newWeight := types.Uint128FromBytes(weightBytes)
			log.Printf("Weight is %s\n", newWeight.String())
			nonChip13Height.Set(float64(newInt))
			break
		}
	}

	return nil
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
	metricsPort := viper.GetInt("metrics-port")
	log.Printf("Starting metrics server on port %d", metricsPort)

	http.Handle("/metrics", promhttp.HandlerFor(prometheusRegistry, promhttp.HandlerOpts{}))
	return http.ListenAndServe(fmt.Sprintf(":%d", metricsPort), nil)
}
