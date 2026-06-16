// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package siemwire wires services.SiemDispatcher (the slim interface
// the audit service depends on) to siem.Dispatcher (the bounded-queue
// fan-out engine). Lives in its own package because:
//   - services/ defines the interface but can't import siem/ without
//     pulling the dispatcher into the services dependency graph
//   - siem/ can't import services/ without an import cycle since
//     services.SiemService implements siem.SourceProvider
//
// Splitting the adapter into a third package breaks both knots
// cleanly. main.go is the only consumer.
package siemwire

import (
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/siem"
)

// DispatcherAdapter satisfies services.SiemDispatcher by delegating
// to a *siem.Dispatcher. Construct after both the audit service and
// the dispatcher exist:
//
//	disp := siem.NewDispatcher(siemSvc, 60*time.Second, logger)
//	disp.Start(ctx)
//	auditSvc.SetSiemDispatcher(&siemwire.DispatcherAdapter{Dispatcher: disp})
type DispatcherAdapter struct {
	Dispatcher *siem.Dispatcher
}

// Dispatch translates a service-layer SiemEvent into the wire-shape
// siem.Event. The two types are kept distinct on purpose so each
// layer can evolve without breaking the other.
func (a *DispatcherAdapter) Dispatch(ev services.SiemEvent) {
	if a == nil || a.Dispatcher == nil {
		return
	}
	a.Dispatcher.Dispatch(siem.Event{
		ID:         ev.ID,
		Timestamp:  ev.Timestamp,
		Actor:      ev.Actor,
		EventType:  ev.EventType,
		TargetType: ev.TargetType,
		TargetID:   ev.TargetID,
		Action:     ev.Action,
		Payload:    ev.Payload,
		Source:     "squadron",
	})
}
