//go:generate go-enum -f=$GOFILE --marshal --names
package config

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/hako/durafmt"
	"github.com/hashicorp/go-multierror"

	"github.com/0xERR0R/blocky/log"
	"github.com/creasty/defaults"
	"gopkg.in/yaml.v2"
)

// NetProtocol resolver protocol ENUM(
// tcp+udp // TCP and UDP protocols
// tcp-tls // TCP-TLS protocol
// https // HTTPS protocol
// )
type NetProtocol uint16

// QueryLogType type of the query log ENUM(
// console // use logger as fallback
// none // no logging
// kafka // Kafka stream
// mysql // MySQL or MariaDB database
// postgresql // PostgreSQL database
// csv // CSV file per day
// csv-client // CSV file per day and client
// )
type QueryLogType int16

type QType dns.Type

func (c QType) String() string {
	return dns.TypeToString[uint16(c)]
}

type Duration time.Duration

func (c *Duration) String() string {
	return durafmt.Parse(time.Duration(*c)).String()
}

// nolint:gochecknoglobals
var netDefaultPort = map[NetProtocol]uint16{
	NetProtocolTcpUdp: 53,
	NetProtocolTcpTls: 853,
	NetProtocolHttps:  443,
}

// Upstream is the definition of external DNS server
type Upstream struct {
	Net  NetProtocol
	Host string
	Port uint16
	Path string
}

// UnmarshalYAML creates Upstream from YAML
func (u *Upstream) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}

	upstream, err := ParseUpstream(s)
	if err != nil {
		return fmt.Errorf("can't convert upstream '%s': %w", s, err)
	}

	*u = upstream

	return nil
}

// ListenConfig is a list of address(es) to listen on
type ListenConfig []string

// UnmarshalYAML creates ListenConfig from YAML
func (l *ListenConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var addresses string
	if err := unmarshal(&addresses); err != nil {
		return err
	}

	*l = strings.Split(addresses, ",")

	return nil
}

// UnmarshalYAML creates ConditionalUpstreamMapping from YAML
func (c *ConditionalUpstreamMapping) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var input map[string]string
	if err := unmarshal(&input); err != nil {
		return err
	}

	result := make(map[string][]Upstream, len(input))

	for k, v := range input {
		var upstreams []Upstream

		for _, part := range strings.Split(v, ",") {
			upstream, err := ParseUpstream(strings.TrimSpace(part))
			if err != nil {
				return fmt.Errorf("can't convert upstream '%s': %w", strings.TrimSpace(part), err)
			}

			upstreams = append(upstreams, upstream)
		}

		result[k] = upstreams
	}

	c.Upstreams = result

	return nil
}

// UnmarshalYAML creates CustomDNSMapping from YAML
func (c *CustomDNSMapping) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var input map[string]string
	if err := unmarshal(&input); err != nil {
		return err
	}

	result := make(map[string][]net.IP, len(input))

	for k, v := range input {
		var ips []net.IP

		for _, part := range strings.Split(v, ",") {
			ip := net.ParseIP(strings.TrimSpace(part))
			if ip == nil {
				return fmt.Errorf("invalid IP address '%s'", part)
			}

			ips = append(ips, ip)
		}

		result[k] = ips
	}

	c.HostIPs = result

	return nil
}

// UnmarshalYAML creates Duration from YAML. If no unit is used, uses minutes
func (c *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var input string
	if err := unmarshal(&input); err != nil {
		return err
	}

	if minutes, err := strconv.Atoi(input); err == nil {
		// duration is defined as number without unit
		// use minutes to ensure back compatibility
		*c = Duration(time.Duration(minutes) * time.Minute)
		return nil
	}

	duration, err := time.ParseDuration(input)
	if err == nil {
		*c = Duration(duration)
		return nil
	}

	return err
}

func (c *QType) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var input string
	if err := unmarshal(&input); err != nil {
		return err
	}

	t, found := dns.StringToType[input]
	if !found {
		types := make([]string, 0, len(dns.StringToType))
		for k := range dns.StringToType {
			types = append(types, k)
		}

		sort.Strings(types)

		return fmt.Errorf("unknown DNS query type: '%s'. Please use following types '%s'",
			input, strings.Join(types, ", "))
	}

	*c = QType(t)

	return nil
}

var validDomain = regexp.MustCompile(
	`^(([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9])\.)*([A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9])$`)

// ParseUpstream creates new Upstream from passed string in format [net]:host[:port][/path]
func ParseUpstream(upstream string) (Upstream, error) {
	var path string

	var port uint16

	n, upstream := extractNet(upstream)

	path, upstream = extractPath(upstream)

	host, portString, err := net.SplitHostPort(upstream)

	// string contains host:port
	if err == nil {
		var p uint64
		p, err = strconv.ParseUint(strings.TrimSpace(portString), 10, 16)

		if err != nil {
			err = fmt.Errorf("can't convert port to number (1 - 65535) %w", err)
			return Upstream{}, err
		}

		port = uint16(p)
	} else {
		// only host, use default port
		host = upstream
		port = netDefaultPort[n]
	}

	// validate hostname or ip
	ip := net.ParseIP(host)

	if ip == nil {
		// is not IP
		if !validDomain.MatchString(host) {
			return Upstream{}, fmt.Errorf("wrong host name '%s'", host)
		}
	}

	return Upstream{
		Net:  n,
		Host: host,
		Port: port,
		Path: path,
	}, nil
}

func extractPath(in string) (path string, upstream string) {
	slashIdx := strings.Index(in, "/")

	if slashIdx >= 0 {
		path = in[slashIdx:]
		upstream = in[:slashIdx]
	} else {
		upstream = in
	}

	return
}

func extractNet(upstream string) (NetProtocol, string) {
	if strings.HasPrefix(upstream, NetProtocolTcpUdp.String()+":") {
		return NetProtocolTcpUdp, strings.Replace(upstream, NetProtocolTcpUdp.String()+":", "", 1)
	}

	if strings.HasPrefix(upstream, NetProtocolTcpTls.String()+":") {
		return NetProtocolTcpTls, strings.Replace(upstream, NetProtocolTcpTls.String()+":", "", 1)
	}

	if strings.HasPrefix(upstream, NetProtocolHttps.String()+":") {
		return NetProtocolHttps, strings.TrimPrefix(strings.Replace(upstream, NetProtocolHttps.String()+":", "", 1), "//")
	}

	return NetProtocolTcpUdp, upstream
}

// Config main configuration
// nolint:maligned
type Config struct {
	Upstream        UpstreamConfig            `yaml:"upstream"`
	UpstreamTimeout Duration                  `yaml:"upstreamTimeout" default:"2s"`
	CustomDNS       CustomDNSConfig           `yaml:"customDNS"`
	Conditional     ConditionalUpstreamConfig `yaml:"conditional"`
	Blocking        BlockingConfig            `yaml:"blocking"`
	ClientLookup    ClientLookupConfig        `yaml:"clientLookup"`
	Caching         CachingConfig             `yaml:"caching"`
	QueryLog        QueryLogConfig            `yaml:"queryLog"`
	Prometheus      PrometheusConfig          `yaml:"prometheus"`
	Redis           RedisConfig               `yaml:"redis"`
	LogLevel        log.Level                 `yaml:"logLevel" default:"info"`
	LogFormat       log.FormatType            `yaml:"logFormat" default:"text"`
	LogPrivacy      bool                      `yaml:"logPrivacy" default:"false"`
	LogTimestamp    bool                      `yaml:"logTimestamp" default:"true"`
	DNSPorts        ListenConfig              `yaml:"port" default:"[\"53\"]"`
	HTTPPorts       ListenConfig              `yaml:"httpPort"`
	HTTPSPorts      ListenConfig              `yaml:"httpsPort"`
	TLSPorts        ListenConfig              `yaml:"tlsPort"`
	// Deprecated
	DisableIPv6  bool            `yaml:"disableIPv6" default:"false"`
	CertFile     string          `yaml:"certFile"`
	KeyFile      string          `yaml:"keyFile"`
	BootstrapDNS Upstream        `yaml:"bootstrapDns"`
	HostsFile    HostsFileConfig `yaml:"hostsFile"`
	Filtering    FilteringConfig `yaml:"filtering"`
}

// PrometheusConfig contains the config values for prometheus
type PrometheusConfig struct {
	Enable bool   `yaml:"enable" default:"false"`
	Path   string `yaml:"path" default:"/metrics"`
}

// UpstreamConfig upstream server configuration
type UpstreamConfig struct {
	ExternalResolvers map[string][]Upstream `yaml:",inline"`
}

// RewriteConfig custom DNS configuration
type RewriteConfig struct {
	Rewrite map[string]string `yaml:"rewrite"`
}

// CustomDNSConfig custom DNS configuration
type CustomDNSConfig struct {
	RewriteConfig       `yaml:",inline"`
	CustomTTL           Duration         `yaml:"customTTL" default:"1h"`
	Mapping             CustomDNSMapping `yaml:"mapping"`
	FilterUnmappedTypes bool             `yaml:"filterUnmappedTypes" default:"true"`
}

// CustomDNSMapping mapping for the custom DNS configuration
type CustomDNSMapping struct {
	HostIPs map[string][]net.IP
}

// ConditionalUpstreamConfig conditional upstream configuration
type ConditionalUpstreamConfig struct {
	RewriteConfig `yaml:",inline"`
	Mapping       ConditionalUpstreamMapping `yaml:"mapping"`
}

// ConditionalUpstreamMapping mapping for conditional configuration
type ConditionalUpstreamMapping struct {
	Upstreams map[string][]Upstream
}

// BlockingConfig configuration for query blocking
type BlockingConfig struct {
	BlackLists           map[string][]string `yaml:"blackLists"`
	WhiteLists           map[string][]string `yaml:"whiteLists"`
	ClientGroupsBlock    map[string][]string `yaml:"clientGroupsBlock"`
	BlockType            string              `yaml:"blockType" default:"ZEROIP"`
	BlockTTL             Duration            `yaml:"blockTTL" default:"6h"`
	DownloadTimeout      Duration            `yaml:"downloadTimeout" default:"60s"`
	DownloadAttempts     int                 `yaml:"downloadAttempts" default:"3"`
	DownloadCooldown     Duration            `yaml:"downloadCooldown" default:"1s"`
	RefreshPeriod        Duration            `yaml:"refreshPeriod" default:"4h"`
	FailStartOnListError bool                `yaml:"failStartOnListError" default:"false"`
}

// ClientLookupConfig configuration for the client lookup
type ClientLookupConfig struct {
	ClientnameIPMapping map[string][]net.IP `yaml:"clients"`
	Upstream            Upstream            `yaml:"upstream"`
	SingleNameOrder     []uint              `yaml:"singleNameOrder"`
}

// CachingConfig configuration for domain caching
type CachingConfig struct {
	MinCachingTime        Duration `yaml:"minTime"`
	MaxCachingTime        Duration `yaml:"maxTime"`
	CacheTimeNegative     Duration `yaml:"cacheTimeNegative" default:"30m"`
	MaxItemsCount         int      `yaml:"maxItemsCount"`
	Prefetching           bool     `yaml:"prefetching"`
	PrefetchExpires       Duration `yaml:"prefetchExpires" default:"2h"`
	PrefetchThreshold     int      `yaml:"prefetchThreshold" default:"5"`
	PrefetchMaxItemsCount int      `yaml:"prefetchMaxItemsCount"`
}

// QueryLogConfig configuration for the query logging
type QueryLogConfig struct {
	Target           string       `yaml:"target"`
	Type             QueryLogType `yaml:"type"`
	LogRetentionDays uint64       `yaml:"logRetentionDays"`
	CreationAttempts int          `yaml:"creationAttempts" default:"3"`
	CreationCooldown Duration     `yaml:"creationCooldown" default:"2s"`
}

// RedisConfig configuration for the redis connection
type RedisConfig struct {
	Address            string   `yaml:"address"`
	Password           string   `yaml:"password" default:""`
	Database           int      `yaml:"database" default:"0"`
	Required           bool     `yaml:"required" default:"false"`
	ConnectionAttempts int      `yaml:"connectionAttempts" default:"3"`
	ConnectionCooldown Duration `yaml:"connectionCooldown" default:"1s"`
}

type HostsFileConfig struct {
	Filepath      string   `yaml:"filePath"`
	HostsTTL      Duration `yaml:"hostsTTL" default:"1h"`
	RefreshPeriod Duration `yaml:"refreshPeriod" default:"1h"`
}

type FilteringConfig struct {
	QueryTypes []QType `yaml:"queryTypes"`
}

// nolint:gochecknoglobals
var config = &Config{}

// LoadConfig creates new config from YAML file
func LoadConfig(path string, mandatory bool) error {
	cfg := Config{}
	if err := defaults.Set(&cfg); err != nil {
		return fmt.Errorf("can't apply default values: %w", err)
	}

	data, err := ioutil.ReadFile(path)

	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !mandatory {
			// config file does not exist
			// return config with default values
			config = &cfg
			return nil
		}

		return fmt.Errorf("can't read config file: %w", err)
	}

	return unmarshalConfig(data, cfg)
}

func unmarshalConfig(data []byte, cfg Config) error {
	err := yaml.UnmarshalStrict(data, &cfg)
	if err != nil {
		return fmt.Errorf("wrong file structure: %w", err)
	}

	err = validateConfig(&cfg)
	if err != nil {
		return fmt.Errorf("unable to validate config: %w", err)
	}

	config = &cfg

	return nil
}

func validateConfig(cfg *Config) (err error) {
	if len(cfg.TLSPorts) != 0 && (cfg.CertFile == "" || cfg.KeyFile == "") {
		err = multierror.Append(err, errors.New("'certFile' and 'keyFile' parameters are mandatory for TLS"))
	}

	if len(cfg.HTTPSPorts) != 0 && (cfg.CertFile == "" || cfg.KeyFile == "") {
		err = multierror.Append(err, errors.New("'certFile' and 'keyFile' parameters are mandatory for HTTPS"))
	}

	if cfg.DisableIPv6 {
		log.Log().Warnf("'disableIPv6' is deprecated. Please use 'filtering.queryTypes' with 'AAAA' instead.")

		cfg.Filtering.QueryTypes = append(cfg.Filtering.QueryTypes, QType(dns.TypeAAAA))
	}

	return
}

// GetConfig returns the current config
func GetConfig() *Config {
	return config
}
