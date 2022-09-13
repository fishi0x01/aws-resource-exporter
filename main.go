package main

import (
	"errors"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

const (
	namespace                      = "aws_resources_exporter"
	DEFAULT_TIMEOUT  time.Duration = 30 * time.Second
	CONFIG_FILE_PATH               = "./aws-resource-exporter-config.yaml"
)

var (
	listenAddress = kingpin.Flag("web.listen-address", "The address to listen on for HTTP requests.").Default(":9115").String()
	metricsPath   = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()

	exporterMetrics *ExporterMetrics
)

func main() {
	os.Exit(run())
}

type BaseConfig struct {
	Enabled  bool           `yaml:"enabled"`
	Interval *time.Duration `yaml:"interval"`
	CacheTTL *time.Duration `yaml:"cache_ttl"`
}

type RDSConfig struct {
	BaseConfig `yaml:"base,inline"`
	Regions    []string `yaml:"regions"`
}

type VPCConfig struct {
	BaseConfig `yaml:"base,inline"`
	Timeout    *time.Duration `yaml:"timeout"`
	Regions    []string       `yaml:"regions"`
}

type Route53Config struct {
	BaseConfig `yaml:"base,inline"`
	Timeout    *time.Duration `yaml:"timeout"`
	Region     string         `yaml:"region"` // Use only a single Region for now, as the current metric is global
}

type EC2Config struct {
	BaseConfig `yaml:"base,inline"`
	Timeout    *time.Duration `yaml:"timeout"`
	Regions    []string       `yaml:"regions"`
}

type Config struct {
	RdsConfig     RDSConfig     `yaml:"rds"`
	VpcConfig     VPCConfig     `yaml:"vpc"`
	Route53Config Route53Config `yaml:"route53"`
	EC2Config     EC2Config     `yaml:"ec2"`
}

func durationPtr(duration time.Duration) *time.Duration {
	return &duration
}

func loadExporterConfiguration(logger log.Logger, configFile string) (*Config, error) {
	var config Config
	file, err := ioutil.ReadFile(configFile)
	if err != nil {
		level.Error(logger).Log("Could not load configuration file")
		return nil, errors.New("Could not load configuration file: " + configFile)
	}
	yaml.Unmarshal(file, &config)

	if config.RdsConfig.CacheTTL == nil {
		config.RdsConfig.CacheTTL = durationPtr(35 * time.Second)
	}
	if config.VpcConfig.CacheTTL == nil {
		config.VpcConfig.CacheTTL = durationPtr(35 * time.Second)
	}
	if config.Route53Config.CacheTTL == nil {
		config.Route53Config.CacheTTL = durationPtr(35 * time.Second)
	}
	if config.EC2Config.CacheTTL == nil {
		config.EC2Config.CacheTTL = durationPtr(35 * time.Second)
	}

	if config.RdsConfig.Interval == nil {
		config.RdsConfig.Interval = durationPtr(15 * time.Second)
	}
	if config.VpcConfig.Interval == nil {
		config.VpcConfig.Interval = durationPtr(15 * time.Second)
	}
	if config.Route53Config.Interval == nil {
		config.Route53Config.Interval = durationPtr(15 * time.Second)
	}
	if config.EC2Config.Interval == nil {
		config.EC2Config.Interval = durationPtr(15 * time.Second)
	}

	if config.VpcConfig.Timeout == nil {
		config.VpcConfig.Timeout = durationPtr(10 * time.Second)
	}
	if config.Route53Config.Timeout == nil {
		config.Route53Config.Timeout = durationPtr(10 * time.Second)
	}
	if config.EC2Config.Timeout == nil {
		config.EC2Config.Timeout = durationPtr(10 * time.Second)
	}
	return &config, nil
}

func setupCollectors(logger log.Logger, configFile string) ([]prometheus.Collector, error) {
	var collectors []prometheus.Collector
	config, err := loadExporterConfiguration(logger, configFile)
	if err != nil {
		return nil, err
	}
	level.Info(logger).Log("msg", "Configuring vpc with regions", "regions", strings.Join(config.VpcConfig.Regions, ","))
	level.Info(logger).Log("msg", "Configuring rds with regions", "regions", strings.Join(config.RdsConfig.Regions, ","))
	level.Info(logger).Log("msg", "Configuring ec2 with regions", "regions", strings.Join(config.EC2Config.Regions, ","))
	level.Info(logger).Log("msg", "Configuring route53 with region", "region", config.Route53Config.Region)
	var vpcSessions []*session.Session
	level.Info(logger).Log("msg", "Will VPC metrics be gathered?", "vpc-enabled", config.VpcConfig.Enabled)
	if config.VpcConfig.Enabled {
		for _, region := range config.VpcConfig.Regions {
			config := aws.NewConfig().WithRegion(region)
			sess := session.Must(session.NewSession(config))
			vpcSessions = append(vpcSessions, sess)
		}
		vpcExporter := NewVPCExporter(vpcSessions, logger, config.VpcConfig)
		collectors = append(collectors, vpcExporter)
		go vpcExporter.CollectLoop()
	}
	level.Info(logger).Log("msg", "Will RDS metrics be gathered?", "rds-enabled", config.RdsConfig.Enabled)
	var rdsSessions []*session.Session
	if config.RdsConfig.Enabled {
		for _, region := range config.RdsConfig.Regions {
			config := aws.NewConfig().WithRegion(region)
			sess := session.Must(session.NewSession(config))
			rdsSessions = append(rdsSessions, sess)
		}
		rdsExporter := NewRDSExporter(rdsSessions, logger, config.RdsConfig)
		collectors = append(collectors, rdsExporter)
		go rdsExporter.CollectLoop()
	}
	level.Info(logger).Log("msg", "Will EC2 metrics be gathered?", "ec2-enabled", config.EC2Config.Enabled)
	var ec2Sessions []*session.Session
	if config.EC2Config.Enabled {
		for _, region := range config.EC2Config.Regions {
			config := aws.NewConfig().WithRegion(region)
			sess := session.Must(session.NewSession(config))
			ec2Sessions = append(ec2Sessions, sess)
		}
		ec2Exporter := NewEC2Exporter(ec2Sessions, logger, config.EC2Config)
		collectors = append(collectors, ec2Exporter)
		go ec2Exporter.CollectLoop()
	}
	level.Info(logger).Log("msg", "Will Route53 metrics be gathered?", "route53-enabled", config.Route53Config.Enabled)
	if config.Route53Config.Enabled {
		awsConfig := aws.NewConfig().WithRegion(config.Route53Config.Region)
		sess := session.Must(session.NewSession(awsConfig))
		r53Exporter := NewRoute53Exporter(sess, logger, config.Route53Config)
		collectors = append(collectors, r53Exporter)
		go r53Exporter.CollectLoop()
	}

	return collectors, nil
}

func run() int {
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print(namespace))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)

	level.Info(logger).Log("msg", "Starting"+namespace, "version", version.Info())
	level.Info(logger).Log("msg", "Build context", version.BuildContext())

	exporterMetrics = NewExporterMetrics()
	var configFile string
	if path := os.Getenv("AWS_RESOURCE_EXPORTER_CONFIG_FILE"); path != "" {
		configFile = path
	} else {
		configFile = CONFIG_FILE_PATH
	}
	cs, err := setupCollectors(logger, configFile)
	if err != nil {
		level.Error(logger).Log("msg", "Could not load configuration file", "err", err)
		return 1
	}
	collectors := append(cs, exporterMetrics)
	prometheus.MustRegister(
		collectors...,
	)

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>AWS Resources Exporter</title></head>
             <body>
             <h1>AWS Resources Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})

	srv := http.Server{Addr: *listenAddress}
	srvc := make(chan struct{})
	term := make(chan os.Signal, 1)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	go func() {
		level.Info(logger).Log("msg", "Starting HTTP server", "address", *listenAddress)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			level.Error(logger).Log("msg", "Error starting HTTP server", "err", err)
			close(srvc)
		}
	}()

	for {
		select {
		case <-term:
			level.Info(logger).Log("msg", "Received SIGTERM, exiting gracefully...")
			return 0
		case <-srvc:
			return 1
		}
	}
}
