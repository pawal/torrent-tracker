// Package api exposes the collected history over HTTP. It is deliberately
// read-only: everything that mutates the registry lives in the CLI, so the
// server needs no authentication.
package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pawal/torrent-tracker/internal/store"
	"github.com/pawal/torrent-tracker/internal/version"
)

// Server wires the store to an http.Handler.
type Server struct {
	Store *store.Store
	Log   *slog.Logger
	// Static, if non-nil, is served at / as the frontend.
	Static fs.FS
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/trackers", s.handleTrackers)
	mux.HandleFunc("GET /api/trackers/{name}", s.handleTracker)
	mux.HandleFunc("GET /api/changes", s.handleChanges)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/networks", s.handleNetworks)
	mux.HandleFunc("GET /api/list", s.handleList)
	mux.HandleFunc("GET /api/list/{filter}", s.handleList)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	})

	// Unknown API paths must fail as JSON rather than falling through to the
	// SPA, which would hand the caller HTML and a 200.
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, http.StatusNotFound, "no such endpoint")
	})

	if s.Static != nil {
		mux.Handle("GET /", s.spaHandler())
	}
	return logging(s.logger(), cors(mux))
}

// cors opens /api/ to any origin. The data is public and read-only, so there is
// nothing for the same-origin policy to protect, and letting other sites query
// it directly is the point of publishing it.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			// Preflights never reach the mux, which routes GET only and
			// would answer them with a 405.
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "*")
				w.Header().Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// spaHandler serves the built frontend, falling back to index.html so client
// routes deep-link correctly.
func (s *Server) spaHandler() http.Handler {
	files := http.FileServer(http.FS(s.Static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path != "/" {
			if _, err := fs.Stat(s.Static, path[1:]); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug("request", "method", r.Method, "path", r.URL.Path,
			"duration", time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger().Error("encode response", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// serverError logs the detail and returns a generic message to the client.
func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.logger().Error("request failed", "err", err)
	s.fail(w, http.StatusInternalServerError, "internal error")
}

// handleVersion reports the build's own version and the DNS library behind
// every lookup. Cached: it cannot change without a restart.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{"version": version.Version}
	if dns := version.DNSLib(); dns != "" {
		resp["dns"] = dns
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Store.Stats(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleTrackers(w http.ResponseWriter, r *http.Request) {
	includeDisabled := r.URL.Query().Get("all") == "1"
	views, err := s.Store.ListTrackerViews(r.Context(), includeDisabled)
	if err != nil {
		s.serverError(w, err)
		return
	}
	until := time.Now().UTC()
	win, err := s.Store.AvailabilityOver(r.Context(), until.AddDate(0, 0, -intParam(r, "days", 30)), until)
	if err != nil {
		s.serverError(w, err)
		return
	}
	for i := range views {
		if a, ok := win.Trackers[views[i].ID]; ok && a.Known() {
			share := a.Share()
			views[i].Uptime = &share
		}
	}
	s.writeJSON(w, http.StatusOK, views)
}

// trackerDetail is the payload for a single tracker page. Info is keyed by
// address so the UI can annotate each interval; probes reference endpoints by
// id, since one address can answer on one endpoint and not another.
type trackerDetail struct {
	store.Tracker
	Records   []store.IPRecord        `json:"records"`
	Changes   []store.Change          `json:"changes"`
	Info      map[string]store.IPInfo `json:"info"`
	Endpoints []store.Endpoint        `json:"endpoints"`
	Probes    []store.Probe           `json:"probes"`
	// History is the closed probe intervals in the window; the open one is the
	// matching row in Probes, so together they cover the axis.
	History     []store.ProbeInterval `json:"probe_history"`
	HistoryFrom time.Time             `json:"probe_history_from"`
	// Resolution is the DNS status over the same window, so a page can show
	// whether a name stopped answering because it stopped resolving.
	Resolution      []store.StatusInterval `json:"resolution"`
	ResolutionStats store.ResolutionStats  `json:"resolution_stats"`
}

func (s *Server) handleTracker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	t, err := s.Store.TrackerByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "no such tracker")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}

	records, err := s.Store.RecordsFor(r.Context(), t.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	changes, err := s.Store.ChangesFor(r.Context(), t.ID, intParam(r, "limit", 200))
	if err != nil {
		s.serverError(w, err)
		return
	}
	info, err := s.Store.IPInfoForTracker(r.Context(), t.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	endpoints, err := s.Store.EndpointsFor(r.Context(), t.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	probes, err := s.Store.ProbesFor(r.Context(), t.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	from := time.Now().UTC().AddDate(0, 0, -intParam(r, "days", 30))
	history, err := s.Store.ProbeHistoryFor(r.Context(), t.ID, from)
	if err != nil {
		s.serverError(w, err)
		return
	}
	resolution, latency, err := s.Store.ResolutionHistoryFor(r.Context(), t.ID, from)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, trackerDetail{
		Tracker: t, Records: records, Changes: changes, Info: info,
		Endpoints: endpoints, Probes: probes,
		History: history, HistoryFrom: from,
		Resolution: resolution, ResolutionStats: latency,
	})
}

// networksResponse summarises where the tracked hosts actually live, and how
// many of them still answer.
type networksResponse struct {
	Coverage  store.EnrichmentCoverage `json:"coverage"`
	Probes    store.EndpointCoverage   `json:"probes"`
	Reach     map[store.Reach]int      `json:"reach"`
	Networks  []store.NetworkStat      `json:"networks"`
	RIRs      []store.NetworkStat      `json:"rirs"`
	Countries []store.NetworkStat      `json:"countries"`
	Software  []store.SoftwareStat     `json:"software"`
}

func (s *Server) handleNetworks(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 20)

	cov, err := s.Store.Coverage(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	probeCov, err := s.Store.ProbeCoverage(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	reach, err := s.Store.ReachSummary(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	networks, err := s.Store.TopNetworks(r.Context(), limit)
	if err != nil {
		s.serverError(w, err)
		return
	}
	rirs, err := s.Store.ByRIR(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	countries, err := s.Store.ByCountry(r.Context(), limit)
	if err != nil {
		s.serverError(w, err)
		return
	}
	software, err := s.Store.SoftwareStats(r.Context(), limit)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, networksResponse{
		Coverage: cov, Probes: probeCov, Reach: reach,
		Networks: networks, RIRs: rirs, Countries: countries, Software: software,
	})
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "since must be an RFC3339 timestamp")
			return
		}
		since = t
	}
	changes, err := s.Store.RecentChanges(r.Context(), since, intParam(r, "limit", 200))
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, changes)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.RecentRuns(r.Context(), intParam(r, "limit", 20))
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, runs)
}

// maxLimit caps ?limit=. The server is public and unauthenticated, so a caller
// must not be able to ask it to materialise a whole table in one response. The
// frontend never asks for more than 200.
const maxLimit = 1000

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return min(n, maxLimit)
}
