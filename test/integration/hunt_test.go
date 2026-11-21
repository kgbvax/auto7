// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/test/integration/peers"
)

// Invented calls and grids of the hunt tests.
const (
	ownCall = "XY9ZZ"
	ownGrid = "JJ00"
	dxCall  = "XX1AA"
	dxGrid  = "JJ11"
)

// periodFT4 is the T/R period of the hunt tests. FT4 keeps the tests
// short.
const periodFT4 = 7500 * time.Millisecond

// dialHz is the 20 m FT4 dial of the fake WSJT-X.
const dialHz = 14_080_000

// rig drives the fake WSJT-X like a real WSJT-X with ws-hunter. It sends
// a Heartbeat and a Status every second. A Reply sets the DX call and
// turns Enable Tx on, and HaltTx turns Enable Tx off, as WSJT-X does.
type rig struct {
	t *testing.T
	w *peers.WSJTX

	mu   sync.Mutex
	st   peers.Status
	seen int
	// holdDial keeps the old dial after SetDialFrequency until
	// releaseDial. heldDial is the dial that WSJT-X was asked to set.
	holdDial bool
	heldDial uint64
	stop     chan struct{}
	done     chan struct{}
}

func newRig(t *testing.T, w *peers.WSJTX, st peers.Status) *rig {
	r := &rig{t: t, w: w, st: st, stop: make(chan struct{}), done: make(chan struct{})}
	go r.loop()
	t.Cleanup(r.close)
	return r
}

// baseStatus is a Status of WSJT-X on 20 m FT4 with the own call.
func baseStatus() peers.Status {
	return peers.Status{
		DialHz: dialHz, Mode: "FT4", TxMode: "FT4", TxDF: 1500, RxDF: 1500,
		DECall: ownCall, DEGrid: ownGrid,
		FreqTolerance: 0xffffffff, TRPeriod: 0xffffffff,
	}
}

func (r *rig) close() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
}

func (r *rig) loop() {
	defer close(r.done)
	beat := time.NewTicker(time.Second)
	defer beat.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	r.send()
	for {
		select {
		case <-r.stop:
			return
		case <-beat.C:
			r.send()
		case <-poll.C:
			if r.react() {
				r.sendStatus()
			}
		}
	}
}

// send sends a Heartbeat of a ws-hunter build and the Status.
func (r *rig) send() {
	_ = r.w.SendHeartbeat(peers.RevisionHunter)
	r.sendStatus()
}

func (r *rig) sendStatus() {
	r.mu.Lock()
	st := r.st
	r.mu.Unlock()
	_ = r.w.SendStatus(st)
}

// react applies the new commands to the Status. It reports whether the
// Status changed.
func (r *rig) react() bool {
	cmds := r.w.Commands()
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, c := range cmds[r.seen:] {
		switch c.Type {
		case peers.TypeReply:
			f := strings.Fields(c.Text)
			from := f[1]
			if f[0] == "CQ" && len(f) > 3 {
				from = f[2]
			}
			r.st.DXCall, r.st.TxEnabled = from, true
			r.st.TxMessage = from + " " + ownCall + " " + ownGrid
			changed = true
		case peers.TypeHaltTx:
			r.st.TxEnabled, r.st.Transmitting = false, false
			changed = true
		case peers.TypeSetDialFrequency:
			// WSJT-X moves the radio by CAT and reports the new dial,
			// unless the test holds the dial.
			if r.holdDial {
				r.heldDial = c.Number
			} else {
				r.st.DialHz = c.Number
				changed = true
			}
		case peers.TypeConfigure:
			if c.Text != "" && c.Text != r.st.Mode {
				r.st.Mode, r.st.TxMode = c.Text, c.Text
				changed = true
			}
		}
	}
	r.seen = len(cmds)
	return changed
}

// set changes the Status and sends it at once.
func (r *rig) set(fn func(*peers.Status)) {
	r.mu.Lock()
	fn(&r.st)
	r.mu.Unlock()
	r.sendStatus()
}

// releaseDial applies the held dial and sends the Status with it. It
// returns the time just before the send.
func (r *rig) releaseDial() time.Time {
	r.mu.Lock()
	r.holdDial = false
	if r.heldDial != 0 {
		r.st.DialHz = r.heldDial
	}
	r.mu.Unlock()
	at := time.Now()
	r.sendStatus()
	return at
}

// count gives the number of received commands of the type.
func (r *rig) count(typ uint32) int {
	n := 0
	for _, c := range r.w.Commands() {
		if c.Type == typ {
			n++
		}
	}
	return n
}

// waitCount waits until the number of commands of the type reaches n.
func (r *rig) waitCount(typ uint32, n int, timeout time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(timeout)
	for r.count(typ) < n {
		if time.Now().After(deadline) {
			r.t.Fatalf("command type %d: %d within %s, want %d", typ, r.count(typ), timeout, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// decode sends a decode of the current slot.
func (r *rig) decode(msg string, df uint32) {
	r.t.Helper()
	now := time.Now().UTC()
	err := r.w.SendDecode(peers.Decode{
		New: true, Time: slotStart(now), SNR: -5, DeltaTime: 0.1, DeltaFreq: df, Mode: "+", Message: msg,
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// slotStart gives the start of the FT4 slot that holds t.
func slotStart(t time.Time) time.Time {
	day := t.UTC().Truncate(24 * time.Hour)
	off := t.Sub(day)
	return day.Add(off - off%periodFT4)
}

// waitSlot sleeps until 1 s into the next FT4 slot, so a decode that the
// test sends falls well inside one slot.
func waitSlot() {
	now := time.Now()
	time.Sleep(time.Until(slotStart(now).Add(periodFT4 + time.Second)))
}

// proc is one running auto7 process.
type proc struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *syncBuffer
	page string
	done chan error
}

// syncBuffer is a bytes.Buffer that is safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// freeTCP returns a loopback TCP address that is free at the time of the
// call.
func freeTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startAuto7 writes a configuration for the env and starts auto7 with it.
// It returns when the operator page answers.
func startAuto7(t *testing.T, e *Env) *proc {
	t.Helper()
	return startAuto7With(t, e, `[[bands]]
name = "20m"
modes = ["FT8", "FT4"]
`)
}

// startAuto7With starts auto7 with the common configuration and the TOML
// text extra after it. It returns when the operator page answers.
func startAuto7With(t *testing.T, e *Env, extra string) *proc {
	t.Helper()
	dir := t.TempDir()
	page := freeTCP(t)
	cfg := fmt.Sprintf(`site = %q
station = %q
host = %q

[mqtt]
broker = %q
client_id = "it-auto7-%s"

[wsjtx]
id = "WSJT-X"
listen = %q

[page]
listen = %q

[presence]
hosts = [%q]

`, testSite, testStation, testHost, e.Link.URL(), strings.ReplaceAll(t.Name(), "/", "-"), e.WSJTXListen, page, testHost) + extra
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &proc{t: t, out: &syncBuffer{}, page: "http://" + page, done: make(chan error, 1)}
	p.cmd = exec.Command(auto7Bin, "-config", path)
	p.cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_STATE_HOME="+filepath.Join(dir, "state"))
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-p.done:
			case <-time.After(10 * time.Second):
				_ = p.cmd.Process.Kill()
				<-p.done
			}
		}
		if t.Failed() {
			t.Logf("auto7 output:\n%s", p.out.String())
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(p.page + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		select {
		case err := <-p.done:
			t.Fatalf("auto7 exited: %v\n%s", err, p.out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the page does not answer:\n%s", p.out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pageText gets the operator page.
func (p *proc) pageText() string {
	p.t.Helper()
	resp, err := http.Get(p.page + "/")
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatal(err)
	}
	return string(b)
}

var tokenRE = regexp.MustCompile(`name="token" value="([^"]+)"`)

// post sends the form of the page to path, like the browser of the
// operator. It returns the status code and the answer text.
func (p *proc) post(path string) (int, string) {
	p.t.Helper()
	m := tokenRE.FindStringSubmatch(p.pageText())
	if m == nil {
		p.t.Fatal("the page has no token")
	}
	req, err := http.NewRequest(http.MethodPost, p.page+path, strings.NewReader(url.Values{"token": {m[1]}}.Encode()))
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", p.page)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// enable posts Enable until auto7 accepts it. The inputs can need some
// seconds to become live.
func (p *proc) enable(timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		code, text := p.post("/enable")
		if code == http.StatusSeeOther {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("Enable: %d %s\n%s", code, text, p.out.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitPage waits until the page holds want.
func (p *proc) waitPage(want string, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		text := p.pageText()
		if strings.Contains(text, want) {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("the page lacks %q:\n%s", want, text)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// liveInputs makes chase-queue and presence live. The chase-queue state
// marks dxCall as needed on 20 m.
func liveInputs(t *testing.T, e *Env) {
	t.Helper()
	if err := e.ChaseQueue.SetStatus("online"); err != nil {
		t.Fatal(err)
	}
	err := e.ChaseQueue.Set(map[string]any{
		"seq": 1, "activity": "dx",
		"ranked": []any{map[string]any{"row_id": 1, "call": dxCall, "band": "20m", "needed": true, "tag": "need"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.ChaseQueue.StartHeartbeat(time.Second)
	if err := e.Presence.Start(time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.Presence.Stop()
		e.ChaseQueue.StopHeartbeat()
		_ = e.ChaseQueue.Clear()
		topics := bus.TopicsFor(testSite, testStation, testHost)
		for _, topic := range []string{topics.State, topics.Status, topics.Candidates} {
			e.Conn.Publish(topic, 1, true, []byte{}).WaitTimeout(time.Second)
		}
	})
}

// callDX sends a needed CQ of dxCall and waits for the Reply.
func callDX(t *testing.T, r *rig) {
	t.Helper()
	waitSlot()
	n := r.count(peers.TypeReply)
	r.decode("CQ "+dxCall+" "+dxGrid, 1200)
	r.waitCount(peers.TypeReply, n+1, 3*time.Second)
	c := r.w.Commands()
	for i := len(c) - 1; i >= 0; i-- {
		if c[i].Type == peers.TypeReply {
			if c[i].Text != "CQ "+dxCall+" "+dxGrid {
				t.Fatalf("Reply to %q", c[i].Text)
			}
			return
		}
	}
}

func TestStartsStoppedAndSendsNothingWhileWSJTXTransmits(t *testing.T) {
	e := newEnv(t)
	liveInputs(t, e)
	st := baseStatus()
	st.TxEnabled, st.Transmitting, st.DXCall = true, true, dxCall
	r := newRig(t, e.WSJTX, st)
	p := startAuto7(t, e)
	// Several Heartbeats and Status messages and a needed CQ reach auto7.
	time.Sleep(3 * time.Second)
	r.decode("CQ "+dxCall+" "+dxGrid, 1200)
	time.Sleep(3 * time.Second)
	if c := e.WSJTX.Commands(); len(c) != 0 {
		t.Fatalf("auto7 sent %d commands while stopped, first type %d", len(c), c[0].Type)
	}
	p.waitPage("<strong>Stopped</strong>", time.Second)
}

func TestHuntWorksANeededStationOnce(t *testing.T) {
	e := newEnv(t)
	liveInputs(t, e)
	qso := watch(t, e, bus.TopicsFor(testSite, testStation, testHost).QSO)
	r := newRig(t, e.WSJTX, baseStatus())
	p := startAuto7(t, e)
	p.enable(20 * time.Second)
	p.waitPage("<strong>Enabled</strong>", 5*time.Second)
	if r.count(peers.TypeSetHoldTxFreq) == 0 {
		t.Error("Enable sent no SetHoldTxFreq to the ws-hunter build")
	}

	callDX(t, r)
	// The partner answers in the next slot, and WSJT-X runs the QSO.
	waitSlot()
	r.decode(ownCall+" "+dxCall+" -10", 1200)
	p.waitPage("QSO "+dxCall, 3*time.Second)
	now := time.Now().UTC()
	err := e.WSJTX.SendQSOLogged(peers.QSOLogged{
		TimeOff: now, TimeOn: now.Add(-30 * time.Second), DXCall: dxCall, DXGrid: dxGrid,
		TxFreqHz: dialHz + 1500, Mode: "FT4", ReportSent: "-05", ReportRcvd: "-10",
		MyCall: ownCall, MyGrid: ownGrid,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-qso:
		var q struct {
			Call, Band, Mode string
			Host             string
		}
		if err := json.Unmarshal([]byte(payload), &q); err != nil {
			t.Fatal(err)
		}
		if q.Call != dxCall || q.Band != "20m" || q.Mode != "FT4" || q.Host != testHost {
			t.Errorf("QSO message %s", payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no message on the QSO topic")
	}

	// The same station calls CQ again: auto7 worked it on this band.
	replies := r.count(peers.TypeReply)
	waitSlot()
	r.decode("CQ "+dxCall+" "+dxGrid, 1200)
	time.Sleep(2 * periodFT4)
	if got := r.count(peers.TypeReply); got != replies {
		t.Errorf("auto7 called %s again after the QSO: %d replies, want %d", dxCall, got, replies)
	}
}

func TestLinkAndPresenceLossStopsUntilEnable(t *testing.T) {
	e := newEnv(t)
	liveInputs(t, e)
	r := newRig(t, e.WSJTX, baseStatus())
	p := startAuto7(t, e)
	p.enable(20 * time.Second)
	p.waitPage("<strong>Enabled</strong>", 5*time.Second)

	halts := r.count(peers.TypeHaltTx)
	e.Link.Cut()
	e.Presence.Stop()
	r.waitCount(peers.TypeHaltTx, halts+1, 5*time.Second)
	for _, c := range r.w.Commands() {
		if c.Type == peers.TypeHaltTx && c.Flag {
			t.Error("HaltTx with auto only true: the stop must end the transmission at once")
		}
	}
	p.waitPage("bus down", 5*time.Second)

	e.Link.Restore()
	if err := e.Presence.Start(time.Second); err != nil {
		t.Fatal(err)
	}
	// Four periods with a needed CQ in each: auto7 stays stopped.
	replies := r.count(peers.TypeReply)
	for range 4 {
		waitSlot()
		r.decode("CQ "+dxCall+" "+dxGrid, 1200)
	}
	time.Sleep(time.Second)
	if got := r.count(peers.TypeReply); got != replies {
		t.Fatalf("auto7 called after the inputs came back: %d replies, want %d", got, replies)
	}
	p.waitPage("<strong>Stopped</strong>", time.Second)

	p.enable(30 * time.Second)
	callDX(t, r)
}

func TestStopOnTheCommandTopic(t *testing.T) {
	e := newEnv(t)
	liveInputs(t, e)
	r := newRig(t, e.WSJTX, baseStatus())
	p := startAuto7(t, e)
	p.enable(20 * time.Second)
	p.waitPage("<strong>Enabled</strong>", 5*time.Second)

	halts := r.count(peers.TypeHaltTx)
	cmd := bus.TopicsFor(testSite, testStation, testHost).Cmd
	if tok := e.Conn.Publish(cmd, 1, false, []byte(`{"action":"stop"}`)); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("publish stop: %v", tok.Error())
	}
	r.waitCount(peers.TypeHaltTx, halts+1, 5*time.Second)
	p.waitPage("<strong>Stopped</strong>", 5*time.Second)
	p.waitPage("stop command on the bus", time.Second)
}

func TestSIGTERMDuringACallSendsHaltTx(t *testing.T) {
	e := newEnv(t)
	liveInputs(t, e)
	r := newRig(t, e.WSJTX, baseStatus())
	p := startAuto7(t, e)
	p.enable(20 * time.Second)
	callDX(t, r)
	r.set(func(s *peers.Status) { s.Transmitting = true })

	halts := r.count(peers.TypeHaltTx)
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		p.done <- err
		if err != nil {
			t.Errorf("auto7 exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("auto7 did not exit after SIGTERM")
	}
	// auto7 sent HaltTx before it exited. The fake reads it from its
	// socket.
	r.waitCount(peers.TypeHaltTx, halts+1, 2*time.Second)
	for _, c := range r.w.Commands() {
		if c.Type == peers.TypeHaltTx && c.Flag {
			t.Error("HaltTx with auto only true at exit")
		}
	}
}
