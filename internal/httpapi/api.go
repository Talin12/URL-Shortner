// Package httpapi exposes the link service over HTTP: creation, redirect, and
// a small read-only stats endpoint.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Talin12/URL-Shortner/internal/analytics"
	"github.com/Talin12/URL-Shortner/internal/shortcode"
	"github.com/Talin12/URL-Shortner/internal/store"
)

// maxDestinationLen bounds what we accept, so one caller cannot push
// multi-megabyte rows into the table every redirect then has to read.
const maxDestinationLen = 2048

// API holds the handler dependencies.
type API struct {
	store    *store.Store
	recorder analytics.Recorder
	logger   *slog.Logger
	baseURL  string
}

// New builds an API. baseURL prefixes codes in creation responses.
func New(s *store.Store, recorder analytics.Recorder, logger *slog.Logger, baseURL string) *API {
	return &API{
		store:    s,
		recorder: recorder,
		logger:   logger,
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// Routes returns the mux for the service. Literal patterns take precedence
// over the {code} wildcard, so /healthz and /api/* are never read as codes.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /debug/analytics", a.handleAnalyticsStats)
	mux.HandleFunc("GET /readyz", a.handleReady)
	mux.HandleFunc("POST /api/links", a.handleCreate)
	mux.HandleFunc("GET /api/links/{code}", a.handleGetLink)
	mux.HandleFunc("GET /api/links/{code}/stats", a.handleStats)
	mux.HandleFunc("GET /{$}", a.handleRoot)
	mux.HandleFunc("GET /{code}", a.handleRedirect)
	return mux
}

type createRequest struct {
	URL string `json:"url"`
}

type createResponse struct {
	Code        string    `json:"code"`
	ShortURL    string    `json:"short_url"`
	Destination string    `json:"destination"`
	CreatedAt   time.Time `json:"created_at"`
}

func (a *API) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON object with a \"url\" field")
		return
	}

	destination, err := normalizeDestination(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	link, err := a.store.CreateLink(r.Context(), destination, shortcode.Encode)
	if err != nil {
		a.logger.Error("create link", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create link")
		return
	}

	w.Header().Set("Location", a.shortURL(link.Code))
	writeJSON(w, http.StatusCreated, createResponse{
		Code:        link.Code,
		ShortURL:    a.shortURL(link.Code),
		Destination: link.Destination,
		CreatedAt:   link.CreatedAt,
	})
}

// handleRedirect is the hot path. Everything it does before writing the
// response header is on the latency budget; the click event deliberately is
// not, once phase 2 lands.
func (a *API) handleRedirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !shortcode.Valid(code) {
		writeError(w, http.StatusNotFound, "unknown code")
		return
	}

	destination, err := a.store.Destination(r.Context(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown code")
			return
		}
		a.logger.Error("resolve code", "code", code, "err", err)
		writeError(w, http.StatusInternalServerError, "could not resolve link")
		return
	}

	// Without this, an intermediary caches the 302 and later clicks never
	// reach the service, silently losing analytics.
	w.Header().Set("Cache-Control", "no-store, private")
	http.Redirect(w, r, destination, http.StatusFound)

	// Phase 1: this blocks after the response is written but before the
	// handler returns, so it still holds the connection and a pool slot.
	a.recorder.Record(r.Context(), store.ClickEvent{
		Code:       code,
		OccurredAt: time.Now().UTC(),
		UserAgent:  truncate(r.UserAgent(), 512),
		Referrer:   truncate(r.Referer(), 512),
	})
}

func (a *API) handleGetLink(w http.ResponseWriter, r *http.Request) {
	link, err := a.store.Link(r.Context(), r.PathValue("code"))
	if err != nil {
		a.writeLookupError(w, err, "load link")
		return
	}
	writeJSON(w, http.StatusOK, createResponse{
		Code:        link.Code,
		ShortURL:    a.shortURL(link.Code),
		Destination: link.Destination,
		CreatedAt:   link.CreatedAt,
	})
}

type statsResponse struct {
	Code   string `json:"code"`
	Clicks int64  `json:"clicks"`
}

func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if _, err := a.store.Link(r.Context(), code); err != nil {
		a.writeLookupError(w, err, "load link for stats")
		return
	}

	clicks, err := a.store.ClickCount(r.Context(), code)
	if err != nil {
		a.logger.Error("count clicks", "code", code, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read stats")
		return
	}
	writeJSON(w, http.StatusOK, statsResponse{Code: code, Clicks: clicks})
}

// handleAnalyticsStats exposes the recorder counters, most importantly the
// drop tallies. Dropping events under backpressure is only a defensible
// decision while the drops are observable; Prometheus replaces this in phase 3.
func (a *API) handleAnalyticsStats(w http.ResponseWriter, _ *http.Request) {
	stats := a.recorder.Stats()
	writeJSON(w, http.StatusOK, struct {
		analytics.Stats
		DroppedTotal uint64 `json:"dropped_total"`
	}{Stats: stats, DroppedTotal: stats.Dropped()})
}

func (a *API) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady checks the dependency, so a load balancer can pull an instance
// that has lost Postgres without also failing its liveness probe.
func (a *API) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "linkflow",
		"usage":   "POST /api/links {\"url\": \"https://example.com\"} then GET /{code}",
	})
}

func (a *API) writeLookupError(w http.ResponseWriter, err error, op string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown code")
		return
	}
	a.logger.Error(op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func (a *API) shortURL(code string) string { return a.baseURL + "/" + code }

// normalizeDestination validates a user-supplied URL. Only absolute http and
// https targets are accepted: anything else (javascript:, file:, data:) would
// turn the redirect into an open vector against whoever clicks it.
func normalizeDestination(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	if len(raw) > maxDestinationLen {
		return "", errors.New("url is too long")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("url is not parseable")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("url must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("url must include a host")
	}
	return u.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
