// SPDX-License-Identifier: AGPL-3.0-or-later

package peers

import (
	"encoding/json"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// PresenceTopic is the topic of the logotter presence heartbeat,
// <site>/<station>/logotter/presence.
func PresenceTopic(site, station string) string {
	return site + "/" + station + "/logotter/presence"
}

// Presence is a fake logotter presence sender for one host.
type Presence struct {
	client pahomqtt.Client
	topic  string
	host   string
	now    func() time.Time

	mu   sync.Mutex
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewPresence creates a sender for the host on the topic. A nil now uses
// time.Now for the ts field.
func NewPresence(c pahomqtt.Client, topic, host string, now func() time.Time) *Presence {
	if now == nil {
		now = time.Now
	}
	return &Presence{client: c, topic: topic, host: host, now: now}
}

// Send publishes one message {"ts", "present", "host"}, live or retained.
func (p *Presence) Send(present, retained bool) error {
	b, err := json.Marshal(struct {
		TS      string `json:"ts"`
		Present bool   `json:"present"`
		Host    string `json:"host"`
	}{p.now().UTC().Format(time.RFC3339), present, p.host})
	if err != nil {
		return err
	}
	return publish(p.client, p.topic, retained, b)
}

// Start sends a live present:true message now and then at each interval,
// until Stop.
func (p *Presence) Start(every time.Duration) error {
	p.Stop()
	if err := p.Send(true, false); err != nil {
		return err
	}
	p.mu.Lock()
	stop := make(chan struct{})
	p.stop = stop
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = p.Send(true, false)
			}
		}
	}()
	return nil
}

// Stop ends the heartbeat. It sends no last message, like a logotter that
// stops.
func (p *Presence) Stop() {
	p.mu.Lock()
	stop := p.stop
	p.stop = nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	p.wg.Wait()
}
