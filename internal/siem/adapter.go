// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package siem

import (
	"github.com/devopsmike2/squadron/internal/services"
)

// DispatcherAdapter satisfies services.SiemDispatcher (defined in
// internal/services/audit_service_impl.go) by delegating to a
// *Dispatcher. services imports nothing from siem, so importing
// services here does not create a cycle.
//
// Wire it in main.go after both the audit service and the dispatcher
// are constructed:
//
//	disp := siem.NewDispatcher(source, 60*time.Second, logger)
//	disp.Start(ctx)
//	auditSvc.SetSiemDispatcher(&siem.DispatcherAdapter{Dispatcher: disp})
type DispatcherAdapter struct {
	Dispatcher *Dispatcher
}

// Dispatch translates a service-layer SiemEvent into the wire-shape
// Event and hands it to the Dispatcher's bounded queues.
func (a *DispatcherAdapter) Dispatch(ev services.SiemEvent) {
	if a == nil || a.Dispatcher == nil {
		return
	}
	a.Dispatcher.Dispatch(Event{
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
