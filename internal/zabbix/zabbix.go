// Package zabbix reads the NOC's Zabbix (read-only): open problems and latest item values.
// Logs say what devices report about themselves; Zabbix says whether things actually work
// (ping, links, pool usage, client counts). noc-bot uses it as context and as evidence of impact.
package zabbix

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client calls the Zabbix JSON-RPC API with an API token. Only *.get methods are used.
type Client struct {
	URL, Token string
	HTTP       *http.Client

	mu       sync.Mutex
	problems []Problem
	fetched  time.Time
	hosts    map[string]host // by lower-case host name and by interface IP
}

type host struct{ ID, Name string }

// Problem is one open Zabbix problem.
type Problem struct {
	Host     string
	Name     string
	Severity int // 0 not classified .. 5 disaster
	Since    time.Time
	Acked    bool
}

var sevName = []string{"未分類", "情報", "警告", "軽度の障害", "重度の障害", "致命的"}

// SeverityName is the severity in words.
func (p Problem) SeverityName() string {
	if p.Severity >= 0 && p.Severity < len(sevName) {
		return sevName[p.Severity]
	}
	return strconv.Itoa(p.Severity)
}

// New returns a client. insecure skips TLS verification (the appliance uses a self-signed cert).
func New(url, token string, insecure bool) *Client {
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}
	return &Client{URL: strings.TrimRight(url, "/"), Token: token, HTTP: &http.Client{Timeout: 15 * time.Second, Transport: tr}}
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	if !strings.HasSuffix(method, ".get") {
		return fmt.Errorf("zabbix: only read methods are allowed, not %s", method)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": 1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api_jsonrpc.php", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message, Data string
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("zabbix %s: %s %s", method, r.Error.Message, r.Error.Data)
	}
	return json.Unmarshal(r.Result, out)
}

func (c *Client) loadHosts(ctx context.Context) error {
	var hs []struct {
		HostID     string `json:"hostid"`
		Host       string `json:"host"`
		Name       string `json:"name"`
		Interfaces []struct {
			IP string `json:"ip"`
		} `json:"interfaces"`
	}
	if err := c.call(ctx, "host.get", map[string]any{"output": []string{"hostid", "host", "name"}, "selectInterfaces": []string{"ip"}}, &hs); err != nil {
		return err
	}
	m := map[string]host{}
	for _, h := range hs {
		x := host{h.HostID, h.Host}
		m[strings.ToLower(h.Host)] = x
		for _, i := range h.Interfaces {
			if i.IP != "" && i.IP != "127.0.0.1" {
				m[i.IP] = x
			}
		}
	}
	c.mu.Lock()
	c.hosts = m
	c.mu.Unlock()
	return nil
}

// HostName maps a syslog sender (its host name or IP) to the Zabbix host name, if Zabbix knows it.
func (c *Client) HostName(ctx context.Context, sender string) (string, bool) {
	c.mu.Lock()
	empty := c.hosts == nil
	c.mu.Unlock()
	if empty {
		if err := c.loadHosts(ctx); err != nil {
			return "", false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.hosts[strings.ToLower(sender)]
	return h.Name, ok
}

// Problems lists open problems, newest first (cached for a minute).
func (c *Client) Problems(ctx context.Context) ([]Problem, error) {
	c.mu.Lock()
	if time.Since(c.fetched) < time.Minute && c.problems != nil {
		p := c.problems
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	var ps []struct {
		ObjectID string `json:"objectid"`
		Name     string `json:"name"`
		Severity string `json:"severity"`
		Clock    string `json:"clock"`
		Acked    string `json:"acknowledged"`
	}
	if err := c.call(ctx, "problem.get", map[string]any{"output": []string{"objectid", "name", "severity", "clock", "acknowledged"},
		"sortfield": []string{"eventid"}, "sortorder": "DESC", "limit": 200}, &ps); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ObjectID)
	}
	var trs []struct {
		TriggerID string `json:"triggerid"`
		Hosts     []struct {
			Host string `json:"host"`
		} `json:"hosts"`
	}
	if len(ids) > 0 {
		if err := c.call(ctx, "trigger.get", map[string]any{"triggerids": ids, "output": []string{"triggerid"}, "selectHosts": []string{"host"}}, &trs); err != nil {
			return nil, err
		}
	}
	hostOf := map[string]string{}
	for _, t := range trs {
		if len(t.Hosts) > 0 {
			hostOf[t.TriggerID] = t.Hosts[0].Host
		}
	}
	out := make([]Problem, 0, len(ps))
	for _, p := range ps {
		sev, _ := strconv.Atoi(p.Severity)
		ts, _ := strconv.ParseInt(p.Clock, 10, 64)
		out = append(out, Problem{Host: hostOf[p.ObjectID], Name: p.Name, Severity: sev, Since: time.Unix(ts, 0), Acked: p.Acked == "1"})
	}
	c.mu.Lock()
	c.problems, c.fetched = out, time.Now()
	c.mu.Unlock()
	return out, nil
}

// ProblemsOn lists open problems on one syslog sender with at least the given severity.
func (c *Client) ProblemsOn(ctx context.Context, sender string, minSeverity int) []Problem {
	name, ok := c.HostName(ctx, sender)
	if !ok {
		return nil
	}
	ps, err := c.Problems(ctx)
	if err != nil {
		return nil
	}
	var out []Problem
	for _, p := range ps {
		if p.Host == name && p.Severity >= minSeverity {
			out = append(out, p)
		}
	}
	return out
}

// Item is one latest value.
type Item struct {
	Name, Value, Units string
	At                 time.Time
}

// Items returns latest values whose name contains match. With a sender, that host's items ("" =
// its first 40 by name); without one, matching items on every host (match is then required).
func (c *Client) Items(ctx context.Context, sender, match string) ([]Item, error) {
	params := map[string]any{"output": []string{"name", "lastvalue", "units", "lastclock"},
		"filter": map[string]any{"status": 0}, "sortfield": "name", "limit": 400, "selectHosts": []string{"host"}}
	if sender != "" {
		if _, ok := c.HostName(ctx, sender); !ok {
			return nil, fmt.Errorf("Zabbix does not know a host %q; leave host empty to search every host", sender)
		}
		c.mu.Lock()
		params["hostids"] = []string{c.hosts[strings.ToLower(sender)].ID}
		c.mu.Unlock()
	} else if match == "" {
		return nil, fmt.Errorf("give a host or a match")
	}
	if match != "" {
		params["search"] = map[string]any{"name": match}
	}
	var is []struct {
		Name      string `json:"name"`
		LastValue string `json:"lastvalue"`
		Units     string `json:"units"`
		LastClock string `json:"lastclock"`
		Hosts     []struct {
			Host string `json:"host"`
		} `json:"hosts"`
	}
	if err := c.call(ctx, "item.get", params, &is); err != nil {
		return nil, err
	}
	var out []Item
	for _, i := range is {
		ts, _ := strconv.ParseInt(i.LastClock, 10, 64)
		if ts == 0 {
			continue
		}
		name := i.Name
		if sender == "" && len(i.Hosts) > 0 {
			name = i.Hosts[0].Host + ": " + name
		}
		out = append(out, Item{Name: name, Value: i.LastValue, Units: i.Units, At: time.Unix(ts, 0)})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	if len(out) > 40 {
		out = out[:40]
	}
	return out, nil
}
