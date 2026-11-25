// noc-bot watches the event's logs, alerts in Slack, and asks laya (with the local LLM as its
// teacher) the questions an operator would ask while scanning logs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IWNOC/noc-bot/internal/bot"
	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/ingest"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/llm"
	"github.com/IWNOC/noc-bot/internal/notify"
	"github.com/IWNOC/noc-bot/internal/slackapp"
	"github.com/IWNOC/noc-bot/internal/store"
	"github.com/IWNOC/noc-bot/internal/teacher"
)

func main() {
	cfgPath := flag.String("config", "noc-bot.yaml", "config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	var ly *laya.Client
	if cfg.Laya.URL != "" {
		ly = laya.New(cfg.Laya.URL, os.Getenv(cfg.Laya.TokenEnv), cfg.Laya.Timeout)
	}
	var lm *llm.Client
	if cfg.LLM.URL != "" {
		lm = llm.New(cfg.LLM.URL, os.Getenv(cfg.LLM.TokenEnv), cfg.LLM.Model, cfg.LLM.Timeout)
	}
	var nt notify.Notifier = notify.Log{L: log}
	var app *slackapp.App
	switch {
	case os.Getenv("SLACK_BOT_TOKEN") != "" && os.Getenv("SLACK_APP_TOKEN") != "" && cfg.Slack.Channel != "":
		app = slackapp.New(os.Getenv("SLACK_BOT_TOKEN"), os.Getenv("SLACK_APP_TOKEN"), cfg.Slack.Channel, log)
		nt = app
	case os.Getenv("SLACK_BOT_TOKEN") != "" && cfg.Slack.Channel != "":
		nt = notify.Bot{Token: os.Getenv("SLACK_BOT_TOKEN"), Channel: cfg.Slack.Channel}
	case cfg.Slack.WebhookEnv != "" && os.Getenv(cfg.Slack.WebhookEnv) != "":
		nt = notify.Webhook{URL: os.Getenv(cfg.Slack.WebhookEnv)}
	default:
		log.Warn("no Slack credentials: posting to the log only")
	}

	b, err := bot.New(cfg, log, st, ly, lm, nt)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if app != nil {
		app.Teams = b.TeamNames
		go func() {
			if err := app.Run(ctx, b); err != nil && ctx.Err() == nil {
				log.Error("slack app stopped", "err", err)
			}
		}()
	}

	in := ingest.NewServer(20000, log)
	if cfg.Listen.TCP != "" {
		go func() {
			if err := in.ServeTCP(ctx, cfg.Listen.TCP); err != nil {
				log.Error("tcp listener", "err", err)
				stop()
			}
		}()
	}
	if cfg.Listen.UDP != "" {
		go func() {
			if err := in.ServeUDP(ctx, cfg.Listen.UDP); err != nil {
				log.Error("udp listener", "err", err)
				stop()
			}
		}()
	}

	tc := &teacher.Teacher{Token: os.Getenv("NOC_BOT_TEACHER_TOKEN"), Known: b.Known, LLM: lm, Tools: b.Tools(), Log: log,
		OnAnswer: func(desk, text string, answers map[string]any, evidence map[string]string) {
			ev, _ := json.Marshal(evidence)
			st.Label(context.Background(), time.Now(), desk, text, answers, "teacher", "noc-bot", false, string(ev))
		}}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/teacher", tc)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "templates": b.Miner.Len(),
			"dropped": in.Dropped.Load(), "bad_lines": in.Bad.Load(), "open_incidents": len(b.Incidents.Open()),
			"known_issue_hits": b.Rules.SuppressedCounts()})
	})
	srv := &http.Server{Addr: cfg.Listen.HTTP, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http", "err", err)
			stop()
		}
	}()
	log.Info("noc-bot started", "tcp", cfg.Listen.TCP, "http", cfg.Listen.HTTP, "laya", ly != nil, "llm", lm != nil)
	b.Run(ctx, in.Out)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
