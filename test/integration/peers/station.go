// SPDX-License-Identifier: AGPL-3.0-or-later

package peers

import (
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"codeberg.org/kgbvax/stationa/shared/schema"
)

// Received is one message that a peer received on its /cmd topic.
type Received struct {
	Topic    string
	Payload  []byte
	Retained bool
}

// Slot is a fake StationA source at <site>/<station>/<name>. It publishes a
// retained /state and /status and records every message on /cmd. It
// publishes on change only, or also with a heartbeat after
// StartHeartbeat.
type Slot struct {
	client              pahomqtt.Client
	site, station, name string
	now                 func() time.Time

	mu     sync.Mutex
	fields map[string]any
	last   []byte
	cmds   []Received
	stop   chan struct{}
	wg     sync.WaitGroup
}

// NewSlot subscribes to the /cmd topic of the slot. now gives the ts field
// of each state. A nil now uses time.Now.
func NewSlot(c pahomqtt.Client, site, station, name string, now func() time.Time) (*Slot, error) {
	if now == nil {
		now = time.Now
	}
	s := &Slot{client: c, site: site, station: station, name: name, now: now, fields: map[string]any{}}
	tok := c.Subscribe(s.CmdTopic(), 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
		s.mu.Lock()
		s.cmds = append(s.cmds, Received{Topic: m.Topic(), Payload: append([]byte(nil), m.Payload()...), Retained: m.Retained()})
		s.mu.Unlock()
	})
	if !tok.WaitTimeout(5 * time.Second) {
		return nil, fmt.Errorf("subscribe %s: timeout", s.CmdTopic())
	}
	if err := tok.Error(); err != nil {
		return nil, err
	}
	return s, nil
}

// StateTopic is <site>/<station>/<name>/state.
func (s *Slot) StateTopic() string { return schema.StateTopic(s.site, s.station, s.name) }

// StatusTopic is <site>/<station>/<name>/status.
func (s *Slot) StatusTopic() string { return schema.StatusTopic(s.site, s.station, s.name) }

// CmdTopic is <site>/<station>/<name>/cmd.
func (s *Slot) CmdTopic() string { return schema.CmdTopic(s.site, s.station, s.name) }

// Set merges the fields into the state and publishes the state, retained,
// with a ts field from the clock.
func (s *Slot) Set(fields map[string]any) error {
	s.mu.Lock()
	maps.Copy(s.fields, fields)
	doc := maps.Clone(s.fields)
	s.mu.Unlock()
	doc["ts"] = s.now().UTC().Format(time.RFC3339)
	return s.Publish(doc)
}

// Publish publishes doc as JSON on the state topic, retained, as it is.
func (s *Slot) Publish(doc any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.last = b
	s.mu.Unlock()
	return publish(s.client, s.StateTopic(), true, b)
}

// SetStatus publishes the status, for example "online" or "offline",
// retained.
func (s *Slot) SetStatus(status string) error {
	return publish(s.client, s.StatusTopic(), true, []byte(status))
}

// Clear removes the retained state and status from the broker.
func (s *Slot) Clear() error {
	if err := publish(s.client, s.StateTopic(), true, nil); err != nil {
		return err
	}
	return publish(s.client, s.StatusTopic(), true, nil)
}

// StartHeartbeat publishes the last state again at each interval. A state
// set by Set gets a new ts each time.
func (s *Slot) StartHeartbeat(every time.Duration) {
	s.StopHeartbeat()
	s.mu.Lock()
	stop := make(chan struct{})
	s.stop = stop
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.mu.Lock()
				fromSet := len(s.fields) > 0
				last := s.last
				s.mu.Unlock()
				if fromSet {
					_ = s.Set(nil)
				} else if last != nil {
					_ = publish(s.client, s.StateTopic(), true, last)
				}
			}
		}
	}()
}

// StopHeartbeat ends the heartbeat. Then the slot publishes on change only.
func (s *Slot) StopHeartbeat() {
	s.mu.Lock()
	stop := s.stop
	s.stop = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	s.wg.Wait()
}

// Commands returns a copy of every message received on /cmd.
func (s *Slot) Commands() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.cmds...)
}

// Close ends the heartbeat and the /cmd subscription.
func (s *Slot) Close() {
	s.StopHeartbeat()
	s.client.Unsubscribe(s.CmdTopic()).WaitTimeout(time.Second)
}

// Slot names of the StationA sources that auto7 reads.
const (
	SlotRadio         = "radio"
	SlotAntSwitch     = "ant-switch"
	SlotAntCtrl       = "ant-ctrl"
	SlotTuner         = "tuner"
	SlotAntennaSelect = "antenna-select"
)

// Station is the set of fake StationA sources of one station.
type Station struct {
	Radio         *Slot
	AntSwitch     *Slot
	AntCtrl       *Slot
	Tuner         *Slot
	AntennaSelect *Slot
}

// NewStation creates the sources of <site>/<station>.
func NewStation(c pahomqtt.Client, site, station string, now func() time.Time) (*Station, error) {
	st := &Station{}
	for _, p := range []struct {
		slot **Slot
		name string
	}{
		{&st.Radio, SlotRadio},
		{&st.AntSwitch, SlotAntSwitch},
		{&st.AntCtrl, SlotAntCtrl},
		{&st.Tuner, SlotTuner},
		{&st.AntennaSelect, SlotAntennaSelect},
	} {
		s, err := NewSlot(c, site, station, p.name, now)
		if err != nil {
			st.Close()
			return nil, err
		}
		*p.slot = s
	}
	return st, nil
}

// Slots lists every source of the station.
func (st *Station) Slots() []*Slot {
	var out []*Slot
	for _, s := range []*Slot{st.Radio, st.AntSwitch, st.AntCtrl, st.Tuner, st.AntennaSelect} {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}

// SetOnline publishes the status "online" for every source.
func (st *Station) SetOnline() error {
	for _, s := range st.Slots() {
		if err := s.SetStatus("online"); err != nil {
			return err
		}
	}
	return nil
}

// SetRadio publishes the radio band, the tx field ("tx" or "rx") and the
// tuning field, with device_online true.
func (st *Station) SetRadio(band string, tx, tuning bool) error {
	txv := "rx"
	if tx {
		txv = "tx"
	}
	return st.Radio.Set(map[string]any{"band": band, "tx": txv, "tuning": tuning, "device_online": true})
}

// SetAntSwitch publishes the selected port and the settled field.
func (st *Station) SetAntSwitch(selected string, settled bool) error {
	return st.AntSwitch.Set(map[string]any{"selected": selected, "settled": settled})
}

// SetAntCtrl publishes the moving field of the antenna controller.
func (st *Station) SetAntCtrl(moving bool) error {
	return st.AntCtrl.Set(map[string]any{"moving": moving})
}

// SetTuner publishes the settling field of the tuner.
func (st *Station) SetTuner(settling bool) error {
	return st.Tuner.Set(map[string]any{"settling": settling})
}

// SetAntennaSelectTarget publishes the target port of antennaselect.
func (st *Station) SetAntennaSelectTarget(target string) error {
	return st.AntennaSelect.Set(map[string]any{"target": target})
}

// Commands returns every message that the station received on a /cmd
// topic.
func (st *Station) Commands() []Received {
	var out []Received
	for _, s := range st.Slots() {
		out = append(out, s.Commands()...)
	}
	return out
}

// Close closes every source.
func (st *Station) Close() {
	for _, s := range st.Slots() {
		s.Close()
	}
}
