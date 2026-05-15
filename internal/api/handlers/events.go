// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/events"
)

// EventsHandlers serves the Server-Sent Events stream that pushes live
// domain events (agent registered, alert fired, drift transition, ...) to
// connected browsers so the UI doesn't have to poll.
type EventsHandlers struct {
	broker *events.Broker
	logger *zap.Logger
}

// NewEventsHandlers wires up the SSE handler set.
func NewEventsHandlers(broker *events.Broker, logger *zap.Logger) *EventsHandlers {
	return &EventsHandlers{broker: broker, logger: logger}
}

// HandleStream serves GET /api/v1/events/stream as text/event-stream.
//
// Wire format is the standard SSE flavor:
//
//	event: <type>
//	data: {...json...}
//	\n
//
// We also send periodic ":heartbeat\n\n" comment lines so reverse proxies
// don't time out the connection during long quiet periods.
func (h *EventsHandlers) HandleStream(c *gin.Context) {
	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	// Standard SSE headers. CORS is already handled by the global middleware,
	// but we explicitly disable nginx-style buffering to make sure events
	// reach the client promptly.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Buffer 256 events per subscriber. Plenty for any realistic UI; if we
	// drop, the SSE handler keeps going and the client just misses an event
	// — they'll re-fetch on next user interaction anyway.
	sub := h.broker.Subscribe(256)
	defer sub.Close()

	// Initial comment so the client knows the stream is live.
	if _, err := fmt.Fprint(w, ":connected\n\n"); err != nil {
		h.logger.Debug("SSE: client closed before initial comment", zap.Error(err))
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := c.Request.Context()

	for {
		select {
		case <-ctx.Done():
			h.logger.Debug("SSE: client disconnected")
			return

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ":heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				h.logger.Warn("SSE: failed to marshal event", zap.Error(err))
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
