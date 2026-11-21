// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package peers

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// broker returns the URL of the test broker from BrokerEnv.
func broker(t *testing.T) string {
	t.Helper()
	b := os.Getenv(BrokerEnv)
	if b == "" {
		t.Fatalf("%s is not set: run test/integration/run.sh", BrokerEnv)
	}
	return b
}

func connect(t *testing.T, brokerURL, id string) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, brokerURL, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// datagram builds a WSJT-X datagram by hand, field by field.
func datagram(typ uint32, id string, fields ...any) []byte {
	var b bytes.Buffer
	for _, f := range append([]any{uint32(0xadbccbda), uint32(3), typ, uint32(len(id)), []byte(id)}, fields...) {
		switch v := f.(type) {
		case []byte:
			b.Write(v)
		default:
			_ = binary.Write(&b, binary.BigEndian, v)
		}
	}
	return b.Bytes()
}

func TestWSJTXRecordsHaltTxAndSetTxDF(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	w, err := NewWSJTX("WSJT-X", server.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.SendHeartbeat(RevisionHunter); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, from, err := server.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := datagram(0, "WSJT-X", uint32(3), uint32(5), []byte("2.7.0"), uint32(13), []byte("a1b2c3-hunter"))
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("heartbeat\n got % x\nwant % x", buf[:n], want)
	}

	for _, d := range [][]byte{
		datagram(8, "WSJT-X", uint8(1)),
		datagram(24, "WSJT-X", uint32(1500)),
	} {
		if _, err := server.WriteToUDP(d, from); err != nil {
			t.Fatal(err)
		}
	}
	halt, err := w.WaitCommand(TypeHaltTx, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if halt.ID != "WSJT-X" || !halt.Flag || halt.Err != nil {
		t.Errorf("HaltTx = %+v, want Id WSJT-X and Auto Tx Only true", halt)
	}
	df, err := w.WaitCommand(TypeSetTxDF, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if df.Number != 1500 || df.Err != nil {
		t.Errorf("SetTxDF = %+v, want 1500 Hz", df)
	}
	if got := len(w.Commands()); got != 2 {
		t.Errorf("recorded %d commands, want 2", got)
	}
}

func TestHorstreporterFailsOnRequest(t *testing.T) {
	h := NewHorstreporter()
	defer h.Close()
	if err := h.SetResponse("/api/hot_bands", map[string]any{"bands": []string{"20m"}}); err != nil {
		t.Fatal(err)
	}
	u, err := url.JoinPath(h.URL(), "/api/hot_bands")
	if err != nil {
		t.Fatal(err)
	}
	get := func() int {
		r, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	h.SetFailing(true)
	if code := get(); code < 500 {
		t.Errorf("status %d while failing, want an error status", code)
	}
	if n := len(h.Requests()); n != 2 {
		t.Errorf("recorded %d requests, want 2", n)
	}
}

func TestCutDropsTheClientConnection(t *testing.T) {
	b, err := url.Parse(broker(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewLinkProxy(b.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := connect(t, p.URL(), "peers-test-cut")
	if !c.IsConnectionOpen() {
		t.Fatal("not connected")
	}
	p.Cut()
	select {
	case <-c.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection did not drop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Connect(ctx, p.URL(), "peers-test-cut-2"); err == nil {
		t.Error("connected while the link is cut")
	}
	p.Restore()
	connect(t, p.URL(), "peers-test-cut-3")
}
