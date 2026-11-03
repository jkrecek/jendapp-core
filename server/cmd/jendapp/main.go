// Command jendapp is the personal-app server.
//
//	jendapp [-config path] serve           run the server
//	jendapp [-config path] pair <name>     create a device token
//	jendapp [-config path] devices         list paired devices
//	jendapp [-config path] revoke <id>     revoke a device
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"jendapp/internal/api"
	"jendapp/internal/claude"
	"jendapp/internal/config"
	"jendapp/internal/inbox"
	"jendapp/internal/module"
	"jendapp/internal/ops"
	"jendapp/internal/store"
)

func main() {
	cfgPath := flag.String("config", config.ExpandHome("~/.jendapp/config.toml"), "config file")
	flag.Parse()
	if err := run(*cfgPath, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "jendapp:", err)
		os.Exit(1)
	}
}

func run(cfgPath string, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve(cfg, st)
	case "pair":
		if len(args) != 2 {
			return errors.New("usage: jendapp pair <device-name>")
		}
		token, err := st.CreateDevice(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Device %q paired. Token (shown once):\n%s\n", args[1], token)
		if cfg.PublicURL != "" {
			q := url.Values{"url": {cfg.PublicURL}, "token": {token}}
			fmt.Printf("\nPairing link (open on the device):\njendapp://pair?%s\n", q.Encode())
		}
		return nil
	case "devices":
		ds, err := st.Devices()
		if err != nil {
			return err
		}
		for _, d := range ds {
			last := "never"
			if d.LastSeen != nil {
				last = d.LastSeen.Local().Format(time.DateTime)
			}
			fmt.Printf("%d\t%s\tcreated %s\tlast seen %s\n", d.ID, d.Name, d.CreatedAt.Local().Format(time.DateOnly), last)
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: jendapp revoke <device-id>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return err
		}
		return st.RevokeDevice(id)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func serve(cfg *config.Config, st *store.Store) error {
	reg, err := module.LoadAll(cfg.Modules)
	if err != nil {
		return err
	}
	if err := st.FailRunningJobs(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := claude.NewRunner(ctx, cfg.ClaudeBin, cfg.MaxJobs, st)
	in, err := inbox.New(cfg.DataDir, st, reg, runner)
	if err != nil {
		return err
	}
	opsRunner := ops.NewRunner(ctx, st)
	a := &api.API{Store: st, Modules: reg, Runner: runner, Ops: opsRunner, Inbox: in}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("jendapp listening", "addr", cfg.Listen, "modules", len(reg.All()))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	runner.Wait()
	opsRunner.Wait()
	return nil
}
