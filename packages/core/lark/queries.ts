import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query key namespace for everything Lark-installation-related. Realtime
 * sync invalidates `installations(wsId)` on `lark_installation:*` events
 * so the Settings panel updates without a refetch. */
export const larkKeys = {
  all: (wsId: string) => ["lark", wsId] as const,
  installations: (wsId: string) => [...larkKeys.all(wsId), "installations"] as const,
  scheduled: (wsId: string) => [...larkKeys.all(wsId), "scheduled"] as const,
};

export const larkInstallationsOptions = (wsId: string) =>
  queryOptions({
    queryKey: larkKeys.installations(wsId),
    queryFn: () => api.listLarkInstallations(wsId),
    enabled: !!wsId,
  });

/** Pending Feishu messages and one-off wake-ups agents scheduled. There is
 * no realtime event for them, so the list also polls: rows the scheduler
 * has sent drop off within a minute instead of waiting for a focus change. */
export const larkScheduledOptions = (wsId: string) =>
  queryOptions({
    queryKey: larkKeys.scheduled(wsId),
    queryFn: () => api.listLarkScheduled(wsId),
    enabled: !!wsId,
    refetchInterval: 60_000,
  });
