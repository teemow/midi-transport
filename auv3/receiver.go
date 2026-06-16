// Package auv3receiver is the off-MCP LAN listener that ingests AUv3
// parameter-tree dumps POSTed by the auv3-probe iPad app
// (github.com/teemow/auv3-probe) and stages them on disk for the
// list_auv3_probes / get_auv3_probe / import_auv3_probe tools.
//
// It is deliberately a SEPARATE HTTP listener from the daemon's MCP endpoint.
// The MCP endpoint is loopback-only (enforced in cmd/mcp-midi-controller), so
// the iPad cannot POST to it; this receiver must bind the LAN instead. Its
// surface is intentionally tiny and write-only: it validates a dump and writes
// <ProbeID>.json into the staging dir. It never touches the engine, transports,
// or hardware, so binding it on the LAN does not widen the control surface.
package auv3

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/teemow/midi-device/device"
	"github.com/teemow/midi-transport/internal/lanhttp"
)

// resp renders this receiver's LAN errors without leaking internal detail.
var resp = lanhttp.Responder{Prefix: "auv3-probe receiver"}

// maxBodyBytes bounds a single request body. The receiver binds the LAN, so an
// unbounded JSON body would let any host on the network drive the daemon out of
// memory. A few MiB is generous for a parameter-tree dump or a probe-run report.
const maxBodyBytes = 8 << 20 // 8 MiB

// Result summarizes a successfully staged dump (also returned to the iPad as
// JSON so the app can confirm the round-trip).
type Result struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Params   int    `json:"params"`
	Writable int    `json:"writable"`
	Staged   string `json:"staged"`
}

// Register adds the probe receiver's routes (NOT /healthz) to mux. The daemon
// uses this to mount the probe surface alongside the AUM-session surface on one
// shared LAN listener; Handler wraps it for standalone use.
//
// Routes:
//
//	POST /auv3-probe              stage one plugin's parameter-tree dump
//	POST /auv3-probe/diagnostics  record a full probe-run report (incl. failures)
func Register(mux *http.ServeMux, outDir string, onStaged func(device.ProbeDump, Result)) {
	mux.HandleFunc("/auv3-probe/diagnostics", handleDiagnostics(outDir))
	mux.HandleFunc("/auv3-probe", handleProbe(outDir, onStaged))
}

// Handler builds the standalone receiver: the probe routes plus a /healthz
// liveness endpoint. Dumps are staged in outDir. If onStaged is non-nil it is
// invoked (synchronously) after each dump is written, e.g. so the daemon can
// notify connected MCP clients that new data arrived.
func Handler(outDir string, onStaged func(device.ProbeDump, Result)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", lanhttp.Healthz)
	Register(mux, outDir, onStaged)
	return mux
}

// handleProbe decodes and validates a ProbeDump, derives its id, and stages it
// as <id>.json in outDir. Validation is intentionally light (the dump is
// authoring input, not a control surface): a dump only needs an id source.
//
// A dump with zero parameters is accepted and staged, not rejected: a plugin
// legitimately exposing no AUM-mappable parameters is useful diagnostic data
// (it tells an agent there is nothing to map), and rejecting it with a 400 was
// a large, spurious source of "errors" when probing every installed plugin.
func handleProbe(outDir string, onStaged func(device.ProbeDump, Result)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		defer func() { _ = r.Body.Close() }()

		var dump device.ProbeDump
		if err := json.NewDecoder(r.Body).Decode(&dump); err != nil {
			resp.Error(w, lanhttp.DecodeErrStatus(err), "decode dump: %v", err)
			return
		}

		id := device.ProbeID(dump)
		if id == "" {
			resp.Error(w, http.StatusBadRequest, "dump has no id source (empty component.subtype and name)")
			return
		}

		b, err := json.MarshalIndent(dump, "", "  ")
		if err != nil {
			resp.Error(w, http.StatusInternalServerError, "re-encode dump: %v", err)
			return
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			resp.Error(w, http.StatusInternalServerError, "create out dir %s: %v", outDir, err)
			return
		}
		path := filepath.Join(outDir, id+".json")
		if err := lanhttp.WriteFileAtomic(path, b, 0o644); err != nil {
			resp.Error(w, http.StatusInternalServerError, "write %s: %v", path, err)
			return
		}

		writable := 0
		for _, p := range dump.Parameters {
			if p.Writable {
				writable++
			}
		}
		res := Result{ID: id, Name: dump.Name, Params: len(dump.Parameters), Writable: writable, Staged: path}
		log.Printf("staged %s -> %s: %d params, %d writable", dump.Name, path, res.Params, res.Writable)
		if onStaged != nil {
			onStaged(dump, res)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}

// DiagnosticsResult summarizes a stored probe-run report (returned to the app).
type DiagnosticsResult struct {
	Total  int    `json:"total"`
	Sent   int    `json:"sent"`
	Empty  int    `json:"empty"`
	Failed int    `json:"failed"`
	Stored string `json:"stored"`
}

// handleDiagnostics records a full probe-run report — including the plugins
// that failed to instantiate or had no parameter tree, which never produce a
// dump — under outDir/_diagnostics/<timestamp>.json. This is what makes "all
// diagnostic data and errors" land on the receiver instead of staying only in
// the iPad app's UI.
func handleDiagnostics(outDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		defer func() { _ = r.Body.Close() }()

		var report device.ProbeReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			resp.Error(w, lanhttp.DecodeErrStatus(err), "decode report: %v", err)
			return
		}

		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			resp.Error(w, http.StatusInternalServerError, "re-encode report: %v", err)
			return
		}
		diagDir := filepath.Join(outDir, "_diagnostics")
		if err := os.MkdirAll(diagDir, 0o755); err != nil {
			resp.Error(w, http.StatusInternalServerError, "create diagnostics dir %s: %v", diagDir, err)
			return
		}
		// Nanosecond precision so two reports posted in the same second do not
		// overwrite each other.
		name := time.Now().UTC().Format("20060102T150405.000000000Z") + ".json"
		path := filepath.Join(diagDir, name)
		if err := lanhttp.WriteFileAtomic(path, b, 0o644); err != nil {
			resp.Error(w, http.StatusInternalServerError, "write %s: %v", path, err)
			return
		}

		total, sent, empty, failed := report.Summary()
		log.Printf("probe run report: %d plugins (%d sent, %d empty, %d failed) -> %s",
			total, sent, empty, failed, path)
		for _, res := range report.Results {
			if res.Status == "failed" {
				log.Printf("  probe failed: %s (%s): %s", res.Name, res.ID, res.Error)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(DiagnosticsResult{
			Total: total, Sent: sent, Empty: empty, Failed: failed, Stored: path,
		})
	}
}
