// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"

	"github.com/kgbvax/auto7/internal/config"
)

const (
	testSite     = "tsite"
	testStation  = "hf"
	testHost     = "thost"
	testPassword = "pw-not-for-logs-7781"
)

// testNow is the time of the fake clock.
var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// syncBuffer is a log buffer that many goroutines can write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	broker *fakeBroker
	client *Client
	topics Topics
	logs   *syncBuffer
}

func testConfig(broker string) config.Config {
	return config.Config{
		Site:    testSite,
		Station: testStation,
		Host:    testHost,
		MQTT: config.MQTTConfig{
			Broker:   broker,
			ClientID: "auto7-unit",
			User:     "auto7",
			Password: config.Secret(testPassword),
		},
	}
}

// newHarness starts a fake broker and a client. setup runs before the
// client connects, so it can store retained messages.
func newHarness(t *testing.T, setup func(b *fakeBroker, tp Topics)) *harness {
	t.Helper()
	h := &harness{broker: newFakeBroker(t), topics: TopicsFor(testSite, testStation, testHost), logs: &syncBuffer{}}
	if setup != nil {
		setup(h.broker, h.topics)
	}
	h.client = New(testConfig(h.broker.URL()), Options{
		Now:    func() time.Time { return testNow },
		Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.client.Close)
	return h
}

// next returns the next event of type T that pred accepts. It fails the
// test after 5 s.
func next[T Event](t *testing.T, ch <-chan Event, pred func(T) bool) T {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				var zero T
				t.Fatalf("event channel closed while waiting for %T", zero)
			}
			if e, ok := ev.(T); ok && (pred == nil || pred(e)) {
				return e
			}
		case <-timeout:
			var zero T
			t.Fatalf("no %T within 5 s", zero)
		}
	}
}

// eventually polls cond every few milliseconds and fails the test after
// 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 5 s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRetainedPresenceIsMarkedRetained(t *testing.T) {
	h := newHarness(t, func(b *fakeBroker, tp Topics) {
		b.SetRetained(tp.Presence, `{"ts":"2026-11-21T11:00:00Z","present":true,"host":"thost"}`)
	})
	ev := h.client.Events()

	p := next[Presence](t, ev, nil)
	if !p.Retained {
		t.Errorf("retained presence: Retained = false, want true")
	}
	if want := time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC); !p.TS.Equal(want) {
		t.Errorf("TS = %v, want %v", p.TS, want)
	}
	if !p.Received.Equal(testNow) {
		t.Errorf("Received = %v, want %v", p.Received, testNow)
	}
	if !p.Present || p.Host != "thost" || p.Conn != 1 {
		t.Errorf("presence = %+v, want present from thost on connection 1", p)
	}

	h.broker.Send(h.topics.Presence, `{"ts":"2026-11-21T11:59:30Z","present":true,"host":"thost"}`)
	live := next[Presence](t, ev, nil)
	if live.Retained {
		t.Errorf("live presence: Retained = true, want false")
	}
}

func TestChaseStateMarksATNOAndFirstTierExpedition(t *testing.T) {
	h := newHarness(t, nil)
	h.broker.Send(h.topics.ChaseState, `{"ts":"2026-11-21T11:58:00Z","seq":42,"activity":"dx","ranked":[
	 {"row_id":7,"call":"XX1AA","band":"20m","needed":true,"tag":"need","source":"bmrec","freq_hz":14074000,"mode":"data",
	  "needs":[{"award":"dxcc","key":"XX","band":"20m","status":"unworked","atno":true}]},
	 {"row_id":-3,"call":"XY3CC","band":"20m","needed":true,"tag":"need","source":"cluster","mode":"data",
	  "expedition":{"call":"XY3CC","mode_class":"data","tier":"first"}},
	 {"row_id":9,"call":"XX2BB","band":"20m","needed":true,"tag":"need","source":"bmrec",
	  "needs":[{"award":"dxcc","key":"XX","band":"20m","status":"worked-unconfirmed"}],
	  "expedition":{"call":"XX2BB","mode_class":"data","tier":"second"}}]}`)

	s := next[ChaseState](t, h.client.Events(), nil)
	if s.Seq != 42 || s.Activity != "dx" || len(s.Ranked) != 3 {
		t.Fatalf("chase state = %+v, want seq 42, dx, 3 rows", s)
	}
	if want := time.Date(2026, 11, 21, 11, 58, 0, 0, time.UTC); !s.TS.Equal(want) {
		t.Errorf("TS = %v, want %v", s.TS, want)
	}
	for _, tc := range []struct {
		call        string
		atno, first bool
	}{
		{"XX1AA", true, false},
		{"XY3CC", false, true},
		{"XX2BB", false, false},
	} {
		var row *ChaseRow
		for i := range s.Ranked {
			if s.Ranked[i].Call == tc.call {
				row = &s.Ranked[i]
			}
		}
		if row == nil {
			t.Errorf("%s: row missing", tc.call)
			continue
		}
		if row.ATNO() != tc.atno || row.FirstTierExpedition() != tc.first {
			t.Errorf("%s: ATNO %v first tier %v, want %v %v", tc.call, row.ATNO(), row.FirstTierExpedition(), tc.atno, tc.first)
		}
	}
	if s.Ranked[0].FreqHz != 14074000 || s.Ranked[0].Mode != "data" {
		t.Errorf("row XX1AA = %+v, want 14074000 Hz data", s.Ranked[0])
	}
}

func TestRetainedStopIsDropped(t *testing.T) {
	h := newHarness(t, func(b *fakeBroker, tp Topics) {
		b.SetRetained(tp.Cmd, `{"action":"stop","value":"old"}`)
	})
	h.broker.Send(h.topics.Cmd, `{"action":"stop","value":"operator"}`)
	s := next[StopCommand](t, h.client.Events(), nil)
	if s.Value != "operator" || s.Retained {
		t.Errorf("first stop = %+v, want the live stop from the operator", s)
	}
	if !strings.Contains(h.logs.String(), "retained") {
		t.Errorf("log does not name the dropped retained command:\n%s", h.logs)
	}
}

func TestOtherActionIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.broker.Send(h.topics.Cmd, `{"action":"enable","value":""}`)
	h.broker.Send(h.topics.Cmd, `{"action":"stop","value":"after"}`)
	s := next[StopCommand](t, h.client.Events(), nil)
	if s.Value != "after" {
		t.Errorf("first stop = %+v, want the stop after the refused enable", s)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "refused") || !strings.Contains(logs, "enable") {
		t.Errorf("log does not show the refused enable:\n%s", logs)
	}
}

func TestLostLinkReportsBusDown(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.client.Events()
	up := next[Link](t, ev, nil)
	if !up.Up || up.Conn != 1 {
		t.Fatalf("first link event = %+v, want up on connection 1", up)
	}

	h.broker.Cut()
	down := next[Link](t, ev, nil)
	if down.Up || down.Reason != BusDown || down.Conn != 1 {
		t.Errorf("link event after cut = %+v, want %q on connection 1", down, BusDown)
	}
	again := next[Link](t, ev, nil)
	if !again.Up || again.Conn != 2 {
		t.Errorf("link event after reconnect = %+v, want up on connection 2", again)
	}
	eventually(t, "status online after reconnect", func() bool {
		v, _ := h.broker.Retained(h.topics.Status)
		return v == StatusOnline
	})
	var sawWill bool
	for _, p := range h.broker.Published() {
		if p.Topic == h.topics.Status && p.Payload == StatusOffline && p.Retained {
			sawWill = true
		}
	}
	if !sawWill {
		t.Errorf("the broker sent no retained offline will on %s", h.topics.Status)
	}
}

func TestPublishFailsWhileBusDown(t *testing.T) {
	b := newFakeBroker(t)
	c := New(testConfig(b.URL()), Options{Now: func() time.Time { return testNow }})
	err := c.PublishState(context.Background(), State{Enabled: true})
	if err == nil || !strings.Contains(err.Error(), BusDown) {
		t.Errorf("publish before connect: err = %v, want %q", err, BusDown)
	}
}

func TestLoginErrorHidesPassword(t *testing.T) {
	for _, code := range []byte{packets.ErrRefusedBadUsernameOrPassword, packets.ErrRefusedNotAuthorised} {
		b := newFakeBroker(t)
		b.RefuseLogins(code)
		logs := &syncBuffer{}
		c := New(testConfig(b.URL()), Options{
			Now:    func() time.Time { return testNow },
			Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.Connect(ctx)
		cancel()
		c.Close()
		if err == nil {
			t.Fatalf("code %d: Connect succeeded, want a login error", code)
		}
		if l := b.Logins(); len(l) == 0 || string(l[0].Password) != testPassword {
			t.Fatalf("code %d: the broker got no login with the password", code)
		}
		if strings.Contains(err.Error(), testPassword) {
			t.Errorf("code %d: error shows the password: %v", code, err)
		}
		if strings.Contains(logs.String(), testPassword) {
			t.Errorf("code %d: log shows the password:\n%s", code, logs)
		}
		if !strings.Contains(err.Error(), "auto7") {
			t.Errorf("code %d: error %q does not name the user", code, err)
		}
	}
}

func TestPublishesOnOwnSlotOnly(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if v, _ := h.broker.Retained(h.topics.Status); v != StatusOnline {
		t.Errorf("status after connect = %q, want online", v)
	}
	if l := h.broker.Logins(); len(l) != 1 || l[0].WillTopic != h.topics.Status || string(l[0].WillMessage) != StatusOffline || !l[0].WillRetain {
		t.Errorf("will = %+v, want retained offline on %s", l, h.topics.Status)
	}

	if err := h.client.PublishState(ctx, State{Enabled: true, Band: "20m", Mode: "FT8", Task: "call XX1AA"}); err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	raw, ok := h.broker.Retained(h.topics.State)
	if !ok {
		t.Fatalf("no retained state on %s", h.topics.State)
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if st["host"] != testHost || st["enabled"] != true || st["ts"] != "2026-11-21T12:00:00Z" || st["task"] != "call XX1AA" {
		t.Errorf("state = %s, want host thost, enabled, ts of the clock, task", raw)
	}

	if err := h.client.PublishCandidates(ctx, []Candidate{{Call: "XX1AA", Band: "20m", Mode: "FT8", LastMsg: "CQ"}}); err != nil {
		t.Fatal(err)
	}
	raw, ok = h.broker.Retained(h.topics.Candidates)
	if !ok || !strings.Contains(raw, `"call":"XX1AA"`) || !strings.Contains(raw, `"last_msg":"CQ"`) {
		t.Errorf("retained candidates = %q, want XX1AA with last_msg CQ", raw)
	}
	if err := h.client.PublishCandidates(ctx, []Candidate{{Call: "XX2BB", Band: "20m", Mode: "CW"}}); err == nil {
		t.Errorf("a CW candidate was published, want a refusal")
	}

	q := QSO{Call: "XX2BB", Grid: "AA00", Band: "20m", FreqHz: 14074000, Mode: "FT8", TimeOn: testNow.Add(-time.Minute), TimeOff: testNow, RSTSent: "-10", RSTRcvd: "-12"}
	if err := h.client.PublishQSO(ctx, q); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.broker.Retained(h.topics.QSO); ok {
		t.Errorf("the QSO is retained, want a live message")
	}
	var sawQSO bool
	for _, p := range h.broker.Published() {
		if p.Topic == h.topics.QSO && strings.Contains(p.Payload, `"call":"XX2BB"`) && strings.Contains(p.Payload, `"time_off":"2026-11-21T12:00:00Z"`) {
			sawQSO = true
		}
	}
	if !sawQSO {
		t.Errorf("no QSO message on %s", h.topics.QSO)
	}

	h.client.Close()
	if v, _ := h.broker.Retained(h.topics.Status); v != StatusOffline {
		t.Errorf("status after Close = %q, want offline", v)
	}
	prefix := testSite + "/" + testStation + "/auto7/"
	for _, p := range h.broker.Published() {
		if !strings.HasPrefix(p.Topic, prefix) || strings.HasSuffix(p.Topic, "/cmd") {
			t.Errorf("publish on %s, want only the own auto7 slot topics", p.Topic)
		}
	}
}

func TestStationSourcesDecode(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.client.Events()
	b, tp := h.broker, h.topics
	slot := func(name, suffix string) string { return testSite + "/" + testStation + "/" + name + "/" + suffix }

	b.Send(slot(SlotRadio, "state"), `{"ts":"2026-11-21T11:59:59Z","freq_hz":14074000,"band":"20m","mode":"data","tx":"tx","tuning":true,"device_online":true}`)
	r := next[RadioState](t, ev, nil)
	if !r.TX || !r.Tuning || r.Band != "20m" || r.FreqHz != 14074000 || r.Mode != "data" || !r.DeviceOnline || r.TS.IsZero() {
		t.Errorf("radio = %+v", r)
	}
	b.Send(slot(SlotRadio, "status"), "online")
	if s := next[Status](t, ev, nil); s.Slot != SlotRadio || !s.Online() {
		t.Errorf("radio status = %+v", s)
	}
	b.Send(slot(SlotAntSwitch, "state"), `{"ts":"2026-11-21T11:59:58Z","selected":"port4","settled":true}`)
	if s := next[AntSwitchState](t, ev, nil); s.Selected != "port4" || !s.Settled {
		t.Errorf("ant-switch = %+v", s)
	}
	b.Send(slot(SlotAntCtrl, "state"), `{"ts":"2026-11-21T11:59:58Z","freq_hz":14074000,"band":"20m","moving":true,"device_online":true}`)
	if s := next[AntCtrlState](t, ev, nil); !s.Moving || s.Band != "20m" {
		t.Errorf("ant-ctrl = %+v", s)
	}
	b.Send(slot(SlotTuner, "state"), `{"ts":"2026-11-21T11:59:58Z","settling":true,"inline":false,"device_online":true}`)
	if s := next[TunerState](t, ev, nil); !s.Settling || s.Inline {
		t.Errorf("tuner = %+v", s)
	}
	b.Send(slot(SlotAntennaSelect, "state"), `{"ts":"2026-11-21T11:59:58Z","mode":"auto","target":"port6","source":"auto","activity":"active"}`)
	if s := next[AntennaSelectState](t, ev, nil); s.Target != "port6" || s.Mode != "auto" {
		t.Errorf("antenna-select = %+v", s)
	}
	b.Send(tp.ChaseStatus, "offline")
	if s := next[Status](t, ev, nil); s.Slot != SlotChase || s.Online() {
		t.Errorf("chase status = %+v", s)
	}
	b.Send(testSite+"/"+testStation+"/auto7/otherhost/state", `{"ts":"2026-11-21T11:59:40Z","host":"otherhost","enabled":true}`)
	if s := next[PeerState](t, ev, nil); s.Host != "otherhost" || !s.Enabled || s.TS.IsZero() {
		t.Errorf("peer state = %+v", s)
	}
	b.Send(testSite+"/"+testStation+"/auto7/otherhost/status", "online")
	if s := next[PeerStatus](t, ev, nil); s.Host != "otherhost" || !s.Online() {
		t.Errorf("peer status = %+v", s)
	}
}
