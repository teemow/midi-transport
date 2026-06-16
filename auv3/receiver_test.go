package auv3

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teemow/midi-device/device"
)

func TestHandlerStagesDumpAndNotifies(t *testing.T) {
	dir := t.TempDir()

	var gotDump device.ProbeDump
	var gotRes Result
	called := 0
	h := Handler(dir, func(d device.ProbeDump, r Result) {
		gotDump = d
		gotRes = r
		called++
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := `{
		"component": {"type": "aufx", "subtype": "dely", "manufacturer": "appl"},
		"name": "AUDelay",
		"parameters": [
			{"address": 0, "keyPath": "global.0", "identifier": "0", "displayName": "Dry/Wet Mix",
			 "min": 0, "max": 100, "value": 50, "unit": "equalPowerCrossfade", "writable": true, "readable": true},
			{"address": 1, "keyPath": "global.1", "identifier": "1", "displayName": "Delay Time",
			 "min": 0, "max": 2, "value": 1, "unit": "seconds", "writable": true, "readable": true}
		]
	}`
	resp, err := http.Post(ts.URL+"/auv3-probe", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var res Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.ID != "dely" || res.Params != 2 || res.Writable != 2 {
		t.Fatalf("response = %+v, want id=dely params=2 writable=2", res)
	}

	staged := filepath.Join(dir, "dely.json")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("expected staged file %s: %v", staged, err)
	}
	b, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	var roundtrip device.ProbeDump
	if err := json.Unmarshal(b, &roundtrip); err != nil {
		t.Fatalf("staged file is not valid ProbeDump JSON: %v", err)
	}
	if roundtrip.Name != "AUDelay" || len(roundtrip.Parameters) != 2 {
		t.Fatalf("staged dump = %+v, want AUDelay with 2 params", roundtrip)
	}

	if called != 1 {
		t.Fatalf("onStaged called %d times, want 1", called)
	}
	if gotRes.Staged != staged || gotDump.Name != "AUDelay" {
		t.Fatalf("onStaged got res=%+v dump.Name=%q", gotRes, gotDump.Name)
	}
}

func TestHandlerPreservesRichMetadata(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(Handler(dir, nil))
	defer ts.Close()

	// Unit-level metadata (channelCapabilities/latency/tailTime/
	// supportsUserPresets) and per-param dependentParameters must survive the
	// receiver's decode -> re-encode round-trip so the JSON contract with the
	// iPad app stays intact.
	body := `{
		"component": {"type": "aumf", "subtype": "verb", "manufacturer": "appl"},
		"name": "BigReverb",
		"channelCapabilities": [1, 2, 2, 2],
		"latency": 0.0123,
		"tailTime": 4.5,
		"supportsUserPresets": true,
		"factoryPresets": [{"number": 0, "name": "Hall"}],
		"userPresets": [{"number": -1, "name": "My Plate"}],
		"parameters": [
			{"address": 10, "keyPath": "global.macro", "identifier": "macro", "displayName": "Macro",
			 "min": 0, "max": 1, "value": 0.5, "unit": "generic", "writable": true, "readable": true,
			 "dependentParameters": [11, 12, 13]}
		]
	}`
	resp, err := http.Post(ts.URL+"/auv3-probe", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	b, err := os.ReadFile(filepath.Join(dir, "verb.json"))
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	var rt device.ProbeDump
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatalf("staged file is not valid ProbeDump JSON: %v", err)
	}
	if len(rt.ChannelCapabilities) != 4 || rt.ChannelCapabilities[3] != 2 {
		t.Fatalf("channelCapabilities = %v, want [1 2 2 2]", rt.ChannelCapabilities)
	}
	if rt.Latency != 0.0123 || rt.TailTime != 4.5 || !rt.SupportsUserPresets {
		t.Fatalf("unit metadata = latency %v tail %v userPresets %v", rt.Latency, rt.TailTime, rt.SupportsUserPresets)
	}
	if len(rt.UserPresets) != 1 || rt.UserPresets[0].Number != -1 || rt.UserPresets[0].Name != "My Plate" {
		t.Fatalf("userPresets = %+v, want [{-1 My Plate}]", rt.UserPresets)
	}
	if len(rt.FactoryPresets) != 1 || rt.FactoryPresets[0].Name != "Hall" {
		t.Fatalf("factoryPresets = %+v, want [{0 Hall}]", rt.FactoryPresets)
	}
	if len(rt.Parameters) != 1 || len(rt.Parameters[0].DependentParameters) != 3 || rt.Parameters[0].DependentParameters[0] != 11 {
		t.Fatalf("dependentParameters = %v, want [11 12 13]", rt.Parameters[0].DependentParameters)
	}
}

func TestHandlerRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(Handler(dir, nil))
	defer ts.Close()

	t.Run("GET is rejected", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/auv3-probe")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("dump with no id source is rejected", func(t *testing.T) {
		body := `{"component": {"subtype": ""}, "name": "", "parameters": []}`
		resp, err := http.Post(ts.URL+"/auv3-probe", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("healthz is ok", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("GET healthz: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
}

func TestHandlerAcceptsEmptyParamDump(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(Handler(dir, nil))
	defer ts.Close()

	// A plugin with no AUM-mappable parameters is valid diagnostic data, not an
	// error: the dump is staged and reported with params=0.
	body := `{"component": {"type": "aufx", "subtype": "util", "manufacturer": "appl"}, "name": "Utility", "parameters": []}`
	resp, err := http.Post(ts.URL+"/auv3-probe", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var res Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.ID != "util" || res.Params != 0 || res.Writable != 0 {
		t.Fatalf("response = %+v, want id=util params=0 writable=0", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "util.json")); err != nil {
		t.Fatalf("expected staged file util.json: %v", err)
	}
}

func TestHandlerStoresDiagnosticsReport(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(Handler(dir, nil))
	defer ts.Close()

	body := `{
		"app": "auv3-probe 1.1.0",
		"device": {"model": "iPad", "systemName": "iPadOS", "systemVersion": "26.0"},
		"results": [
			{"id": "isem", "name": "iSEM", "status": "sent", "params": 40, "writable": 38},
			{"id": "util", "name": "Utility", "status": "empty", "params": 0, "writable": 0},
			{"id": "broken", "name": "Broken FX", "status": "failed", "error": "could not instantiate audio unit"}
		]
	}`
	resp, err := http.Post(ts.URL+"/auv3-probe/diagnostics", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var res DiagnosticsResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Total != 3 || res.Sent != 1 || res.Empty != 1 || res.Failed != 1 {
		t.Fatalf("summary = %+v, want total=3 sent=1 empty=1 failed=1", res)
	}
	if res.Stored == "" {
		t.Fatalf("expected a stored path")
	}
	if _, err := os.Stat(res.Stored); err != nil {
		t.Fatalf("expected stored report at %s: %v", res.Stored, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "_diagnostics"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one diagnostics file, got %v (err %v)", entries, err)
	}
}
