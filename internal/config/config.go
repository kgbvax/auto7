// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config reads and checks the auto7 configuration. The
// configuration is one TOML file. Secrets come only from the environment,
// and the file must not hold a password.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// PasswordEnv is the environment variable that holds the MQTT password.
const PasswordEnv = "AUTO7_MQTT_PASSWORD"

// Hard bounds of the CQ run and the offset moves. The TX gate enforces the
// same values, so the configuration cannot allow more than the gate.
const (
	// CQMaxPeriodsCeiling is the largest number of CQ periods without an
	// answer in one run.
	CQMaxPeriodsCeiling = 10
	// CQListenPeriodsFloor is the smallest number of listen periods after
	// a CQ run.
	CQListenPeriodsFloor = 4
	// OffsetMovesPerHourCeiling is the largest number of TX offset moves in
	// one hour.
	OffsetMovesPerHourCeiling = 6
	// OffsetDistanceFloorHz is the smallest distance in Hz between the own
	// TX offset and the offset of the called station.
	OffsetDistanceFloorHz = 50
)

// Config is the complete configuration.
type Config struct {
	// Site is the StationA site, the first part of every topic.
	Site string `toml:"site"`
	// Station is the StationA station, for example "hf".
	Station string `toml:"station"`
	// Host is the name of this host. It names the own state slot. The
	// default is the short host name of the computer.
	Host          string            `toml:"host"`
	MQTT          MQTTConfig        `toml:"mqtt"`
	WSJTX         WSJTXConfig       `toml:"wsjtx"`
	Page          PageConfig        `toml:"page"`
	Presence      PresenceConfig    `toml:"presence"`
	Horstreporter HorstreporterConf `toml:"horstreporter"`
	// Bands is the list of bands and modes that auto7 may use.
	Bands []Band `toml:"bands"`
	// FoxHoundDialsHz lists the dial frequencies in Hz of Fox and Hound
	// operation. auto7 never follows a spot on one of these dials.
	FoxHoundDialsHz []uint64 `toml:"fox_hound_dials_hz"`
	Limits          Limits   `toml:"limits"`
}

// MQTTConfig holds the broker connection.
type MQTTConfig struct {
	// Broker is the broker URL. It must use ssl:// unless its host is a
	// loopback or private address.
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	// Password comes only from the environment variable PasswordEnv.
	Password Secret `toml:"-" json:"-"`
}

// WSJTXConfig holds the UDP link to WSJT-X.
type WSJTXConfig struct {
	// ID is the WSJT-X instance Id. auto7 accepts datagrams only from this
	// Id.
	ID string `toml:"id"`
	// Listen is the loopback UDP address that WSJT-X sends to.
	Listen string `toml:"listen"`
}

// PageConfig holds the operator page.
type PageConfig struct {
	// Listen is the loopback TCP address of the page.
	Listen string `toml:"listen"`
}

// PresenceConfig says which hosts give presence.
type PresenceConfig struct {
	// Hosts lists the hosts whose presence heartbeat counts. The default
	// is the own host only.
	Hosts []string `toml:"hosts"`
}

// HorstreporterConf is the band activity service.
type HorstreporterConf struct {
	// URL is the base URL. Empty means that auto7 does not ask it.
	URL string `toml:"url"`
	// QTH is the own station for the analysis: a locator or a callsign.
	QTH string `toml:"qth"`
}

// Band is one entry of the band list.
type Band struct {
	// Name is the band, for example "20m".
	Name string `toml:"name"`
	// Modes lists the modes on this band: FT8, FT4 or both.
	Modes []string `toml:"modes"`
}

// Limits are the limits that the operator can change, each in its range.
type Limits struct {
	// Tries is the number of calls to one station.
	Tries int `toml:"tries"`
	// TriesHighValue is the number of calls to a high-value station.
	TriesHighValue int `toml:"tries_high_value"`
	// SkipMinutes is how long auto7 skips a station after it gives up.
	SkipMinutes int `toml:"skip_minutes"`
	// CQMaxPeriods is the largest number of CQ periods without an answer.
	CQMaxPeriods int `toml:"cq_max_periods"`
	// CQListenPeriods is the number of listen periods after a CQ run.
	CQListenPeriods int `toml:"cq_listen_periods"`
	// OffsetMovesPerHour is the largest number of TX offset moves per hour.
	OffsetMovesPerHour int `toml:"offset_moves_per_hour"`
	// OffsetMinDistanceHz is the smallest distance from the offset of the
	// called station.
	OffsetMinDistanceHz int `toml:"offset_min_distance_hz"`
	// NotHeardMinutes is how long a spot stays "not heard here".
	NotHeardMinutes int `toml:"not_heard_minutes"`
}

// limitRange is the allowed range of one limit.
type limitRange struct {
	key      string
	min, max int
	value    func(*Limits) int
}

// limitRanges holds the range of each limit.
var limitRanges = []limitRange{
	{"tries", 1, 10, func(l *Limits) int { return l.Tries }},
	{"tries_high_value", 1, 30, func(l *Limits) int { return l.TriesHighValue }},
	{"skip_minutes", 1, 240, func(l *Limits) int { return l.SkipMinutes }},
	{"cq_max_periods", 1, CQMaxPeriodsCeiling, func(l *Limits) int { return l.CQMaxPeriods }},
	{"cq_listen_periods", CQListenPeriodsFloor, 20, func(l *Limits) int { return l.CQListenPeriods }},
	{"offset_moves_per_hour", 0, OffsetMovesPerHourCeiling, func(l *Limits) int { return l.OffsetMovesPerHour }},
	{"offset_min_distance_hz", OffsetDistanceFloorHz, 1000, func(l *Limits) int { return l.OffsetMinDistanceHz }},
	{"not_heard_minutes", 5, 120, func(l *Limits) int { return l.NotHeardMinutes }},
}

// Mode names.
const (
	ModeFT8 = "FT8"
	ModeFT4 = "FT4"
)

// knownBands lists the bands that the band list accepts, in order.
var knownBands = []string{"160m", "80m", "40m", "30m", "20m", "17m", "15m", "12m", "10m", "6m"}

// Lowest and highest dial frequency in Hz that WSJT-X accepts by CAT.
const (
	minDialHz = 135_700
	maxDialHz = 250_000_000_000
)

// Defaults returns the default configuration.
func Defaults() Config {
	host := ownHost()
	return Config{
		Site:     "site",
		Station:  "hf",
		Host:     host,
		MQTT:     MQTTConfig{Broker: "tcp://127.0.0.1:1883"},
		WSJTX:    WSJTXConfig{ID: "WSJT-X", Listen: "127.0.0.1:2237"},
		Page:     PageConfig{Listen: "127.0.0.1:8077"},
		Presence: PresenceConfig{Hosts: []string{host}},
		Limits: Limits{
			Tries:               4,
			TriesHighValue:      12,
			SkipMinutes:         10,
			CQMaxPeriods:        10,
			CQListenPeriods:     4,
			OffsetMovesPerHour:  6,
			OffsetMinDistanceHz: 50,
			NotHeardMinutes:     30,
		},
	}
}

// ownHost returns the short host name in lower case.
func ownHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.SplitN(h, ".", 2)[0])
}

// DefaultPath returns the default path of the configuration file in the
// user configuration directory.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(dir, "auto7", "config.toml")
}

// Load reads the file at path, overlays the environment and checks the
// result. If optional is true, a missing file gives the defaults.
func Load(path string, optional bool) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		md, err := toml.Decode(string(data), &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("decode %s: %w", path, err)
		}
		for _, k := range md.Keys() {
			if strings.Contains(strings.ToLower(k[len(k)-1]), "password") {
				return Config{}, fmt.Errorf("%s: the key %s is not allowed: the password comes only from %s", path, k.String(), PasswordEnv)
			}
		}
		if !md.IsDefined("presence", "hosts") {
			cfg.Presence.Hosts = []string{cfg.Host}
		}
	case optional && errors.Is(err, os.ErrNotExist):
	default:
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if v := os.Getenv(PasswordEnv); v != "" {
		cfg.MQTT.Password = Secret(v)
	}
	cfg.normalize()
	return cfg, cfg.Validate()
}

// normalize brings names into their canonical form.
func (c *Config) normalize() {
	c.Host = strings.ToLower(strings.TrimSpace(c.Host))
	if c.MQTT.ClientID == "" {
		c.MQTT.ClientID = "auto7-" + c.Host
	}
	for i, h := range c.Presence.Hosts {
		c.Presence.Hosts[i] = strings.ToLower(strings.TrimSpace(h))
	}
	for i := range c.Bands {
		b := &c.Bands[i]
		b.Name = strings.ToLower(strings.TrimSpace(b.Name))
		for j, m := range b.Modes {
			b.Modes[j] = strings.ToUpper(strings.TrimSpace(m))
		}
	}
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.Site == "" || c.Station == "" || c.Host == "" {
		return errors.New("site, station and host must be set")
	}
	for _, s := range []string{c.Site, c.Station, c.Host} {
		if strings.ContainsAny(s, "+#/ ") {
			return fmt.Errorf("%q: site, station and host must not hold +, #, / or a space", s)
		}
	}
	if err := checkBroker(c.MQTT.Broker); err != nil {
		return err
	}
	if c.WSJTX.ID == "" {
		return errors.New("wsjtx.id must be set")
	}
	if err := checkLoopback("wsjtx.listen", c.WSJTX.Listen); err != nil {
		return err
	}
	if err := checkLoopback("page.listen", c.Page.Listen); err != nil {
		return err
	}
	if len(c.Presence.Hosts) == 0 {
		return errors.New("presence.hosts must name at least one host")
	}
	for _, h := range c.Presence.Hosts {
		if h == "" {
			return errors.New("presence.hosts holds an empty name")
		}
	}
	if c.Horstreporter.URL != "" {
		u, err := url.Parse(c.Horstreporter.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("horstreporter.url: want http://host:port or https://host:port")
		}
		if u.User != nil {
			return errors.New("horstreporter.url must not hold a user or a password")
		}
		if strings.TrimSpace(c.Horstreporter.QTH) == "" {
			return errors.New("horstreporter.qth is required with horstreporter.url")
		}
	}
	if err := checkBands(c.Bands); err != nil {
		return err
	}
	for _, f := range c.FoxHoundDialsHz {
		if f < minDialHz || f > maxDialHz {
			return fmt.Errorf("fox_hound_dials_hz: %d: want %d to %d Hz", f, minDialHz, maxDialHz)
		}
	}
	l := c.Limits
	for _, r := range limitRanges {
		if v := r.value(&l); v < r.min || v > r.max {
			return fmt.Errorf("limits.%s = %d: want %d to %d", r.key, v, r.min, r.max)
		}
	}
	return nil
}

// checkBroker applies the transport rule to the broker URL. It never puts
// the URL itself into an error, because the URL can hold a user.
func checkBroker(broker string) error {
	u, err := url.Parse(broker)
	if err != nil || u.Host == "" {
		return errors.New("mqtt.broker: want scheme://host:port")
	}
	if u.User != nil {
		return fmt.Errorf("mqtt.broker must not hold a user or a password: set mqtt.user and %s", PasswordEnv)
	}
	switch u.Scheme {
	case "ssl", "tls", "mqtts":
		return nil
	case "tcp", "mqtt":
	default:
		return fmt.Errorf("mqtt.broker: scheme %q: want ssl:// or tcp://", u.Scheme)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	a, err := netip.ParseAddr(host)
	if err == nil && isLocalNet(a.Unmap()) {
		return nil
	}
	return fmt.Errorf("mqtt.broker: host %s: a plain tcp:// link needs a loopback or private address, use ssl:// for other hosts", host)
}

// cgnat is the shared address range that VPNs such as Tailscale use.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isLocalNet reports whether a is a loopback, private or VPN address.
func isLocalNet(a netip.Addr) bool {
	return a.IsLoopback() || a.IsPrivate() || cgnat.Contains(a)
}

// checkLoopback requires a host:port address on a loopback host.
func checkLoopback(key, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q: want host:port", key, addr)
	}
	if host == "localhost" {
		return nil
	}
	if a, err := netip.ParseAddr(host); err == nil && a.Unmap().IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s %q: want a loopback address", key, addr)
}

// checkBands checks the band list.
func checkBands(bands []Band) error {
	seen := map[string]bool{}
	for _, b := range bands {
		if b.Name == "60m" {
			return errors.New("bands: 60m is not allowed, because auto7 has no power limit for it")
		}
		if !slices.Contains(knownBands, b.Name) {
			return fmt.Errorf("bands: %q: want one of %s", b.Name, strings.Join(knownBands, ", "))
		}
		if seen[b.Name] {
			return fmt.Errorf("bands: %s is listed twice", b.Name)
		}
		seen[b.Name] = true
		if len(b.Modes) == 0 {
			return fmt.Errorf("bands: %s needs at least one mode", b.Name)
		}
		for _, m := range b.Modes {
			if m != ModeFT8 && m != ModeFT4 {
				return fmt.Errorf("bands: %s: mode %q: want FT8 or FT4", b.Name, m)
			}
			if m == ModeFT4 && b.Name == "160m" {
				return errors.New("bands: FT4 on 160m is not allowed")
			}
		}
	}
	return nil
}

// Secret holds a value that never shows in text. Every format verb, JSON,
// text encoding and slog give "[set]" or an empty string.
type Secret string

// Reveal returns the value. Use it only where the value goes to the peer.
func (s Secret) Reveal() string { return string(s) }

func (s Secret) redacted() string {
	if s == "" {
		return ""
	}
	return "[set]"
}

// String gives the redacted form.
func (s Secret) String() string { return s.redacted() }

// GoString gives the redacted form.
func (s Secret) GoString() string { return fmt.Sprintf("%q", s.redacted()) }

// Format gives the redacted form for every verb.
func (s Secret) Format(f fmt.State, verb rune) {
	if verb == 'q' || (verb == 'v' && f.Flag('#')) {
		fmt.Fprintf(f, "%q", s.redacted())
		return
	}
	fmt.Fprint(f, s.redacted())
}

// MarshalText gives the redacted form.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.redacted()), nil }

// LogValue gives the redacted form.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.redacted()) }
