// SPDX-License-Identifier: AGPL-3.0-or-later

// Command auto7 automates chasing in FT8 and FT4. It reads the decodes of
// WSJT-X, asks chase-queue which stations are needed, and asks WSJT-X to
// call them. It calls only while the operator is present and has enabled
// it on the local operator page. See CLAUDE.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kgbvax/auto7/internal/bands"
	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/hunt"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/tx/web"
	"github.com/kgbvax/auto7/internal/ui"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// connectTimeout bounds the first connect to the broker.
const connectTimeout = 30 * time.Second

const usageText = `Usage: auto7 [-config path]

auto7 reads the decodes of WSJT-X, asks chase-queue which stations are
needed, and asks WSJT-X to call them. It starts in Stopped. It calls only
after the operator enables it on the operator page, and only while the
operator is present.

Flags:
`

const envText = `
Environment:
  AUTO7_MQTT_PASSWORD  The password of the MQTT user. auto7 refuses a
                       password in the configuration file.
  XDG_STATE_HOME       The base of the state directory with the hunt
                       lists. The default is ~/.local/state.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run starts auto7 and returns the exit code.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("auto7", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath(), "path of the configuration file. A missing file at the default path gives the defaults.")
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
		fmt.Fprint(stderr, envText)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })

	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := config.Load(*path, !explicit)
	if err != nil {
		log.Error("auto7: configuration", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, log); err != nil {
		log.Error("auto7: exit", "err", err)
		return 1
	}
	log.Info("auto7: exit")
	return 0
}

// serve wires the parts and runs them until ctx ends or one part fails.
// Before it returns, the gate sends HaltTx when auto7 was enabled.
func serve(parent context.Context, cfg config.Config, log *slog.Logger) error {
	dir, err := hunt.StateDir()
	if err != nil {
		return err
	}
	lists, err := hunt.OpenLists(dir, time.Duration(cfg.Limits.SkipMinutes)*time.Minute, time.Now)
	if err != nil {
		// The lists stay unreadable, and Enable is refused.
		log.Error("auto7: hunt lists unreadable: Enable is not possible", "err", err)
	}

	rx, err := wsjtx.Listen(cfg.WSJTX, time.Now)
	if err != nil {
		return err
	}
	defer rx.Close()
	// The send handle of the gate. Only the gate gets it.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return fmt.Errorf("open the WSJT-X send socket: %w", err)
	}
	defer conn.Close()
	ln, err := ui.Listen(cfg.Page.Listen)
	if err != nil {
		return err
	}
	defer ln.Close()

	bc := bus.New(cfg, bus.Options{Logger: log})
	defer bc.Close()
	cctx, cancelConnect := context.WithTimeout(parent, connectTimeout)
	err = bc.Connect(cctx)
	cancelConnect()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	gateWSJTX, huntWSJTX := make(chan wsjtx.Event, 64), make(chan wsjtx.Event, 64)
	gateBus, huntBus := make(chan bus.Event, 64), make(chan bus.Event, 64)
	var h *hunter
	gate := tx.New(tx.Options{
		Config:    cfg,
		Conn:      conn,
		Peer:      rx.Peer,
		ResetPeer: rx.ResetPeer,
		Publisher: bc,
		WSJTX:     gateWSJTX,
		Bus:       gateBus,
		Logger:    log,
		ListsErr:  lists.Err,
		Task: func() string {
			if h == nil {
				return ""
			}
			return h.Task()
		},
	})
	needs := hunt.NewNeeds(lists, cfg.FoxHoundDialsHz)
	feed := hunt.NewFeed(bc, time.Now)
	chooser := hunt.NewChooser(gate, lists, cfg.Limits, needs.OwnCall)
	forager := bands.New(bands.Options{
		Gate: gate, Bands: cfg.Bands, FoxHound: needs.SpotHighlightOnly,
		NotHeard: time.Duration(cfg.Limits.NotHeardMinutes) * time.Minute, Logger: log,
	})
	chooser.SetSpotHook(forager.Spot)
	offsets := bands.NewOffsets(bands.OffsetOptions{Gate: gate, Limits: cfg.Limits, Logger: log})
	chooser.SetOffsetHook(offsets.Hop)
	chooser.SetSpotCheck(func() bool { return needs.HighValueSpot(cfg.Bands) })
	h = newHunter(needs, feed, chooser, forager, offsets, time.Now, log)

	handlers, err := web.New(gate, cfg.Page.Listen)
	if err != nil {
		return err
	}
	page, err := ui.New(ui.Form{
		EnablePath: web.EnablePath, StopPath: web.StopPath,
		TokenField: web.TokenField, Token: handlers.Token(),
	}, func() ui.View {
		v := ui.View{State: gate.State(), Task: h.Task()}
		if err := needs.ListsErr(); err != nil {
			v.ListsErr = err.Error()
		}
		return v
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	mux.Handle("/", page)
	srv := &http.Server{Handler: ui.Secure(mux), ReadHeaderTimeout: 5 * time.Second}

	var wg sync.WaitGroup
	start := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	rawWSJTX := make(chan wsjtx.Event, 64)
	start(func() {
		if err := rx.Run(ctx, rawWSJTX); err != nil {
			cancel(err)
		}
	})
	start(func() { fanOut(ctx, rawWSJTX, gateWSJTX, huntWSJTX) })
	start(func() { fanOut(ctx, bc.Events(), gateBus, huntBus) })
	start(func() { h.run(ctx, huntWSJTX, huntBus) })
	if c := bands.NewClient(cfg.Horstreporter, nil); c != nil {
		start(func() { forager.RunActivity(ctx, c) })
	}
	start(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("operator page: %w", err))
		}
	})
	gateDone := make(chan struct{})
	go func() {
		defer close(gateDone)
		_ = gate.Run(ctx)
	}()
	log.Info("auto7: started in Stopped", "page", "http://"+cfg.Page.Listen, "wsjtx", cfg.WSJTX.Listen, "host", cfg.Host)

	<-ctx.Done()
	// Run sends HaltTx before it returns when auto7 was enabled. The send
	// socket stays open until then.
	<-gateDone
	sctx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	_ = srv.Shutdown(sctx)
	cancelShutdown()
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// fanOut hands each event of in to every out until ctx ends. When in
// closes, it closes every out.
func fanOut[T any](ctx context.Context, in <-chan T, outs ...chan<- T) {
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-in:
			if !ok {
				for _, o := range outs {
					close(o)
				}
				return
			}
			for _, o := range outs {
				select {
				case o <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}
