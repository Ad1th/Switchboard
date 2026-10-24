package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ad1th/Switchboard/internal/balancer"
	"github.com/Ad1th/Switchboard/internal/demo"
)

func setup(t *testing.T, token string) (*httptest.Server, *Server) {
	t.Helper()
	fleet := NewFleet()
	t.Cleanup(fleet.Close)
	var bcs []balancer.BackendConfig
	for range 2 {
		d, err := fleet.Spawn(demo.Chaos{})
		if err != nil {
			t.Fatal(err)
		}
		bcs = append(bcs, balancer.BackendConfig{URL: d.URL})
	}
	pool, err := balancer.New(bcs, balancer.Options{Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	pool.CheckAll()
	s := &Server{Pool: pool, Fleet: fleet, AdminToken: token}
	mux := http.NewServeMux()
	mux.Handle("/", pool)
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, s
}

func call(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestControlPanelServes(t *testing.T) {
	ts, _ := setup(t, "")
	for _, path := range []string{"/_switchboard/", "/_switchboard/assets/app.js", "/_switchboard/assets/app.css", "/_switchboard/metrics"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: got %d", path, resp.StatusCode)
		}
	}
}

func TestAdminAPI(t *testing.T) {
	ts, s := setup(t, "")
	api := ts.URL + "/_switchboard/api/"

	if code, st := call(t, "PUT", api+"strategy", "", `{"name":"hash"}`); code != 200 || st["strategy"] != "hash" {
		t.Fatalf("strategy: %d %v", code, st)
	}
	if code, _ := call(t, "PUT", api+"strategy", "", `{"name":"nope"}`); code != 400 {
		t.Fatalf("bad strategy accepted: %d", code)
	}
	if code, st := call(t, "POST", api+"backends", "", `{"demo":true}`); code != 201 || len(st["backends"].([]any)) != 3 {
		t.Fatalf("install demo line: %d %v", code, st)
	}
	if code, _ := call(t, "PATCH", api+"backends/1", "", `{"weight":4,"draining":true}`); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if b := s.Pool.Get("1"); b.Weight() != 4 {
		t.Fatalf("weight not applied")
	}
	if code, c := call(t, "PUT", api+"backends/2/chaos", "", `{"error_rate":0.5}`); code != 200 || c["error_rate"] != 0.5 {
		t.Fatalf("chaos: %d %v", code, c)
	}
	if code, _ := call(t, "DELETE", api+"backends/3", "", ""); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call(t, "PUT", api+"loadgen", "", `{"rps":10}`); code != 400 {
		t.Fatalf("loadgen without generator should fail, got %d", code)
	}
}

func TestChaosCannotBeChangedThroughTheProxy(t *testing.T) {
	ts, _ := setup(t, "")
	code, _ := call(t, "POST", ts.URL+"/_chaos", "", `{"down":true}`)
	if code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", code)
	}
}

func TestAdminToken(t *testing.T) {
	ts, _ := setup(t, "s3cret")
	api := ts.URL + "/_switchboard/api/"
	if code, _ := call(t, "PUT", api+"strategy", "", `{"name":"p2c"}`); code != 401 {
		t.Fatalf("no token: got %d, want 401", code)
	}
	if code, _ := call(t, "PUT", api+"strategy", "wrong", `{"name":"p2c"}`); code != 401 {
		t.Fatalf("wrong token: got %d, want 401", code)
	}
	if code, _ := call(t, "PUT", api+"strategy", "s3cret", `{"name":"p2c"}`); code != 200 {
		t.Fatalf("right token: got %d", code)
	}
	if code, st := call(t, "GET", api+"state", "", ""); code != 200 || st["locked"] != true {
		t.Fatalf("state should stay readable: %d", code)
	}
}

func TestEventStream(t *testing.T) {
	ts, s := setup(t, "")
	go s.Pool.Run(t.Context(), time.Hour)

	resp, err := http.Get(ts.URL + "/_switchboard/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(200 * time.Millisecond)
		http.Get(ts.URL + "/hello")
	}()

	seen := map[string]bool{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	deadline := time.After(3 * time.Second)
	for !seen["calls"] {
		select {
		case <-deadline:
			t.Fatalf("saw events %v, want state and calls", seen)
		default:
		}
		if !sc.Scan() {
			t.Fatal("stream ended")
		}
		if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			seen[ev] = true
		}
	}
	if !seen["state"] {
		t.Fatal("no state event")
	}
}
