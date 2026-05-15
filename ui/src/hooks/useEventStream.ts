// useEventStream subscribes to Squadron's Server-Sent Events stream and
// fires a callback for every domain event the broker pushes. Callers use
// this to invalidate their SWR caches when relevant entities change, so
// the UI doesn't have to poll on a fixed interval.
//
// Lifecycle:
//   - Opens an EventSource on mount, closes it on unmount.
//   - Reconnects automatically (EventSource does this natively).
//   - Filters by event type if `types` is provided; otherwise fires for
//     every event.

import { useEffect, useRef } from "react";

import { apiBaseUrl } from "@/config";

// Event shape mirrors internal/events.Event from the Go side.
export interface SquadronEvent {
  type: string;
  at: string;
  data?: Record<string, unknown>;
}

export interface UseEventStreamOptions {
  /** If set, the handler only fires for events whose type is in this list. */
  types?: readonly string[];
  /** Disable the subscription without unmounting (useful for feature flags). */
  enabled?: boolean;
}

export function useEventStream(
  onEvent: (event: SquadronEvent) => void,
  options: UseEventStreamOptions = {},
): void {
  const { types, enabled = true } = options;
  // Keep the latest callback in a ref so we don't reconnect every time the
  // parent component renders with a new closure.
  const handlerRef = useRef(onEvent);
  handlerRef.current = onEvent;

  useEffect(() => {
    if (!enabled) return;

    const url = `${apiBaseUrl}/events/stream`;
    const source = new EventSource(url);

    const onMessage = (e: MessageEvent) => {
      try {
        const parsed = JSON.parse(e.data) as SquadronEvent;
        if (types && !types.includes(parsed.type)) return;
        handlerRef.current(parsed);
      } catch {
        // Bad payload — ignore.
      }
    };

    // Squadron's SSE handler emits `event: <type>` lines. EventSource lets
    // us subscribe per-type via addEventListener, OR receive every payload
    // via the default message handler. We attach to each type we care
    // about so the browser's type-routing does the filtering for us when
    // possible.
    if (types && types.length > 0) {
      for (const t of types) {
        source.addEventListener(t, onMessage);
      }
    } else {
      // Without an explicit list, listen broadly. EventSource fires
      // typed events on the matching listener, not on 'message', so we
      // attach to the known Squadron types.
      for (const t of KNOWN_TYPES) {
        source.addEventListener(t, onMessage);
      }
    }

    return () => {
      source.close();
    };
    // types is intentionally captured by value via JSON.stringify so callers
    // can pass a fresh array each render without spinning the connection.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, JSON.stringify(types ?? null)]);
}

// KNOWN_TYPES mirrors internal/events.Type in Go. Keep in sync.
const KNOWN_TYPES = [
  "agent_registered",
  "agent_drift_changed",
  "agent_status_changed",
  "alert_fired",
  "alert_resolved",
  "audit_event_recorded",
] as const;
