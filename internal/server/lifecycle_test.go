package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shichao-wang/cpagw/internal/gateway"
	"github.com/shichao-wang/cpagw/internal/store"
)

func TestStatusStoppedAndStalePID(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), s)
	if err != nil || status.State != "stopped" {
		t.Fatalf("%+v %v", status, err)
	}
	r := gateway.RuntimeState{PID: os.Getpid(), InstanceID: "other", StartTime: "not-this-process", Listen: "127.0.0.1:8317", ProbeToken: "private", Ready: true}
	if err = store.WriteJSON(s.Path(gateway.RuntimeFileName), r); err != nil {
		t.Fatal(err)
	}
	status, err = Status(context.Background(), s)
	if err != nil || status.State != "stopped" {
		t.Fatalf("%+v %v", status, err)
	}
	if Stop(context.Background(), s) == nil {
		t.Fatal("陈旧 PID 必须拒绝停止，不能向当前测试进程发送信号")
	}
}
func TestStatusRequiresMatchingInstanceProbe(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	birth, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	var instance atomic.Value
	instance.Store("matching")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != gateway.ReadyPath || r.Header.Get("X-CPAGW-Instance-Token") != "private-token" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"ready":true,"instanceID":%q,"revision":42}`, instance.Load())
	}))
	defer mock.Close()
	r := gateway.RuntimeState{PID: os.Getpid(), InstanceID: "matching", StartTime: birth, Listen: strings.TrimPrefix(mock.URL, "http://"), ProbeToken: "private-token", Ready: true}
	if err = store.WriteJSON(s.Path(gateway.RuntimeFileName), r); err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), s)
	if err != nil || status.State != "running" || status.Revision != 42 {
		t.Fatalf("%+v %v", status, err)
	}
	instance.Store("wrong")
	status, err = Status(context.Background(), s)
	if err != nil || status.State != "unresponsive" {
		t.Fatalf("%+v %v", status, err)
	}
}
