// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"bytes"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// hx decodes hex text. Spaces only group the bytes for the reader.
func hx(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(strings.Join(parts, ""), " ", ""))
	if err != nil {
		t.Fatalf("bad hex in test: %v", err)
	}
	return b
}

// head is the framing of a command with the Id "WSJT-X": magic, schema 3,
// the type and the Id as a QByteArray.
func head(typ string) string {
	return "adbccbda 00000003 " + typ + " 00000006 57534a542d58 "
}

func TestEncoders(t *testing.T) {
	const id = "WSJT-X"
	dec := wsjtx.Decode{
		ID:        id,
		New:       true,
		Time:      12*time.Hour + 34*time.Minute + 45*time.Second,
		SNR:       -7,
		DeltaTime: 0.2,
		DeltaFreq: 1234,
		Mode:      "~",
		Message:   "CQ XX1AA KK11",
	}
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"Reply", Reply(id, dec, 0), hx(t, head("00000004"),
			"02b2fe88", "fffffff9", "3fc999999999999a", "000004d2",
			"00000001 7e", "0000000d 4351205858314141204b4b3131", "00", "00")},
		{"Reply with a null time and a modifier", Reply(id, wsjtx.Decode{Time: -1, Mode: "~", Message: "A", LowConfidence: true}, 0x02), hx(t, head("00000004"),
			"ffffffff", "00000000", "0000000000000000", "00000000",
			"00000001 7e", "00000001 41", "01", "02")},
		{"HaltTx at once", HaltTx(id, false), hx(t, head("00000008"), "00")},
		{"HaltTx auto only", HaltTx(id, true), hx(t, head("00000008"), "01")},
		{"FreeText", FreeText(id, "CQ DX XX0OP JJ00", true), hx(t, head("00000009"),
			"00000010 4351204458205858304f50204a4a3030", "01")},
		{"Configure mode only", Configure(id, ModeOnly("FT4")), hx(t, head("0000000f"),
			"00000003 465434", "ffffffff", "00000000", "00", "ffffffff", "ffffffff",
			"00000000", "00000000", "00")},
		{"Configure all fields", Configure(id, Configuration{Mode: "FT4", FreqTolerance: 50, SubMode: "A", FastMode: true,
			TRPeriod: 7, RxDF: 1500, DXCall: "XY3CC", DXGrid: "KK11", Generate: true}), hx(t, head("0000000f"),
			"00000003 465434", "00000032", "00000001 41", "01", "00000007", "000005dc",
			"00000005 5859334343", "00000004 4b4b3131", "01")},
		{"HighlightCallsign", HighlightCallsign(id, "XX1AA", RGB(255, 0, 0x10), Color{}, true), hx(t, head("0000000d"),
			"00000005 5858314141",
			"01 ffff ffff 0000 1010 0000",
			"00 ffff 0000 0000 0000 0000",
			"01")},
		{"SetDialFrequency", SetDialFrequency(id, 14_074_000), hx(t, head("00000014"), "0000000000d6c090")},
		{"SetTxDF", SetTxDF(id, 1500), hx(t, head("00000018"), "000005dc")},
		{"SetHoldTxFreq", SetHoldTxFreq(id, true), hx(t, head("00000019"), "01")},
		{"SetTxEnabled off", SetTxEnabled(id, false), hx(t, head("0000001a"), "00")},
		{"SetTxEnabled on", SetTxEnabled(id, true), hx(t, head("0000001a"), "01")},
		{"ClearDX", ClearDX(id), hx(t, head("0000001c"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.got, tc.want) {
				t.Errorf("bytes\n got %x\nwant %x", tc.got, tc.want)
			}
		})
	}
}

// fakeConn records each datagram and its address.
type fakeConn struct {
	to   []netip.AddrPort
	sent [][]byte
	err  error
}

func (c *fakeConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.to = append(c.to, addr)
	c.sent = append(c.sent, append([]byte(nil), b...))
	return len(b), nil
}

func TestSenderSendsToThePeer(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:52001")
	c := &fakeConn{}
	s := New(c, "WSJT-X", func() (netip.AddrPort, bool) { return peer, true })
	if err := s.HaltTx(false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTxDF(1500); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 2 || c.to[0] != peer || c.to[1] != peer {
		t.Fatalf("sent %d datagrams to %v, want 2 to %v", len(c.sent), c.to, peer)
	}
	if !bytes.Equal(c.sent[0], HaltTx("WSJT-X", false)) || !bytes.Equal(c.sent[1], SetTxDF("WSJT-X", 1500)) {
		t.Errorf("wrong bytes: %x %x", c.sent[0], c.sent[1])
	}
}

func TestSenderWithoutPeer(t *testing.T) {
	c := &fakeConn{}
	s := New(c, "WSJT-X", func() (netip.AddrPort, bool) { return netip.AddrPort{}, false })
	if err := s.HaltTx(false); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("err %v, want ErrNoPeer", err)
	}
	if s.HasPeer() || len(c.sent) != 0 {
		t.Fatal("sent without a peer")
	}
}

func TestSenderRefusesLongCommand(t *testing.T) {
	c := &fakeConn{}
	s := New(c, "WSJT-X", func() (netip.AddrPort, bool) { return netip.MustParseAddrPort("127.0.0.1:52001"), true })
	if err := s.FreeText(strings.Repeat("A", MaxDatagram), false); !errors.Is(err, ErrTooLong) {
		t.Fatalf("err %v, want ErrTooLong", err)
	}
	if len(c.sent) != 0 {
		t.Fatal("sent a long command")
	}
}

// TestSenderOverUDP sends a real datagram over the loopback interface
// and reads it with the WSJT-X decoder header layout.
func TestSenderOverUDP(t *testing.T) {
	rx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	tx, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	peer := rx.LocalAddr().(*net.UDPAddr).AddrPort()
	s := New(tx, "WSJT-X", func() (netip.AddrPort, bool) { return peer, true })
	if err := s.ClearDX(); err != nil {
		t.Fatal(err)
	}
	if err := rx.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	n, _, err := rx.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	if want := hx(t, head("0000001c")); !bytes.Equal(buf[:n], want) {
		t.Errorf("got %x, want %x", buf[:n], want)
	}
}
