// Package config loads noc-bot's YAML configuration. Site knowledge (senders, teams, known issues)
// lives here and in the store, never in code, so the same binary works at next year's event.
package config

import (
	"fmt"
	"os"
	"path"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen   Listen          `yaml:"listen"`
	Senders  []Sender        `yaml:"senders"`
	Expected []Expected      `yaml:"expected"`
	Teams    map[string]Team `yaml:"teams"`
	Known    []KnownIssue    `yaml:"known_issues"`
	Laya     Laya            `yaml:"laya"`
	LLM      LLM             `yaml:"llm"`
	Slack    Slack           `yaml:"slack"`
	Store    Store           `yaml:"store"`
	Clock    Clock           `yaml:"clock"`
	Web      Web             `yaml:"web"`
}

// Web lets the conversation agent search the internet (a local SearXNG), read public pages and
// look up who is behind an AS number or a public address.
type Web struct {
	SearxURL string `yaml:"searx_url"` // e.g. http://127.0.0.1:8888; empty: no web tools
}

type Listen struct {
	TCP  string `yaml:"tcp"`  // RemoteFmt lines from rsyslog, e.g. ":6514"
	UDP  string `yaml:"udp"`  // optional
	HTTP string `yaml:"http"` // teacher endpoint and health, e.g. ":8080"
}

// Sender describes a log source. Match on the sender IP or hostname (shell glob).
type Sender struct {
	From   string `yaml:"from"`
	Host   string `yaml:"host"`
	Name   string `yaml:"name"`
	Role   string `yaml:"role"`
	Team   string `yaml:"team"`
	Vendor string `yaml:"vendor"`
}

// Expected is a sender that must keep talking.
type Expected struct {
	Host    string        `yaml:"host"` // glob on hostname
	Silence time.Duration `yaml:"silence"`
}

type Team struct {
	Areas       []string `yaml:"areas"`   // network areas it owns: physical, l2l3, server, auth, wireless
	Mention     string   `yaml:"mention"` // e.g. <!subteam^S0XXXX>
	Channel     string   `yaml:"channel"`
	Description string   `yaml:"description"`
}

// KnownIssue is something accepted that should not alert as such.
type KnownIssue struct {
	ID      string    `yaml:"id"`
	Reason  string    `yaml:"reason"`
	Expires time.Time `yaml:"expires"`
	// what it covers
	Vendor   string `yaml:"vendor"`
	Host     string `yaml:"host"`
	Program  string `yaml:"program"`
	Template string `yaml:"template"` // regexp on the template text
	// clock skew issues: the skew that is normal for these senders
	ExpectSkew    time.Duration `yaml:"expect_skew"`
	SkewTolerance time.Duration `yaml:"skew_tolerance"`
}

type Laya struct {
	URL      string        `yaml:"url"`       // laya-svc, e.g. http://laya-svc:8100
	TokenEnv string        `yaml:"token_env"` // env var holding the bearer token
	Timeout  time.Duration `yaml:"timeout"`
}

type LLM struct {
	URL      string        `yaml:"url"` // OpenAI-compatible base, e.g. https://llm.example/v1
	Model    string        `yaml:"model"`
	Deep     string        `yaml:"deep_model"`  // slower model for reports
	AgentM   string        `yaml:"agent_model"` // model for Slack conversation; default: model
	TokenEnv string        `yaml:"token_env"`
	Timeout  time.Duration `yaml:"timeout"`
}

type Slack struct {
	WebhookEnv string `yaml:"webhook_env"`
	Channel    string `yaml:"channel"` // default channel (noc-general)
}

type Store struct {
	Path string `yaml:"path"`
}

type Clock struct {
	MaxSkew time.Duration `yaml:"max_skew"` // alert when a sender's clock is further off than this
}

// Load reads and validates a config file.
func Load(p string) (*Config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	if c.Listen.HTTP == "" {
		c.Listen.HTTP = ":8080"
	}
	if c.Laya.Timeout == 0 {
		c.Laya.Timeout = 90 * time.Second
	}
	if c.LLM.Timeout == 0 {
		c.LLM.Timeout = 60 * time.Second
	}
	if c.Clock.MaxSkew == 0 {
		c.Clock.MaxSkew = 2 * time.Minute
	}
	if c.Store.Path == "" {
		c.Store.Path = "noc-bot.db"
	}
	for i := range c.Known {
		if c.Known[i].SkewTolerance == 0 {
			c.Known[i].SkewTolerance = time.Minute
		}
	}
}

func (c *Config) validate() error {
	if c.Listen.TCP == "" && c.Listen.UDP == "" {
		return fmt.Errorf("listen.tcp or listen.udp is required")
	}
	for _, s := range c.Senders {
		if s.From == "" && s.Host == "" {
			return fmt.Errorf("sender %q: from or host is required", s.Name)
		}
		if s.Team != "" {
			if _, ok := c.Teams[s.Team]; !ok {
				return fmt.Errorf("sender %q: unknown team %q", s.Name, s.Team)
			}
		}
	}
	for _, k := range c.Known {
		if k.ID == "" || k.Reason == "" {
			return fmt.Errorf("known issue needs id and reason: %+v", k)
		}
	}
	return nil
}

// SenderFor finds what we know about a sender.
func (c *Config) SenderFor(from, host string) Sender {
	for _, s := range c.Senders {
		if s.From != "" && from != "" && glob(s.From, from) {
			return s
		}
		if s.Host != "" && glob(s.Host, host) {
			return s
		}
	}
	return Sender{Name: host}
}

func glob(pattern, s string) bool {
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

// Glob is exported for other packages.
func Glob(pattern, s string) bool { return glob(pattern, s) }
