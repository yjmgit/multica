import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { larkKeys } from "./queries";

/** Cancels a pending scheduled message. Awaits the server: a message that
 * was already sent cannot be taken back, so the row only disappears once
 * the cancel is confirmed. */
export function useCancelLarkScheduled(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (scheduledId: string) => api.cancelLarkScheduled(wsId, scheduledId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: larkKeys.scheduled(wsId) });
    },
  });
}
