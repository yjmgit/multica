"use client";

import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Bot, MessageSquare } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { useAuthStore } from "@multica/core/auth";
import { agentListOptions } from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import { larkScheduledOptions, useCancelLarkScheduled } from "@multica/core/lark";
import type { LarkScheduledMessage } from "@multica/core/types";
import { useLocale, useT } from "../../i18n";

// LarkScheduledSection lists the Feishu messages and one-off agent wake-ups
// that agents scheduled (`multica lark send --in/--at`, `multica lark wakeup
// --in/--at`) and lets the people who may manage the agent cancel them.
// Recurring wake-ups are autopilots and are managed on the Autopilots page.
// Renders nothing when nothing is pending.
export function LarkScheduledSection({
  wsId,
  canManageWorkspace,
  hideHeader = false,
}: {
  wsId: string;
  canManageWorkspace: boolean;
  /** Omit the heading and hint where the surrounding tab already names
   * the list (the Autopilots page). */
  hideHeader?: boolean;
}) {
  const { t } = useT("settings");
  const { data: scheduled = [] } = useQuery(larkScheduledOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const userId = useAuthStore((s) => s.user?.id);
  const cancel = useCancelLarkScheduled(wsId);

  if (scheduled.length === 0) return null;

  // The server lets the agent's owner or a workspace owner/admin cancel;
  // mirror that so nobody is offered a button that returns 403.
  const canCancel = (agentId: string) =>
    canManageWorkspace || (!!userId && agents.some((a) => a.id === agentId && a.owner_id === userId));

  const handleCancel = (id: string) => {
    cancel.mutate(id, {
      onSuccess: () => toast.success(t(($) => $.lark.toast_scheduled_cancelled)),
      onError: (e) =>
        toast.error(e instanceof Error ? e.message : t(($) => $.lark.toast_scheduled_cancel_failed)),
    });
  };

  return (
    <section className="space-y-3">
      {!hideHeader && (
        <div className="space-y-1">
          <h2 className="text-body font-semibold">{t(($) => $.lark.scheduled_title)}</h2>
          <p className="text-caption text-muted-foreground">{t(($) => $.lark.scheduled_recurring_hint)}</p>
        </div>
      )}
      <Card>
        <CardContent className="divide-y">
          {scheduled.map((item) => (
            <ScheduledRow
              key={item.id}
              item={item}
              canCancel={canCancel(item.agent_id)}
              cancelling={cancel.isPending && cancel.variables === item.id}
              onCancel={() => handleCancel(item.id)}
            />
          ))}
        </CardContent>
      </Card>
    </section>
  );
}

function ScheduledRow({
  item,
  canCancel,
  cancelling,
  onCancel,
}: {
  item: LarkScheduledMessage;
  canCancel: boolean;
  cancelling: boolean;
  onCancel: () => void;
}) {
  const { t } = useT("settings");
  const locale = useLocale();
  const { getAgentName } = useActorName();
  const isRun = item.kind === "agent_run";
  const Icon = isRun ? Bot : MessageSquare;
  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="flex min-w-0 items-start gap-3">
        <Icon className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        <div className="min-w-0 space-y-1">
          <p className="text-body break-words">{item.text}</p>
          <p className="text-micro text-muted-foreground">
            {[
              new Date(item.fire_at).toLocaleString(locale),
              getAgentName(item.agent_id),
              isRun ? t(($) => $.lark.scheduled_kind_agent_run) : t(($) => $.lark.scheduled_kind_message),
              item.receive_id_type === "open_id"
                ? t(($) => $.lark.scheduled_target_user)
                : t(($) => $.lark.scheduled_target_chat),
            ].join(" · ")}
          </p>
        </div>
      </div>
      {canCancel && (
        <Button variant="outline" size="sm" onClick={onCancel} disabled={cancelling}>
          {cancelling ? t(($) => $.lark.scheduled_cancelling) : t(($) => $.lark.scheduled_cancel)}
        </Button>
      )}
    </div>
  );
}
