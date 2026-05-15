// EventSubscriber maps Squadron's domain events from the SSE stream to SWR
// cache invalidations. Mounted once at the App root so every page benefits;
// individual pages don't have to remember to subscribe.
//
// Each event type → list of SWR cache keys that should re-fetch.

import { useCallback } from "react";
import { mutate } from "swr";

import { useEventStream, type SquadronEvent } from "@/hooks/useEventStream";

// Mapping from event type to the SWR keys we want to revalidate. Keep
// these in sync with the keys used in the pages that own the data.
const EVENT_INVALIDATIONS: Record<string, readonly string[]> = {
  agent_registered: ["agents", "command-palette/agents"],
  agent_drift_changed: ["agents", "command-palette/agents"],
  agent_status_changed: ["agents", "command-palette/agents"],
  alert_fired: ["/api/v1/alerts/rules", "command-palette/alert-rules"],
  alert_resolved: ["/api/v1/alerts/rules", "command-palette/alert-rules"],
};

export function EventSubscriber() {
  const onEvent = useCallback((event: SquadronEvent) => {
    const keys = EVENT_INVALIDATIONS[event.type];
    if (!keys) return;
    for (const key of keys) {
      void mutate(key);
    }
  }, []);

  useEventStream(onEvent);
  return null;
}
