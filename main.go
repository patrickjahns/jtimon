package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	auth_pb "github.com/nileshsimaria/jtimon/authentication"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
	viper "github.com/spf13/viper"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	// DefaultGRPCWindowSize is the default GRPC Window Size
	DefaultGRPCWindowSize = 1048576
	// MatchExpressionXpath is for the pattern matching the xpath and key-value pairs
	MatchExpressionXpath = "\\/([^\\/]*)\\[(.*?)+?(?:\\])"
	// MatchExpressionKey is for pattern matching the single and multiple key value pairs
	MatchExpressionKey = "([A-Za-z0-9-/]*)=(.*?)?(?:and|$)+"
)

var (
	cfgFile        = flag.StringSlice("config", make([]string, 0), "Config file name(s)")
	cfgFileList    = flag.String("config-file-list", "", "List of Config files")
	expConfig      = flag.Bool("explore-config", false, "Explore full config of JTIMON and exit")
	print          = flag.Bool("print", false, "Print Telemetry data")
	outJSON        = flag.Bool("json", false, "Convert telemetry packet into JSON")
	logMux         = flag.Bool("log-mux-stdout", false, "All logs to stdout")
	mr             = flag.Int64("max-run", 0, "Max run time in seconds")
	stateHandler   = flag.Bool("stats-handler", false, "Use GRPC statshandler")
	ver            = flag.Bool("version", false, "Print version and build-time of the binary and exit")
	compression    = flag.String("compression", "", "Enable HTTP/2 compression (gzip, deflate)")
	latencyProfile = flag.Bool("latency-profile", false, "Profile latencies. Place them in TSDB")
	prom           = flag.Bool("prometheus", false, "Stats for prometheus monitoring system")
	promPort       = flag.Int32("prometheus-port", 8090, "Prometheus port")
	prefixCheck    = flag.Bool("prefix-check", false, "Report missing __prefix__ in telemetry packet")
	apiControl     = flag.Bool("api", false, "Receive HTTP commands when running")
	pProf          = flag.Bool("pprof", false, "Profile JTIMON")
	pProfPort      = flag.Int32("pprof-port", 6060, "Profile port")
	gtrace         = flag.Bool("gtrace", false, "Collect GRPC traces")
	grpcHeaders    = flag.Bool("grpc-headers", false, "Add grpc headers in DB")

	version   = "version-not-available"
	buildTime = "build-time-not-available"
)

// JCtx is JTIMON run time context
type JCtx struct {
	config    Config
	file      string
	index     int
	wg        *sync.WaitGroup
	dMap      map[uint32]map[uint32]map[string]dropData
	influxCtx InfluxCtx
	stats     statsCtx
	pause     struct {
		pch   chan int64
		upch  chan struct{}
		subch chan bool
		logch chan bool
	}
}

type workerCtx struct {
	signalch chan os.Signal
	err      error
}

func configRead(jctx *JCtx, init bool) error {
	var err error
	viper.SetConfigFile(jctx.file)
	viper.SetConfigType("json")

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file, %s", err)
	}
	err = viper.Unmarshal(&jctx.config)
	if err != nil {
		fmt.Printf("\nConfig parsing error for %s: %v\n", jctx.file, err)
		return fmt.Errorf("config parsing (json Unmarshal) error for %s[%d]: %v", jctx.file, jctx.index, err)
	}

	if init {
		logInit(jctx)
		b, err := json.MarshalIndent(jctx.config, "", "    ")
		if err != nil {
			return fmt.Errorf("Config parsing error (json Marshal) for %s[%d]: %v", jctx.file, jctx.index, err)
		}
		jLog(jctx, fmt.Sprintf("\nRunning config of JTIMON:\n %s\n", string(b)))

		if init {
			jctx.pause.subch = make(chan bool)
			jctx.pause.logch = make(chan bool)

			go periodicStats(jctx)
			influxInit(jctx)
			dropInit(jctx)
			go apiInit(jctx)
		}

		if *grpcHeaders {
			pmap := make(map[string]interface{})
			for i := range jctx.config.Paths {
				pmap["path"] = jctx.config.Paths[i].Path
				pmap["reporting-rate"] = float64(jctx.config.Paths[i].Freq)
				addGRPCHeader(jctx, pmap)
			}
		}
	}

	return nil
}

// A worker function is the one who gets job done.
func worker(file string, idx int, wg *sync.WaitGroup) (chan os.Signal, error) {
	signalch := make(chan os.Signal)
	jctx := JCtx{
		file:  file,
		index: idx,
		wg:    wg,
		stats: statsCtx{
			startTime: time.Now(),
		},
	}

	err := configRead(&jctx, true)
	if err != nil {
		fmt.Println(err)
		return signalch, err
	}

	go func() {
		for {
			select {
			case sig := <-signalch:
				switch sig {
				case os.Interrupt:
					// Received Interrupt Signal, Stop the program
					printSummary(&jctx)
					fmt.Println("Signal handling")
					wg.Done()
				case syscall.SIGHUP:
					jctx.pause.subch <- true
					jctx.pause.logch <- true
					configRead(&jctx, false)
				case syscall.SIGCONT:
					go func() {
						var retry bool
						var opts []grpc.DialOption

						if jctx.config.TLS.CA != "" {
							certificate, _ := tls.LoadX509KeyPair(jctx.config.TLS.ClientCrt, jctx.config.TLS.ClientKey)

							certPool := x509.NewCertPool()
							bs, err := ioutil.ReadFile(jctx.config.TLS.CA)
							if err != nil {
								jLog(&jctx, fmt.Sprintf("[%d] Failed to read ca cert: %s\n", idx, err))
								return
							}

							ok := certPool.AppendCertsFromPEM(bs)
							if !ok {
								jLog(&jctx, fmt.Sprintf("[%d] Failed to append certs\n", idx))
								return
							}

							transportCreds := credentials.NewTLS(&tls.Config{
								Certificates: []tls.Certificate{certificate},
								ServerName:   jctx.config.TLS.ServerName,
								RootCAs:      certPool,
							})
							opts = append(opts, grpc.WithTransportCredentials(transportCreds))
						} else {
							opts = append(opts, grpc.WithInsecure())
						}

						if *stateHandler {
							opts = append(opts, grpc.WithStatsHandler(&statshandler{jctx: &jctx}))
						}

						if *compression != "" {
							var dc grpc.Decompressor
							if *compression == "gzip" {
								dc = grpc.NewGZIPDecompressor()
							} else if *compression == "deflate" {
								dc = newDEFLATEDecompressor()
							}
							compressionOpts := grpc.Decompressor(dc)
							opts = append(opts, grpc.WithDecompressor(compressionOpts))
						}

						ws := jctx.config.GRPC.WS
						opts = append(opts, grpc.WithInitialWindowSize(ws))

						hostname := jctx.config.Host + ":" + strconv.Itoa(jctx.config.Port)
						if hostname == ":0" {
							return
						}
					connect:
						if retry {
							jLog(&jctx, fmt.Sprintf("Reconnecting to %s", hostname))
						} else {
							jLog(&jctx, fmt.Sprintf("Connecting to %s", hostname))
						}
						conn, err := grpc.Dial(hostname, opts...)
						if err != nil {
							jLog(&jctx, fmt.Sprintf("[%d] Could not dial: %v\n", idx, err))
							time.Sleep(10 * time.Second)
							retry = true
							goto connect
						}

						if jctx.config.User != "" && jctx.config.Password != "" {
							user := jctx.config.User
							pass := jctx.config.Password
							if !jctx.config.Meta {
								lc := auth_pb.NewLoginClient(conn)
								dat, err := lc.LoginCheck(context.Background(), &auth_pb.LoginRequest{UserName: user, Password: pass, ClientId: jctx.config.CID})
								if err != nil {
									jLog(&jctx, fmt.Sprintf("[%d] Could not login: %v\n", idx, err))
									return
								}
								if !dat.Result {
									jLog(&jctx, fmt.Sprintf("[%d] LoginCheck failed", idx))
									return
								}
							}
						}

						subscribe(conn, &jctx)
						// Close the current connection and retry
						conn.Close()
						// If we are here we must try to reconnect again.
						// Reconnect after 10 seconds.
						time.Sleep(10 * time.Second)
						retry = true
						goto connect
					}()
				}
			}
		}
	}()

	fmt.Println("Returning from worker")
	return signalch, nil
}

func testMyCode() {
	var v int
	var wg sync.WaitGroup
	wg.Add(2)
	var m sync.Mutex
	m.Lock()
	go func() {
		v = 10
		m.Unlock()
		wg.Done()
	}()
	go func() {
		m.Lock()
		fmt.Println(v)
		wg.Done()
	}()
	wg.Wait()
}

func main() {
	if false {
		testMyCode()
		return
	}

	flag.Parse()
	if *pProf {
		go func() {
			addr := fmt.Sprintf("localhost:%d", *pProfPort)
			fmt.Println(http.ListenAndServe(addr, nil))
		}()
	}

	if *prom {
		go func() {
			addr := fmt.Sprintf("localhost:%d", promPort)
			http.Handle("/metrics", promhttp.Handler())
			fmt.Println(http.ListenAndServe(addr, nil))
		}()

	}
	startGtrace(*gtrace)

	fmt.Printf("Version: %s BuildTime %s\n", version, buildTime)
	if *ver {
		return
	}

	if *expConfig {
		config, err := ExploreConfig()
		if err == nil {
			fmt.Printf("\n%s\n", config)
		} else {
			fmt.Printf("Can not generate config\n")
		}
		return
	}

	err := GetConfigFiles(cfgFile, cfgFileList)
	if err != nil {
		fmt.Printf("Config parsing error: %s \n", err)
		return
	}

	n := len(*cfgFile)
	var wg sync.WaitGroup
	wg.Add(n)
	wList := make([]*workerCtx, n)

	for idx, file := range *cfgFile {
		signalch, err := worker(file, idx, &wg)
		if err != nil {
			wg.Done()
		}
		wList[idx] = &workerCtx{
			signalch: signalch,
			err:      err,
		}
	}

	// Start the Worked go routines which are waiting on the select loop
	for _, worker := range wList {
		if worker.err == nil {
			worker.signalch <- syscall.SIGCONT
		}
	}

	go func() {
		sigchan := make(chan os.Signal, 10)
		signal.Notify(sigchan, os.Interrupt, syscall.SIGHUP)
		for {
			s := <-sigchan
			switch s {
			case syscall.SIGHUP:
				for _, worker := range wList {
					if worker.err == nil {
						worker.signalch <- s
					}
				}
			case os.Interrupt:
				// Send the interrupt to the worker routines and
				// return
				for _, worker := range wList {
					if worker.err == nil {
						worker.signalch <- s
					}
				}
				return
			}
		}
	}()

	go func() {
		if *mr == 0 {
			return
		}
		tickChan := time.NewTimer(time.Second * time.Duration(*mr)).C
		<-tickChan
		for _, worker := range wList {
			if worker.err == nil {
				worker.signalch <- os.Interrupt
			}
		}
	}()
	wg.Wait()
	fmt.Printf("All done ... exiting!\n")
}
