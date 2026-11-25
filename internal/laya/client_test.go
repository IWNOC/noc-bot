package laya

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A real laya-svc reply: boolean questions carry true/false in prediction_set.
func TestDecideReadsBooleanPredictionSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"odd":{"answer":true,"prediction_set":[true],"route":"review","source":"teacher"},
		"team":{"answer":"l2l3","prediction_set":["l2l3","server"],"source":"vega"},
		"severity":{"answer":2,"prediction_set":[1,2]}},"teacher_called":true}`))
	}))
	defer srv.Close()
	d, err := New(srv.URL, "", time.Second).Decide(context.Background(), "line", "x")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := d.Answers["odd"].Bool(); !ok || !v || d.Answers["team"].String() != "l2l3" {
		t.Fatalf("%+v", d)
	}
	if n, ok := d.Answers["severity"].Int(); !ok || n != 2 {
		t.Fatalf("severity %+v", d.Answers["severity"])
	}
}

func TestUnreached(t *testing.T) {
	_, err := New("http://127.0.0.1:1", "", 200*time.Millisecond).Decide(context.Background(), "line", "x")
	if !Unreached(err) {
		t.Fatalf("connection refused must be unreached: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"odd":{"prediction_set":"bad"}}}`))
	}))
	defer srv.Close()
	_, err = New(srv.URL, "", time.Second).Decide(context.Background(), "line", "x")
	if err == nil || Unreached(err) {
		t.Fatalf("an unreadable answer was still decided, must not be retried: %v", err)
	}
}
