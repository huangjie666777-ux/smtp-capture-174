package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

type HTTPServer struct {
	addr    string
	store   *Store
	cfg     Config
	limiter *ListenerLimiter
}

func NewHTTPServer(cfg Config, store *Store, limiter *ListenerLimiter) *HTTPServer {
	return &HTTPServer{addr: cfg.HTTPAddr, store: store, cfg: cfg, limiter: limiter}
}

func (h *HTTPServer) ListenAndServe() error {
	r := chi.NewRouter()
	r.Get("/messages", h.listMessages)
	r.Get("/messages/{id}", h.getMessage)
	r.Get("/messages/{id}/raw", h.downloadMessage)

	server := &http.Server{
		Addr:              h.addr,
		Handler:           r,
		ReadTimeout:       time.Duration(h.cfg.ReadDeadlineSec) * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    h.cfg.MaxLineBytes,
	}
	inner, err := net.Listen("tcp", h.addr)
	if err != nil {
		return err
	}
	reject := []byte("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 20\r\nConnection: close\r\n\r\ntoo many connections")
	listener := newLimitedListener(inner, h.limiter, reject)
	return server.Serve(listener)
}

func (h *HTTPServer) listMessages(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	if len(values) != 1 || values.Get("recipient") == "" || values["recipient"] == nil || len(values["recipient"]) != 1 {
		http.Error(w, "exactly one recipient query parameter is required", http.StatusBadRequest)
		return
	}
	recipient, err := normalizeRecipient(values.Get("recipient"))
	if err != nil {
		http.Error(w, "invalid recipient", http.StatusBadRequest)
		return
	}
	if _, ok := h.cfg.AllowedMailbox[recipient]; !ok {
		http.NotFound(w, r)
		return
	}
	items, err := h.store.ListMessages(recipient)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []MessageSummary{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *HTTPServer) getMessage(w http.ResponseWriter, r *http.Request) {
	message, ok := h.loadMessage(w, r)
	if !ok {
		return
	}
	response := struct {
		ID         string   `json:"id"`
		ReceivedAt string   `json:"received_at"`
		Sender     string   `json:"sender"`
		Recipients []string `json:"recipients"`
		Size       int      `json:"size"`
	}{
		ID:         message.ID,
		ReceivedAt: message.ReceivedAt.Format(time.RFC3339Nano),
		Sender:     message.Sender,
		Recipients: message.Recipients,
		Size:       len(message.Raw),
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *HTTPServer) downloadMessage(w http.ResponseWriter, r *http.Request) {
	message, ok := h.loadMessage(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", `attachment; filename="`+message.ID+`.eml"`)
	w.WriteHeader(http.StatusOK)
	w.Write(message.Raw)
}

func (h *HTTPServer) loadMessage(w http.ResponseWriter, r *http.Request) (*Message, bool) {
	id := chi.URLParam(r, "id")
	message, err := h.store.GetMessage(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return nil, false
	}
	return message, true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
