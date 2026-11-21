// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bus is the MQTT client of auto7. It reads presence, the chase
// state and the state of the StationA sources, and it publishes on the own
// auto7 slot only. It has no general publish and sends no command to
// StationA. auto7 changes band through the CAT control of WSJT-X.
//
// Each delivered message carries its retained flag, its payload ts and
// the local receive time. The client does not decide what a message means
// for TX. The gate does that.
package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	stmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	"codeberg.org/kgbvax/stationa/shared/schema"

	"github.com/kgbvax/auto7/internal/config"
)

// ErrBusDown is the error of a publish while the broker link is down.
var ErrBusDown = errors.New(BusDown)

const (
	// publishTimeout bounds the wait for the broker to acknowledge a
	// publish.
	publishTimeout = 10 * time.Second
	// subscribeTimeout bounds the wait for one subscription.
	subscribeTimeout = 10 * time.Second
	// maxQueued is the number of waiting events at which the client drops
	// new data events. It never drops link events or stop commands.
	maxQueued = 4096
)

// Options holds the optional settings of New.
type Options struct {
	// Now gives the local receive time. Nil means time.Now.
	Now func() time.Time
	// Logger receives the log lines. Nil means slog.Default.
	Logger *slog.Logger
}

// Client is the MQTT client of auto7.
type Client struct {
	site, station, host string
	broker, user        string
	password            string
	topics              Topics
	now                 func() time.Time
	log                 *slog.Logger
	paho                pahomqtt.Client

	queue  *queue
	events chan Event
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	conn atomic.Uint64

	linkMu   sync.Mutex
	ups      uint64
	downs    uint64
	skipLost int
	ready    chan error

	closeOnce sync.Once
}

// New builds a client for the configuration. Call Connect to open the
// link.
func New(cfg config.Config, opts Options) *Client {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		site:     cfg.Site,
		station:  cfg.Station,
		host:     cfg.Host,
		broker:   cfg.MQTT.Broker,
		user:     cfg.MQTT.User,
		password: cfg.MQTT.Password.Reveal(),
		topics:   TopicsFor(cfg.Site, cfg.Station, cfg.Host),
		now:      now,
		log:      log,
		queue:    &queue{sig: make(chan struct{}, 1)},
		events:   make(chan Event, 16),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		ready:    make(chan error, 1),
	}
	o := pahomqtt.NewClientOptions().
		AddBroker(cfg.MQTT.Broker).
		SetClientID(cfg.MQTT.ClientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(false).
		SetConnectTimeout(10*time.Second).
		SetMaxReconnectInterval(10*time.Second).
		SetOrderMatters(true).
		SetWill(c.topics.Status, StatusOffline, 1, true).
		SetOnConnectHandler(c.onConnect).
		SetConnectionLostHandler(c.onLost)
	if cfg.MQTT.User != "" {
		o.SetUsername(cfg.MQTT.User)
		o.SetPassword(c.password)
	}
	c.paho = pahomqtt.NewClient(o)
	go c.pump()
	return c
}

// Topics returns the topics of the client.
func (c *Client) Topics() Topics { return c.topics }

// Events delivers the events in the order of arrival. The channel closes
// after Close. Read it all the time: the client keeps at most a bounded
// number of waiting data events.
func (c *Client) Events() <-chan Event { return c.events }

// Up reports whether the broker link is up now.
func (c *Client) Up() bool { return c.paho.IsConnectionOpen() }

// Connect opens the broker link and waits until the subscriptions are in
// place. After a lost link the client reconnects by itself. The error
// never holds the password.
func (c *Client) Connect(ctx context.Context) error {
	if err := stmqtt.Connect(ctx, c.paho); err != nil {
		return c.connectError(err)
	}
	select {
	case err := <-c.ready:
		return err
	case <-ctx.Done():
		c.paho.Disconnect(0)
		return ctx.Err()
	}
}

// connectError describes a failed connect without the password.
func (c *Client) connectError(err error) error {
	text := fmt.Sprintf("connect to the MQTT broker %s as %q", c.broker, c.user)
	if c.password != "" && strings.Contains(err.Error(), c.password) {
		return fmt.Errorf("%s: %s", text, c.redact(err.Error()))
	}
	return fmt.Errorf("%s: %w", text, err)
}

// redact removes the password from s.
func (c *Client) redact(s string) string {
	if c.password == "" {
		return s
	}
	return strings.ReplaceAll(s, c.password, "[set]")
}

// Close publishes "offline" on the own status, ends the link and closes
// the event channel. A second call does nothing.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.paho.IsConnectionOpen() {
			tok := c.paho.Publish(c.topics.Status, 1, true, []byte(StatusOffline))
			tok.WaitTimeout(2 * time.Second)
		}
		c.paho.Disconnect(250)
		c.cancel()
		<-c.done
	})
}

// PublishState publishes the own state, retained, on
// <site>/<station>/auto7/<host>/state. The client adds the host and the
// ts.
func (c *Client) PublishState(ctx context.Context, s State) error {
	return c.publish(ctx, c.topics.State, true, statePayload{
		TS:         c.ts(),
		Host:       c.host,
		Enabled:    s.Enabled,
		Band:       s.Band,
		Mode:       s.Mode,
		Task:       s.Task,
		StopReason: s.StopReason,
		Reduced:    s.Reduced,
	})
}

// PublishCandidates publishes the decoded stations, retained, on
// <site>/<station>/auto7/candidates. Each mode must be FT8 or FT4.
func (c *Client) PublishCandidates(ctx context.Context, cands []Candidate) error {
	p := candidatesPayload{TS: c.ts(), Host: c.host, Candidates: make([]candidatePayload, 0, len(cands))}
	for _, k := range cands {
		if k.Mode != config.ModeFT8 && k.Mode != config.ModeFT4 {
			return fmt.Errorf("candidate %s: mode %q: want FT8 or FT4", k.Call, k.Mode)
		}
		p.Candidates = append(p.Candidates, candidatePayload{Call: k.Call, Band: k.Band, Mode: k.Mode, LastMsg: k.LastMsg})
	}
	return c.publish(ctx, c.topics.Candidates, true, p)
}

// PublishQSO publishes one logged QSO, not retained, on
// <site>/<station>/auto7/qso.
func (c *Client) PublishQSO(ctx context.Context, q QSO) error {
	return c.publish(ctx, c.topics.QSO, false, qsoPayload{
		TS:      c.ts(),
		Host:    c.host,
		Call:    q.Call,
		Grid:    q.Grid,
		Band:    q.Band,
		FreqHz:  q.FreqHz,
		Mode:    q.Mode,
		TimeOn:  q.TimeOn.UTC().Format(time.RFC3339),
		TimeOff: q.TimeOff.UTC().Format(time.RFC3339),
		RSTSent: q.RSTSent,
		RSTRcvd: q.RSTRcvd,
		MyCall:  q.MyCall,
		MyGrid:  q.MyGrid,
	})
}

func (c *Client) ts() string { return c.now().UTC().Format(time.RFC3339) }

// publish sends one JSON message with QoS 1 and waits for the broker. It
// is the only publish path of the package.
func (c *Client) publish(ctx context.Context, topic string, retained bool, v any) error {
	if !c.paho.IsConnectionOpen() {
		return fmt.Errorf("publish %s: %w", topic, ErrBusDown)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	tok := c.paho.Publish(topic, 1, retained, b)
	select {
	case <-tok.Done():
		if err := tok.Error(); err != nil {
			return fmt.Errorf("publish %s: %s: %w", topic, c.redact(err.Error()), ErrBusDown)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("publish %s: %w", topic, ctx.Err())
	}
}

// onConnect runs on its own goroutine after each connect. It reports the
// link, publishes "online" and subscribes. The link event comes before
// every message of the new connection.
func (c *Client) onConnect(pc pahomqtt.Client) {
	c.linkMu.Lock()
	if c.ups > c.downs {
		// paho can run the new connect before it reports the loss of
		// the old link. Report the loss first and skip the late report.
		c.downs++
		c.skipLost++
		c.queue.push(Link{Up: false, Conn: c.conn.Load(), At: c.now(), Reason: BusDown}, true, c.log)
	}
	c.ups++
	n := c.conn.Add(1)
	c.queue.push(Link{Up: true, Conn: n, At: c.now()}, true, c.log)
	c.linkMu.Unlock()
	c.log.Info("bus up", "broker", c.broker, "conn", n)

	var firstErr error
	if tok := pc.Publish(c.topics.Status, 1, true, []byte(StatusOnline)); !tok.WaitTimeout(publishTimeout) || tok.Error() != nil {
		c.log.Warn("publish online failed", "topic", c.topics.Status, "err", c.errText(tok.Error()))
	}
	for _, s := range c.subscriptions() {
		tok := pc.Subscribe(s.topic, 1, c.handler(s.decode))
		if !tok.WaitTimeout(subscribeTimeout) {
			firstErr = orErr(firstErr, fmt.Errorf("subscribe %s: timeout", s.topic))
			continue
		}
		if err := tok.Error(); err != nil {
			firstErr = orErr(firstErr, fmt.Errorf("subscribe %s: %s", s.topic, c.redact(err.Error())))
		}
	}
	if firstErr != nil {
		c.log.Error("bus subscribe failed", "err", firstErr)
	}
	select {
	case c.ready <- firstErr:
	default:
	}
}

// orErr returns a, or b when a is nil.
func orErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (c *Client) errText(err error) string {
	if err == nil {
		return ""
	}
	return c.redact(err.Error())
}

// onLost reports a lost link.
func (c *Client) onLost(_ pahomqtt.Client, err error) {
	c.linkMu.Lock()
	defer c.linkMu.Unlock()
	if c.skipLost > 0 {
		c.skipLost--
		return
	}
	if c.ups <= c.downs {
		return
	}
	c.downs++
	text := c.errText(err)
	c.queue.push(Link{Up: false, Conn: c.conn.Load(), At: c.now(), Reason: BusDown, Err: text}, true, c.log)
	c.log.Warn(BusDown, "broker", c.broker, "err", text)
}

// pump moves the queued events to the event channel.
func (c *Client) pump() {
	defer close(c.done)
	defer close(c.events)
	for {
		ev, ok := c.queue.pop()
		if !ok {
			select {
			case <-c.queue.sig:
				continue
			case <-c.ctx.Done():
				return
			}
		}
		select {
		case c.events <- ev:
		case <-c.ctx.Done():
			return
		}
	}
}

// queue holds the events between the paho goroutines and the pump. A paho
// handler must never block, so push never waits.
type queue struct {
	mu    sync.Mutex
	items []Event
	sig   chan struct{}
}

// push adds ev. When the queue is full, it drops ev unless keep is true.
func (q *queue) push(ev Event, keep bool, log *slog.Logger) {
	q.mu.Lock()
	if !keep && len(q.items) >= maxQueued {
		q.mu.Unlock()
		log.Warn("bus event queue full: event dropped", "type", fmt.Sprintf("%T", ev))
		return
	}
	q.items = append(q.items, ev)
	q.mu.Unlock()
	select {
	case q.sig <- struct{}{}:
	default:
	}
}

func (q *queue) pop() (Event, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, false
	}
	ev := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	return ev, true
}

// decoder turns one payload into an event. A nil event with a nil error
// means that the message is dropped on purpose.
type decoder func(payload []byte, meta Meta) (Event, error)

type subscription struct {
	topic  string
	decode decoder
}

// subscriptions lists every topic that the client reads.
func (c *Client) subscriptions() []subscription {
	subs := []subscription{
		{c.topics.Presence, decodePresence},
		{c.topics.ChaseState, decodeChase},
		{c.topics.ChaseStatus, statusDecoder(SlotChase)},
		{c.topics.PeerState, decodePeerState},
		{c.topics.PeerStatus, decodePeerStatus},
		{c.topics.Cmd, c.decodeCmd},
	}
	stateDecoders := map[string]decoder{
		SlotRadio:         decodeRadio,
		SlotAntSwitch:     decodeAntSwitch,
		SlotAntCtrl:       decodeAntCtrl,
		SlotTuner:         decodeTuner,
		SlotAntennaSelect: decodeAntennaSelect,
	}
	for _, slot := range stationSlots {
		subs = append(subs,
			subscription{schema.StateTopic(c.site, c.station, slot), stateDecoders[slot]},
			subscription{schema.StatusTopic(c.site, c.station, slot), statusDecoder(slot)},
		)
	}
	return subs
}

// handler builds the paho handler for one subscription. It runs on the
// paho dispatch goroutine and only decodes and queues.
func (c *Client) handler(decode decoder) pahomqtt.MessageHandler {
	return func(_ pahomqtt.Client, m pahomqtt.Message) {
		meta := Meta{Topic: m.Topic(), Retained: m.Retained(), Received: c.now(), Conn: c.conn.Load()}
		ev, err := decode(m.Payload(), meta)
		if err != nil {
			c.log.Warn("bus message ignored", "topic", meta.Topic, "retained", meta.Retained, "err", err)
			return
		}
		if ev == nil {
			return
		}
		_, stop := ev.(StopCommand)
		c.queue.push(ev, stop, c.log)
	}
}

// parseTS reads a payload ts. It gives the zero time when ts is empty or
// does not parse.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// errCleared marks an empty payload on a state topic, which is a cleared
// retained state.
var errCleared = errors.New("empty payload")

// decodeJSON reads a JSON payload into v and returns the ts field.
func decodeJSON(payload []byte, v any) (time.Time, error) {
	if len(payload) == 0 {
		return time.Time{}, errCleared
	}
	var t struct {
		TS string `json:"ts"`
	}
	if err := json.Unmarshal(payload, &t); err != nil {
		return time.Time{}, err
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return time.Time{}, err
	}
	return parseTS(t.TS), nil
}

// stateDecoder builds a decoder for a JSON state. It drops a cleared
// state without a log line.
func stateDecoder[P any](build func(P, Meta) Event) decoder {
	return func(payload []byte, meta Meta) (Event, error) {
		var p P
		ts, err := decodeJSON(payload, &p)
		if errors.Is(err, errCleared) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		meta.TS = ts
		return build(p, meta), nil
	}
}

var decodePresence = stateDecoder(func(p struct {
	Present bool   `json:"present"`
	Host    string `json:"host"`
}, m Meta) Event {
	return Presence{Meta: m, Present: p.Present, Host: p.Host}
})

var decodeChase = stateDecoder(func(p struct {
	Seq      int64      `json:"seq"`
	Activity string     `json:"activity"`
	Ranked   []ChaseRow `json:"ranked"`
	Problems []string   `json:"problems"`
}, m Meta) Event {
	return ChaseState{Meta: m, Seq: p.Seq, Activity: p.Activity, Ranked: p.Ranked, Problems: p.Problems}
})

var decodeRadio = stateDecoder(func(p struct {
	FreqHz       int64  `json:"freq_hz"`
	Band         string `json:"band"`
	Mode         string `json:"mode"`
	TX           string `json:"tx"`
	Tuning       bool   `json:"tuning"`
	DeviceOnline bool   `json:"device_online"`
}, m Meta) Event {
	return RadioState{Meta: m, Band: p.Band, FreqHz: p.FreqHz, Mode: p.Mode, TX: p.TX == "tx", Tuning: p.Tuning, DeviceOnline: p.DeviceOnline}
})

var decodeAntSwitch = stateDecoder(func(p struct {
	Selected string `json:"selected"`
	Settled  bool   `json:"settled"`
}, m Meta) Event {
	return AntSwitchState{Meta: m, Selected: p.Selected, Settled: p.Settled}
})

var decodeAntCtrl = stateDecoder(func(p struct {
	Moving       bool   `json:"moving"`
	Band         string `json:"band"`
	FreqHz       int64  `json:"freq_hz"`
	DeviceOnline bool   `json:"device_online"`
}, m Meta) Event {
	return AntCtrlState{Meta: m, Moving: p.Moving, Band: p.Band, FreqHz: p.FreqHz, DeviceOnline: p.DeviceOnline}
})

var decodeTuner = stateDecoder(func(p struct {
	Settling     bool `json:"settling"`
	Inline       bool `json:"inline"`
	DeviceOnline bool `json:"device_online"`
}, m Meta) Event {
	return TunerState{Meta: m, Settling: p.Settling, Inline: p.Inline, DeviceOnline: p.DeviceOnline}
})

var decodeAntennaSelect = stateDecoder(func(p struct {
	Target string `json:"target"`
	Mode   string `json:"mode"`
	Source string `json:"source"`
}, m Meta) Event {
	return AntennaSelectState{Meta: m, Target: p.Target, Mode: p.Mode, Source: p.Source}
})

// hostOfTopic returns the host part of <site>/<station>/auto7/<host>/<suffix>.
func hostOfTopic(topic string) string {
	parts := strings.Split(topic, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

var decodePeerState = stateDecoder(func(p struct {
	Enabled bool `json:"enabled"`
}, m Meta) Event {
	return PeerState{Meta: m, Host: hostOfTopic(m.Topic), Enabled: p.Enabled}
})

func decodePeerStatus(payload []byte, meta Meta) (Event, error) {
	return PeerStatus{Meta: meta, Host: hostOfTopic(meta.Topic), Value: string(payload)}, nil
}

// statusDecoder builds the decoder of a /status topic. The payload is
// plain text and has no ts.
func statusDecoder(slot string) decoder {
	return func(payload []byte, meta Meta) (Event, error) {
		return Status{Meta: meta, Slot: slot, Value: string(payload)}, nil
	}
}

// decodeCmd accepts only a live "stop". It drops a retained command and
// logs every other action as refused.
func (c *Client) decodeCmd(payload []byte, meta Meta) (Event, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var p schema.CmdPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.log.Warn("bus command refused: not a command", "topic", meta.Topic, "err", err)
		return nil, nil
	}
	if meta.Retained {
		c.log.Info("bus command dropped: retained", "topic", meta.Topic, "action", p.Action)
		return nil, nil
	}
	if p.Action != ActionStop {
		c.log.Warn("bus command refused: auto7 accepts only stop", "topic", meta.Topic, "action", p.Action)
		return nil, nil
	}
	return StopCommand{Meta: meta, Value: p.Value}, nil
}
