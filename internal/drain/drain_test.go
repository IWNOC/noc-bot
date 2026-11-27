package drain

import "testing"

func TestBGPLinesShareTemplate(t *testing.T) {
	m := New(0.5)
	a := m.Add("Bgp", "%BGP-3-NOTIFICATION: received from neighbor 163.220.192.86 (VRF default AS 45679) 4/0 (Hold Timer Expired Error/None) 0 bytes")
	b := m.Add("Bgp", "%BGP-3-NOTIFICATION: received from neighbor 2401:5e40:c600::4:5679:1 (VRF default AS 45679) 4/0 (Hold Timer Expired Error/None) 0 bytes")
	if !a.New {
		t.Fatal("first should be new")
	}
	if b.New || a.ID != b.ID {
		t.Fatalf("v4 and v6 peers should share a template: %+v %+v", a, b)
	}
}

func TestDifferentMessagesDiffer(t *testing.T) {
	m := New(0.5)
	a := m.Add("vyos-hostsd", "Writing /etc/resolv.conf")
	b := m.Add("vyos-hostsd", "Writing /etc/hosts")
	c := m.Add("vyos-hostsd", "Saving state to /run/vyos-hostsd/vyos-hostsd.state")
	if a.ID == c.ID {
		t.Fatal("different first tokens / lengths must differ")
	}
	// same shape, one differing token: merged with a wildcard
	if a.ID != b.ID || b.Template != "Writing <*>" {
		t.Fatalf("got %+v %+v", a, b)
	}
}

func TestIDStableAcrossGeneralisationAndRestore(t *testing.T) {
	m := New(0.5)
	a := m.Add("dhclient", "DHCPDISCOVER on eth3 to 255.255.255.255 port 67 interval 7")
	m.Add("dhclient", "DHCPDISCOVER on eth4 to 255.255.255.255 port 67 interval 12")
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	m2 := New(0.5)
	if err := m2.Restore(snap); err != nil {
		t.Fatal(err)
	}
	c := m2.Add("dhclient", "DHCPDISCOVER on eth5 to 255.255.255.255 port 67 interval 3")
	if c.New || c.ID != a.ID {
		t.Fatalf("id changed: %s vs %s", c.ID, a.ID)
	}
}

func TestMask(t *testing.T) {
	got := Mask("user=x mac=92:3a:cd:f8:5c:4b ip 10.1.2.3 v6 2001:d90:30:0:e541::1 n=42")
	want := "user=<USER> mac=<MAC> ip <IP> v6 <IP6> n=<NUM>"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestMaskKeepsClockTimes(t *testing.T) {
	if got := Mask("at 10:33:23 peer 163.220.192.86 and 2401:5e40:c600::4:5679:1"); got != "at <NUM>:<NUM>:<NUM> peer <IP> and <IP6>" {
		t.Fatalf("got %q", got)
	}
}
