package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bitcoin-black/accept-nano/nano"
	"github.com/cenkalti/log"
	bolt "github.com/coreos/bbolt"
	"github.com/ulule/limiter"
	"github.com/ulule/limiter/drivers/store/memory"
)

const paymentsBucket = "payments"

var (
	Version           = ""
	generateSeed      = flag.Bool("seed", false, "generate a seed and exit")
	configPath        = flag.String("config", "config.toml", "config file path")
	version           = flag.Bool("version", false, "display version and exit")
	config            Config
	db                *bolt.DB
	server            http.Server
	rateLimiter       *limiter.Limiter
	node              *nano.Node
	stopCheckPayments = make(chan struct{})
	checkPaymentWG    sync.WaitGroup
)

func main() {
	if Version == "" {
		Version = "v0.0.0"
	}

	flag.Parse()

	if *version {
		fmt.Println(Version)
		return
	}

	if *generateSeed {
		seed, err := NewSeed()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(seed)
		return
	}

	err := config.Read()
	if err != nil {
		log.Fatal(err)
	}

	if config.EnableDebugLog {
		log.SetLevel(log.DEBUG)
	}

	if config.CoinmarketcapAPIKey == "" {
		log.Warning("empty CoinmarketcapAPIKey in config, fiat conversions will not work")
	}

	rate, err := limiter.NewRateFromFormatted(config.RateLimit)
	if err != nil {
		log.Fatal(err)
	}

	rateLimiter = limiter.New(memory.NewStore(), rate)
	node = nano.New(config.NodeURL, config.NodeAuthUsername, config.NodeAuthPassword)
	node.SetTimeout(time.Duration(config.NodeTimeout) * time.Millisecond)

	log.Debugln("opening db:", config.DatabasePath)
	db, err = bolt.Open(config.DatabasePath, 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	log.Debugln("db has been opened successfully")

	err = db.Update(func(tx *bolt.Tx) error {
		_, txErr := tx.CreateBucketIfNotExists([]byte(paymentsBucket))
		return txErr
	})
	if err != nil {
		log.Fatal(err)
	}

	// Check existing payments.
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(paymentsBucket))
		return b.ForEach(func(k, v []byte) error {
			payment, err2 := LoadPayment(k)
			if err != nil {
				log.Error(err2)
				return nil
			}
			payment.StartChecking()
			return nil
		})
	})
	if err != nil {
		log.Fatal(err)
	}

	go runServer()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	close(stopCheckPayments)

	shutdownTimeout := time.Duration(config.ShutdownTimeout) * time.Millisecond
	log.Noticeln("shutting down with timeout:", shutdownTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	err = server.Shutdown(ctx)
	if err != nil {
		log.Errorln("shutdown error:", err)
	}

	checkPaymentWG.Wait()

	err = db.Close()
	if err != nil {
		log.Fatal(err)
	}
}
