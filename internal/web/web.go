// Package web lets the conversation agent look things up on the internet: search through a local
// SearXNG, and read public pages. Two guards: search queries must not carry addresses from our
// logs, and page fetches only reach public internet addresses (never 10.x, 100.64/10, localhost and
// the like), so a page cannot steer the agent into our own network.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Client searches and fetches.
type Client struct {
	SearxURL string // e.g. http://127.0.0.1:8888
	search   *http.Client
	fetch    *http.Client
}

// New returns a client. Fetches bypass any proxy and refuse non-public addresses.
func New(searxURL string) *Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: nil}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if Public(ip) {
					return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				}
			}
			return nil, fmt.Errorf("%s is not a public internet address", host)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	return &Client{
		SearxURL: strings.TrimRight(searxURL, "/"),
		search:   &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{Proxy: nil}},
		fetch: &http.Client{Timeout: 30 * time.Second, Transport: tr, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if r.URL.Scheme != "https" && r.URL.Scheme != "http" {
				return errors.New("only http and https")
			}
			return nil
		}},
	}
}

var private = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Public reports whether ip is a public internet address.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range private {
		if p.Contains(ip) {
			return false
		}
	}
	return ip.IsValid()
}

var leak = regexp.MustCompile(`(?i)\b\d{1,3}(?:\.\d{1,3}){3}\b|(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}|\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`)

// Result is one search hit.
type Result struct {
	Title, URL, Snippet string
}

// Search asks SearXNG. Queries with IP or MAC addresses are refused: search with the log pattern
// (addresses already replaced by <IP>), the vendor, the message name.
func (c *Client) Search(ctx context.Context, q string) ([]Result, error) {
	if leak.MatchString(q) {
		return nil, errors.New("the query contains an address from our network; remove IPs/MACs and search with the message name or pattern")
	}
	u := c.SearxURL + "/search?format=json&q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.search.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Results []struct {
			Title, URL, Content string
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&r); err != nil {
		return nil, err
	}
	var out []Result
	for i, x := range r.Results {
		if i >= 8 {
			break
		}
		out = append(out, Result{Title: x.Title, URL: x.URL, Snippet: x.Content})
	}
	return out, nil
}

// Fetch reads a public page and returns its text (at most max characters).
func (c *Client) Fetch(ctx context.Context, raw string, max int) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("only http(s) URLs")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; noc-bot/0.1)")
	resp, err := c.fetch.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body := io.LimitReader(resp.Body, 3<<20)
	ct := resp.Header.Get("Content-Type")
	var text string
	if strings.Contains(ct, "html") || ct == "" {
		text = htmlText(body)
	} else if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") {
		b, _ := io.ReadAll(body)
		text = string(b)
	} else {
		return "", fmt.Errorf("not a text page (%s)", ct)
	}
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > max {
		text = string(r[:max]) + "…"
	}
	return text, nil
}

func htmlText(r io.Reader) string {
	z := html.NewTokenizer(r)
	var sb strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return sb.String()
		case html.StartTagToken:
			n, _ := z.TagName()
			switch string(n) {
			case "script", "style", "noscript", "svg", "nav", "footer", "header":
				skip++
			case "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "pre":
				sb.WriteString("\n")
			}
		case html.EndTagToken:
			n, _ := z.TagName()
			switch string(n) {
			case "script", "style", "noscript", "svg", "nav", "footer", "header":
				if skip > 0 {
					skip--
				}
			}
		case html.TextToken:
			if skip == 0 {
				sb.Write(z.Text())
				sb.WriteString(" ")
			}
		}
	}
}

// getJSON reads a public JSON API.
func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "noc-bot/0.1")
	resp, err := c.fetch.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, u)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// LookupASN says who is behind an AS: RIPEstat (holder, announcing, abuse contact) and PeeringDB.
func (c *Client) LookupASN(ctx context.Context, asn string) (string, error) {
	asn = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(asn)), "AS")
	if _, err := fmt.Sscan(asn, new(uint32)); err != nil {
		return "", fmt.Errorf("not an AS number: %q", asn)
	}
	var sb strings.Builder
	var ov struct {
		Data struct {
			Holder    string `json:"holder"`
			Announced bool   `json:"announced"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "https://stat.ripe.net/data/as-overview/data.json?resource=AS"+asn, &ov); err == nil {
		fmt.Fprintf(&sb, "AS%s holder: %s (announcing prefixes: %v) [RIPEstat]\n", asn, ov.Data.Holder, ov.Data.Announced)
	}
	var ab struct {
		Data struct {
			Abuse []string `json:"abuse_contacts"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "https://stat.ripe.net/data/abuse-contact-finder/data.json?resource=AS"+asn, &ab); err == nil && len(ab.Data.Abuse) > 0 {
		fmt.Fprintf(&sb, "abuse contact: %s [RIPEstat]\n", strings.Join(ab.Data.Abuse, ", "))
	}
	var pd struct {
		Data []struct {
			Name     string `json:"name"`
			AKA      string `json:"aka"`
			Website  string `json:"website"`
			Type     string `json:"info_type"`
			Policy   string `json:"policy_general"`
			ASSet    string `json:"irr_as_set"`
			Notes    string `json:"notes"`
			LookGlas string `json:"looking_glass"`
			ID       int    `json:"id"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "https://www.peeringdb.com/api/net?asn="+asn, &pd); err == nil {
		for _, n := range pd.Data {
			fmt.Fprintf(&sb, "PeeringDB: %s", n.Name)
			if n.AKA != "" {
				fmt.Fprintf(&sb, " (aka %s)", n.AKA)
			}
			fmt.Fprintf(&sb, ", type %s, peering policy %s, as-set %s, website %s, https://www.peeringdb.com/net/%d\n", n.Type, n.Policy, n.ASSet, n.Website, n.ID)
			if n.LookGlas != "" {
				fmt.Fprintf(&sb, "looking glass: %s\n", n.LookGlas)
			}
			if n.Notes != "" {
				fmt.Fprintf(&sb, "notes: %s\n", clip(n.Notes, 400))
			}
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("nothing found for AS%s", asn)
	}
	fmt.Fprintf(&sb, "more: https://bgp.he.net/AS%s\n", asn)
	return sb.String(), nil
}

// LookupIP says which prefix and AS a public address belongs to. Private and internal addresses
// are refused: they are ours, and lookups would only leak them.
func (c *Client) LookupIP(ctx context.Context, ip string) (string, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "", fmt.Errorf("not an IP address: %q", ip)
	}
	if !Public(a) {
		return "", errors.New("that is a private or internal address; only public addresses can be looked up")
	}
	var po struct {
		Data struct {
			Resource string `json:"resource"`
			ASNs     []struct {
				ASN    int    `json:"asn"`
				Holder string `json:"holder"`
			} `json:"asns"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "https://stat.ripe.net/data/prefix-overview/data.json?resource="+a.String(), &po); err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s is in %s [RIPEstat]\n", a, po.Data.Resource)
	for _, x := range po.Data.ASNs {
		fmt.Fprintf(&sb, "origin AS%d: %s\n", x.ASN, x.Holder)
	}
	var ab struct {
		Data struct {
			Abuse []string `json:"abuse_contacts"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "https://stat.ripe.net/data/abuse-contact-finder/data.json?resource="+a.String(), &ab); err == nil && len(ab.Data.Abuse) > 0 {
		fmt.Fprintf(&sb, "abuse contact: %s\n", strings.Join(ab.Data.Abuse, ", "))
	}
	return sb.String(), nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
