// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Package integration runs auto7 against a Mosquitto broker in a container
// and the fake peers of package peers. Run the tests with run.sh.
package integration

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kgbvax/auto7/test/integration/peers"
)

// auto7Bin is the path of the auto7 binary that TestMain builds.
var auto7Bin string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "auto7-it-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	auto7Bin = filepath.Join(dir, "auto7")
	build := exec.Command("go", "build", "-race", "-o", auto7Bin, "github.com/kgbvax/auto7/cmd/auto7")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build cmd/auto7:", err)
		return 1
	}
	return m.Run()
}

// Env is the test setup of one test: the broker link, the peers and a free
// UDP address for auto7.
type Env struct {
	Broker        string
	Link          *peers.LinkProxy
	Conn          *peers.Conn
	ChaseQueue    *peers.ChaseQueue
	Presence      *peers.Presence
	Horstreporter *peers.Horstreporter
	WSJTX         *peers.WSJTX
	// WSJTXListen is the loopback UDP address on which auto7 listens.
	WSJTXListen string
}

// Site and station of the test topics, and the host of auto7.
const (
	testSite    = "itsite"
	testStation = "hf"
	testHost    = "ithost"
)

// newEnv starts the peers. auto7 must use env.Link.URL() as the broker, so
// that a test can cut its link.
func newEnv(t *testing.T) *Env {
	t.Helper()
	brokerURL := os.Getenv(peers.BrokerEnv)
	if brokerURL == "" {
		t.Fatalf("%s is not set: run test/integration/run.sh", peers.BrokerEnv)
	}
	u, err := url.Parse(brokerURL)
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{Broker: brokerURL}
	if e.Link, err = peers.NewLinkProxy(u.Host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Link.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e.Conn, err = peers.Connect(ctx, brokerURL, "it-peers-"+t.Name()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Conn.Close)
	if e.ChaseQueue, err = peers.NewChaseQueue(e.Conn.Client, testSite, testStation, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.ChaseQueue.Close)
	e.Presence = peers.NewPresence(e.Conn.Client, peers.PresenceTopic(testSite, testStation), testHost, nil)
	t.Cleanup(e.Presence.Stop)
	e.Horstreporter = peers.NewHorstreporter()
	t.Cleanup(e.Horstreporter.Close)

	e.WSJTXListen = freeUDP(t)
	if e.WSJTX, err = peers.NewWSJTX("WSJT-X", e.WSJTXListen); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.WSJTX.Close() })
	return e
}

// freeUDP returns a loopback UDP address that is free at the time of the
// call.
func freeUDP(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().String()
}

// runAuto7 runs the auto7 binary with the arguments until it ends or the
// timeout ends. It returns the combined output.
func runAuto7(t *testing.T, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, auto7Bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSmoke proves that the broker and the peers start, and that the
// built binary runs. The other integration tests run auto7 for real.
func TestSmoke(t *testing.T) {
	newEnv(t)
	out, err := runAuto7(t, 30*time.Second, "-h")
	if err != nil {
		t.Fatalf("auto7 -h: %v\n%s", err, out)
	}
	if !strings.Contains(out, "AUTO7_MQTT_PASSWORD") {
		t.Errorf("auto7 -h does not name AUTO7_MQTT_PASSWORD:\n%s", out)
	}
}
