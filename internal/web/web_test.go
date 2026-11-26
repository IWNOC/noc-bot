package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPublic(t *testing.T) {
	for _, s := range []string{"10.110.51.4", "100.81.161.88", "127.0.0.1", "192.168.1.1", "::1", "fd00::1", "169.254.169.254"} {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("%s must not be public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700:4700::1111", "163.220.192.86"} {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("%s is public", s)
		}
	}
}

func TestFetchRefusesInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	if _, err := New("").Fetch(context.Background(), srv.URL, 100); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("fetched an internal address: %v", err)
	}
}

func TestSearchRefusesAddresses(t *testing.T) {
	c := New("http://127.0.0.1:1")
	for _, q := range []string{"peer 163.220.192.86 bad AS", "mac 92:3a:cd:f8:5c:4b", "2401:5e40:c600::4:5679:1 down"} {
		if _, err := c.Search(context.Background(), q); err == nil || !strings.Contains(err.Error(), "address") {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestHTMLText(t *testing.T) {
	got := htmlText(strings.NewReader(`<html><head><script>x()</script><style>p{}</style></head><body><nav>menu</nav><p>BAD_AS_NUMBER means</p><p>the peer AS differs</p></body></html>`))
	if strings.Contains(got, "x()") || strings.Contains(got, "menu") || !strings.Contains(got, "BAD_AS_NUMBER means") {
		t.Fatalf("%q", got)
	}
}

func TestLookupIPRefusesPrivate(t *testing.T) {
	for _, ip := range []string{"10.110.51.4", "100.81.161.88", "fd7a:115c:a1e0::1"} {
		if _, err := New("").LookupIP(context.Background(), ip); err == nil {
			t.Errorf("%s looked up", ip)
		}
	}
}
