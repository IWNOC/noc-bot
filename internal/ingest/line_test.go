package ingest

import (
	"testing"
	"time"
)

func TestParseArista(t *testing.T) {
	l, err := Parse("2026-11-25T10:32:35.942572+00:00 10.110.0.1 eps-rt-01 devtime=2026-11-25T19:32:35+00:00 Bgp: %BGP-5-ADJCHANGE: peer 163.220.192.86 (VRF default AS 45679) old state OpenConfirm")
	if err != nil {
		t.Fatal(err)
	}
	if l.From != "10.110.0.1" || l.Host != "eps-rt-01" || l.Program != "Bgp" {
		t.Fatalf("got %+v", l)
	}
	if l.Msg != "%BGP-5-ADJCHANGE: peer 163.220.192.86 (VRF default AS 45679) old state OpenConfirm" {
		t.Fatalf("msg %q", l.Msg)
	}
	if got := l.Skew().Round(time.Hour); got != 9*time.Hour {
		t.Fatalf("skew %v", got)
	}
}

func TestParseKeaWithBOM(t *testing.T) {
	l, err := Parse("2026-11-25T10:33:24.204414+00:00 10.110.1.14 kea-primary-65c7995745-v5wzj devtime=2026-11-25T10:33:23.460337Z kea-dhcp4 \ufeff2026-11-25 10:33:23.460 INFO  [kea-dhcp4.packets/1.1] DHCP4_PACKET")
	if err != nil {
		t.Fatal(err)
	}
	if l.Program != "kea-dhcp4" || l.Msg[:4] != "2026" {
		t.Fatalf("got %+v", l)
	}
}

func TestParsePid(t *testing.T) {
	l, err := Parse("2026-11-25T17:00:00Z 10.20.0.2 eps-rt-02 devtime=2026-11-25T17:00:00Z dhclient[4242]: DHCPDISCOVER on eth3 to 255.255.255.255 port 67 interval 7")
	if err != nil || l.Program != "dhclient" {
		t.Fatalf("got %+v %v", l, err)
	}
}

func TestParseBad(t *testing.T) {
	for _, s := range []string{"", "hello world", "x y z w v"} {
		if _, err := Parse(s); err == nil {
			t.Fatalf("expected error for %q", s)
		}
	}
}
