// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package siemwire wires extension/siem.Dispatcher (the public
// boundary the audit service depends on) to internal/siem.Dispatcher
// (the bounded-queue fan-out engine). Lives in its own package
// because:
//   - extension/siem defines the interface but can't import
//     internal/siem from outside its module
//   - internal/siem can't import extension/siem without inviting an
//     import cycle through services/
//
// Splitting the adapter into a third package breaks both knots
// cleanly. The Enterprise wire file in cmd/all-in-one/ is the
// usual consumer.
package siemwire

import (
	extsiem "github.com/devopsmike2/squadron/extension/siem"
	"github.com/devopsmike2/squadron/internal/siem"
)

// DispatcherAdapter satisfies extension/siem.Dispatcher by
// delegating to a *internal/siem.Dispatcher. Construct after both
// the audit service and the dispatcher exist:
//
//	disp := siem.NewDispatcher(siemSvc, 60*time.Second, logger)
//	disp.Start(ctx)
//	auditSvc.SetSiemDispatcher(&siemwire.DispatcherAdapter{Dispatcher: disp})
type DispatcherAdapter struct {
	Dispatcher *siem.Dispatcher
}

// Dispatch translates the public-boundary extension/siem.Event into
// the internal siem.Event wire shape. The two are kept distinct on
// purpose so each layer can evolve without breaking the other.
func (a *DispatcherAdapter) Dispatch(ev extsiem.Event) {
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
