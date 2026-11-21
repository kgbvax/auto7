// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// write stores a configuration file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPasswordKeyIsRefusedWithoutTheValue(t *testing.T) {
	for _, body := range []string{
		"[mqtt]\npassword = \"hunter-secret-1\"\n",
		"[horstreporter]\npassword = \"hunter-secret-1\"\n",
	} {
		_, err := Load(write(t, body), false)
		if err == nil {
			t.Fatalf("%q: accepted", body)
		}
		if !strings.Contains(err.Error(), "password") {
			t.Errorf("error %q does not name the key", err)
		}
		if strings.Contains(err.Error(), "hunter-secret-1") {
			t.Errorf("error %q shows the value", err)
		}
	}
}

func TestBrokerNeedsTLSOnAPublicAddress(t *testing.T) {
	for _, broker := range []string{
		"tcp://203.0.113.7:1883",
		"tcp://broker.example.org:1883",
		"mqtt://198.51.100.1:1883",
	} {
		if _, err := Load(write(t, "[mqtt]\nbroker = \""+broker+"\"\n"), false); err == nil {
			t.Errorf("%s: accepted", broker)
		} else if !strings.Contains(err.Error(), "mqtt.broker") {
			t.Errorf("%s: error %q does not name mqtt.broker", broker, err)
		}
	}
	for _, broker := range []string{
		"tcp://127.0.0.1:1883",
		"tcp://localhost:1883",
		"tcp://[::1]:1883",
		"tcp://10.1.2.3:1883",
		"tcp://192.168.7.8:1883",
		"tcp://172.20.0.1:1883",
		"tcp://100.101.102.103:1883",
		"ssl://203.0.113.7:8883",
		"ssl://broker.example.org:8883",
	} {
		if _, err := Load(write(t, "[mqtt]\nbroker = \""+broker+"\"\n"), false); err != nil {
			t.Errorf("%s: %v", broker, err)
		}
	}
}

func TestBrokerURLWithUserIsRefused(t *testing.T) {
	_, err := Load(write(t, "[mqtt]\nbroker = \"tcp://xx1aa:pw-in-url@127.0.0.1:1883\"\n"), false)
	if err == nil {
		t.Fatal("accepted")
	}
	if strings.Contains(err.Error(), "pw-in-url") {
		t.Errorf("error %q shows the password", err)
	}
}

func TestBandList(t *testing.T) {
	for name, body := range map[string]string{
		"60 m":        "[[bands]]\nname = \"60m\"\nmodes = [\"FT8\"]\n",
		"FT4 on 160m": "[[bands]]\nname = \"160m\"\nmodes = [\"FT8\", \"FT4\"]\n",
		"unknown":     "[[bands]]\nname = \"11m\"\nmodes = [\"FT8\"]\n",
		"bad mode":    "[[bands]]\nname = \"20m\"\nmodes = [\"JT65\"]\n",
		"no modes":    "[[bands]]\nname = \"20m\"\n",
		"twice":       "[[bands]]\nname = \"20m\"\nmodes = [\"FT8\"]\n[[bands]]\nname = \"20m\"\nmodes = [\"FT4\"]\n",
	} {
		_, err := Load(write(t, body), false)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg, err := Load(write(t, "[[bands]]\nname = \"160m\"\nmodes = [\"ft8\"]\n[[bands]]\nname = \"20M\"\nmodes = [\"FT8\", \"FT4\"]\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Band{{Name: "160m", Modes: []string{"FT8"}}, {Name: "20m", Modes: []string{"FT8", "FT4"}}}
	if fmt.Sprint(cfg.Bands) != fmt.Sprint(want) {
		t.Errorf("bands = %v, want %v", cfg.Bands, want)
	}
}

func TestLimitOutsideItsRangeNamesTheLimit(t *testing.T) {
	cases := map[string][]int{
		"tries":                  {0, 11},
		"tries_high_value":       {0, 31},
		"skip_minutes":           {0, 241},
		"cq_max_periods":         {0, 11},
		"cq_listen_periods":      {3, 21},
		"offset_moves_per_hour":  {-1, 7},
		"offset_min_distance_hz": {49, 1001},
		"not_heard_minutes":      {4, 121},
	}
	for key, values := range cases {
		for _, v := range values {
			_, err := Load(write(t, fmt.Sprintf("[limits]\n%s = %d\n", key, v)), false)
			if err == nil {
				t.Errorf("limits.%s = %d: accepted", key, v)
				continue
			}
			if !strings.Contains(err.Error(), "limits."+key) {
				t.Errorf("limits.%s = %d: error %q does not name the limit", key, v, err)
			}
		}
	}
}

func TestMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"), true)
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{
		Tries:               4,
		TriesHighValue:      12,
		SkipMinutes:         10,
		CQMaxPeriods:        10,
		CQListenPeriods:     4,
		OffsetMovesPerHour:  6,
		OffsetMinDistanceHz: 50,
		NotHeardMinutes:     30,
	}
	if cfg.Limits != want {
		t.Errorf("limits = %+v, want %+v", cfg.Limits, want)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	host = strings.ToLower(strings.SplitN(host, ".", 2)[0])
	if len(cfg.Presence.Hosts) != 1 || cfg.Presence.Hosts[0] != host {
		t.Errorf("presence hosts = %v, want [%s]", cfg.Presence.Hosts, host)
	}
	if cfg.Host != host {
		t.Errorf("host = %q, want %q", cfg.Host, host)
	}
}

func TestMissingFileIsAnErrorWhenRequired(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml"), false); err == nil {
		t.Fatal("accepted")
	}
}

func TestPasswordComesFromTheEnvironmentAndNeverShows(t *testing.T) {
	t.Setenv("AUTO7_MQTT_PASSWORD", "env-secret-7")
	cfg, err := Load(write(t, "[mqtt]\nuser = \"auto7\"\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password.Reveal() != "env-secret-7" {
		t.Fatalf("password = %q", cfg.MQTT.Password.Reveal())
	}
	js, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%s", cfg.MQTT.Password), fmt.Sprintf("%q", cfg.MQTT.Password),
		string(js),
	} {
		if strings.Contains(s, "env-secret-7") {
			t.Errorf("text form shows the password: %s", s)
		}
	}
}

func TestLoopbackAddresses(t *testing.T) {
	for name, body := range map[string]string{
		"wsjtx":     "[wsjtx]\nlisten = \"192.168.1.5:2237\"\n",
		"page":      "[page]\nlisten = \"0.0.0.0:8077\"\n",
		"page port": "[page]\nlisten = \"127.0.0.1\"\n",
	} {
		if _, err := Load(write(t, body), false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFoxHoundDials(t *testing.T) {
	cfg, err := Load(write(t, "fox_hound_dials_hz = [14090000, 7056000]\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cfg.FoxHoundDialsHz) != "[14090000 7056000]" {
		t.Errorf("dials = %v", cfg.FoxHoundDialsHz)
	}
	if _, err := Load(write(t, "fox_hound_dials_hz = [0]\n"), false); err == nil {
		t.Error("dial 0 accepted")
	}
}

func TestExampleLoadsAndHoldsOnlyInventedCallsAndNoHost(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, false); err != nil {
		t.Fatalf("example does not load: %v", err)
	}
	text := string(data)

	invented := regexp.MustCompile(`^(XX|XY)[0-9][A-Z]{1,3}$`)
	grid := regexp.MustCompile(`^[A-R]{2}[0-9]{2}([A-X]{2})?$`)
	call := regexp.MustCompile(`\b[A-Z0-9]?[A-Z][A-Z0-9]?[0-9][A-Z]{1,4}\b`)
	for _, m := range call.FindAllString(strings.ToUpper(text), -1) {
		if !invented.MatchString(m) && !grid.MatchString(m) {
			t.Errorf("call %q is not on the invented list", m)
		}
	}
	ipv4 := regexp.MustCompile(`\b[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b`)
	for _, m := range ipv4.FindAllString(text, -1) {
		if m != "127.0.0.1" {
			t.Errorf("host address %q in the example", m)
		}
	}
	hostInURL := regexp.MustCompile(`://([^/:"\s]+)`)
	for _, m := range hostInURL.FindAllStringSubmatch(text, -1) {
		if m[1] != "127.0.0.1" && m[1] != "localhost" {
			t.Errorf("host %q in the example", m[1])
		}
	}
}
