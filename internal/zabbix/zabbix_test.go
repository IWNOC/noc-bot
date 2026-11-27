package zabbix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProblemsOnAndReadOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("no token")
		}
		var req struct{ Method string }
		json.NewDecoder(r.Body).Decode(&req)
		res := map[string]string{
			"host.get":    `[{"hostid":"1","host":"eps-vpn-01","name":"eps-vpn-01","interfaces":[{"ip":"10.20.0.1"}]}]`,
			"problem.get": `[{"objectid":"7","name":"Unavailable by ICMP ping","severity":"4","clock":"1795800000","acknowledged":"0"}]`,
			"trigger.get": `[{"triggerid":"7","hosts":[{"host":"eps-vpn-01"}]}]`,
		}[req.Method]
		w.Write([]byte(`{"jsonrpc":"2.0","result":` + res + `,"id":1}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", false)
	// the syslog sender is the IP; Zabbix knows it by name
	ps := c.ProblemsOn(context.Background(), "10.20.0.1", 3)
	if len(ps) != 1 || ps[0].Host != "eps-vpn-01" || ps[0].SeverityName() != "重度の障害" {
		t.Fatalf("%+v", ps)
	}
	if err := c.call(context.Background(), "host.update", nil, nil); err == nil {
		t.Fatal("write methods must be refused")
	}
}
