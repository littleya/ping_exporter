package main

import (
	"context"
	"fmt"
    "io/ioutil"
	"net"
	"net/http"
    "net/url"
	"os"
	"strings"
	"time"

	"github.com/czerwonk/ping_exporter/config"
	"github.com/digineo/go-ping"
	mon "github.com/digineo/go-ping/monitor"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"gopkg.in/alecthomas/kingpin.v2"
)

const version string = "0.4.5"

var (
	showVersion          = kingpin.Flag("version", "Print version information").Default().Bool()
	listenAddress        = kingpin.Flag("web.listen-address", "Address on which to expose metrics and web interface").Default(":9427").String()
	metricsPath          = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics").Default("/metrics").String()
	configFile           = kingpin.Flag("config.path", "Path to config file").Default("").String()
	pingInterval         = kingpin.Flag("ping.interval", "Interval for ICMP echo requests").Default("5s").Duration()
	pingTimeout          = kingpin.Flag("ping.timeout", "Timeout for ICMP echo request").Default("4s").Duration()
	pingSize             = kingpin.Flag("ping.size", "Payload size for ICMP echo requests").Default("56").Uint16()
	historySize          = kingpin.Flag("ping.history-size", "Number of results to remember per target").Default("10").Int()
	dnsRefresh           = kingpin.Flag("dns.refresh", "Interval for refreshing DNS records and updating targets accordingly (0 if disabled)").Default("1m").Duration()
	dnsNameServer        = kingpin.Flag("dns.nameserver", "DNS server used to resolve hostname of targets").Default("").String()
	logLevel             = kingpin.Flag("log.level", "Only log messages with the given severity or above. Valid levels: [debug, info, warn, error, fatal]").Default("info").String()
	targets              = kingpin.Arg("targets", "A list of targets to ping").Strings()
	remoteTargets        = kingpin.Flag("remote-targets", "A list of remote targets url").Strings()
	updateRemoteInterval = kingpin.Flag("update-remote-interval", "Interval for update remote targets").Default("30s").Duration()
)

var (
	enableDeprecatedMetrics = true // default may change in future
	deprecatedMetrics       = kingpin.Flag("metrics.deprecated", "Enable or disable deprecated metrics (`ping_rtt_ms{type=best|worst|mean|std_dev}`). Valid choices: [enable, disable]").Default("enable").String()
)

var allTargets []*target

func init() {
	kingpin.Parse()
}

func main() {
	if *showVersion {
		printVersion()
		os.Exit(0)
	}

	err := log.Logger.SetLevel(log.Base(), *logLevel)
	if err != nil {
		log.Errorln(err)
		os.Exit(1)
	}

	switch *deprecatedMetrics {
	case "enable":
		enableDeprecatedMetrics = true
	case "disable":
		enableDeprecatedMetrics = false
	default:
		kingpin.FatalUsage("metrics.deprecated must be `enable` or `disable`")
	}

	if mpath := *metricsPath; mpath == "" {
		log.Warnln("web.telemetry-path is empty, correcting to `/metrics`")
		mpath = "/metrics"
		metricsPath = &mpath
	} else if mpath[0] != '/' {
		mpath = "/" + mpath
		metricsPath = &mpath
	}

	cfg, err := loadConfig()
	if err != nil {
		kingpin.FatalUsage("could not load config.path: %v", err)
	}

	if cfg.Ping.History < 1 {
		kingpin.FatalUsage("ping.history-size must be greater than 0")
	}

	if cfg.Ping.Size < 0 || cfg.Ping.Size > 65500 {
		kingpin.FatalUsage("ping.size must be between 0 and 65500")
	}

	if len(cfg.Targets) == 0 {
		kingpin.FatalUsage("No targets specified")
	}

	m, err := startMonitor(cfg)
	if err != nil {
		log.Errorln(err)
		os.Exit(2)
	}

	startServer(m)
}

func printVersion() {
	fmt.Println("ping-exporter")
	fmt.Printf("Version: %s\n", version)
	fmt.Println("Author(s): Philip Berndroth, Daniel Czerwonk")
	fmt.Println("Metric exporter for go-icmp")
}

func startMonitor(cfg *config.Config) (*mon.Monitor, error) {
	resolver := setupResolver(cfg)
	var bind4, bind6 string
	if ln, err := net.Listen("tcp4", "127.0.0.1:0"); err == nil {
		//ipv4 enabled
		ln.Close()
		bind4 = "0.0.0.0"
	}
	if ln, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		//ipv6 enabled
		ln.Close()
		bind6 = "::"
	}
	pinger, err := ping.New(bind4, bind6)
	if err != nil {
		return nil, err
	}

	if pinger.PayloadSize() != cfg.Ping.Size {
		pinger.SetPayloadSize(cfg.Ping.Size)
	}

	monitor := mon.New(pinger,
		cfg.Ping.Interval.Duration(),
		cfg.Ping.Timeout.Duration())
	monitor.HistorySize = cfg.Ping.History

    remoteTargets := fetchRemoteTargets(cfg)
    cfg.Targets = append(cfg.Targets, remoteTargets...)

	// targets := make([]*target, len(cfg.Targets))
    allTargets = make([]*target, len(cfg.Targets))
	for i, host := range cfg.Targets {
		t := &target{
			host:      host,
			addresses: make([]net.IPAddr, 0),
			delay:     time.Duration(10*i) * time.Millisecond,
			resolver:  resolver,
		}
		allTargets[i] = t

		err := t.addOrUpdateMonitor(monitor)
		if err != nil {
			log.Errorln(err)
		}
	}

    go syncRemoteTargets(cfg)
	go startDNSAutoRefresh(cfg.DNS.Refresh.Duration(), monitor, cfg)

	return monitor, nil
}

func startDNSAutoRefresh(interval time.Duration, monitor *mon.Monitor, cfg *config.Config) {
	if interval <= 0 {
		return
	}

	for range time.NewTicker(interval).C {
		refreshDNS(monitor, cfg)
	}
}

func refreshDNS(monitor *mon.Monitor, cfg *config.Config) {
	log.Infoln("refreshing DNS")
	for _, t := range allTargets {
		go func(ta *target) {
			err := ta.addOrUpdateMonitor(monitor)
			if err != nil {
				log.Errorf("could refresh dns: %v", err)
			}
		}(t)
	}
}

func syncRemoteTargets(cfg *config.Config) {
    for {
        resolver := setupResolver(cfg)
        remoteTargets := fetchRemoteTargets(cfg)
        // Append targets
        for _, host := range remoteTargets {
            if checkIsInTarget(allTargets, host) {
                 continue
            }
            t := &target{
                host: host,
                addresses: make([]net.IPAddr, 0),
                resolver: resolver,
            }
            allTargets = append(allTargets, t)
        }
        // Format delay
        for i, t := range allTargets {
            t.delay = time.Duration(10 * i) * time.Millisecond
        }
        time.Sleep(cfg.UpdateRemoteInterval.Duration() * time.Second)
    }
}

func checkIsInTarget(targets []*target, host string) bool {
    for _, t := range targets {
        if t.host == host{
            return true
        }
    }

    return false
}

func fetchRemoteTargets(cfg *config.Config) []string {
    client := &http.Client{}
    remoteTargets := []string{}
    for _, uri := range cfg.RemoteTargets {
        u, err := url.Parse(uri)
        if err != nil {
            continue
        }
        if u.Scheme != "http" && u.Scheme != "https" {
            continue
        }

        req, err := http.NewRequest("GET", uri, nil)
        if u.User != nil {
            passwd, _ := u.User.Password()
            req.SetBasicAuth(u.User.Username(), passwd)
        }
        resp, err := client.Do(req)
        if err != nil {
            continue
        }
        body, err := ioutil.ReadAll(resp.Body)
        for _, t := range strings.Split(string(body), "\n") {
            if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//"){
                continue
            }
            // _, err := net.LookupIP(t)
            // if err != nil {
            //     continue
            // }
            remoteTargets = append(remoteTargets, t)
        }
    }
    return remoteTargets
}

func startServer(monitor *mon.Monitor) {
	log.Infof("Starting ping exporter (Version: %s)", version)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, indexHTML, *metricsPath)
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(&pingCollector{monitor: monitor})
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      log.NewErrorLogger(),
		ErrorHandling: promhttp.ContinueOnError})
	http.Handle(*metricsPath, h)

	log.Infof("Listening for %s on %s", *metricsPath, *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

func loadConfig() (*config.Config, error) {
	if *configFile == "" {
		cfg := config.Config{}
		addFlagToConfig(&cfg)
		return &cfg, nil
	}

	f, err := os.Open(*configFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg, err := config.FromYAML(f)
	if err != nil {
		addFlagToConfig(cfg)
	}
	return cfg, err
}

func setupResolver(cfg *config.Config) *net.Resolver {
	if cfg.DNS.Nameserver == "" {
		return net.DefaultResolver
	}

	if !strings.HasSuffix(cfg.DNS.Nameserver, ":53") {
		cfg.DNS.Nameserver += ":53"
	}
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{}
		return d.DialContext(ctx, "udp", cfg.DNS.Nameserver)
	}
	return &net.Resolver{PreferGo: true, Dial: dialer}
}

// addFlagToConfig updates cfg with command line flag values, unless the
// config has non-zero values.
func addFlagToConfig(cfg *config.Config) {
	if len(cfg.Targets) == 0 {
		cfg.Targets = *targets
	}
	if cfg.Ping.History == 0 {
		cfg.Ping.History = *historySize
	}
	if cfg.Ping.Interval == 0 {
		cfg.Ping.Interval.Set(*pingInterval)
	}
	if cfg.Ping.Timeout == 0 {
		cfg.Ping.Timeout.Set(*pingTimeout)
	}
	if cfg.Ping.Size == 0 {
		cfg.Ping.Size = *pingSize
	}
	if cfg.DNS.Refresh == 0 {
		cfg.DNS.Refresh.Set(*dnsRefresh)
	}
	if cfg.DNS.Nameserver == "" {
		cfg.DNS.Nameserver = *dnsNameServer
	}
	if len(cfg.RemoteTargets) == 0 {
		cfg.RemoteTargets = *remoteTargets
	}
	if cfg.UpdateRemoteInterval == 0 {
        cfg.UpdateRemoteInterval.Set(*pingInterval)

	}
}

const indexHTML = `<!doctype html>
<html>
<head>
	<meta charset="UTF-8">
	<title>ping Exporter (Version ` + version + `)</title>
</head>
<body>
	<h1>ping Exporter</h1>
	<p><a href="%s">Metrics</a></p>
	<h2>More information:</h2>
	<p><a href="https://github.com/czerwonk/ping_exporter">github.com/czerwonk/ping_exporter</a></p>
</body>
</html>
`
