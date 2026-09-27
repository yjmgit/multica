"use client";

import { useQuery } from "@tanstack/react-query";
import { AlarmClock } from "lucide-react";
import { useAuthStore } from "@multica/core/auth";
import { memberListOptions } from "@multica/core/workspace/queries";
import { larkScheduledOptions } from "@multica/core/lark";
import { CollectionPageState } from "../../layout/collection-page";
import { LarkScheduledSection } from "../../settings/components/lark-scheduled-section";
import { useT } from "../../i18n";

// FeishuScheduledTab is the Autopilots page's view of the Feishu reminders
// and one-off wake-ups agents scheduled from Feishu — where people look for
// "things that will run later", rather than only under Settings > Feishu.
export function FeishuScheduledTab({ wsId }: { wsId: string }) {
  const { t } = useT("autopilots");
  const userId = useAuthStore((s) => s.user?.id);
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: scheduled = [], isLoading } = useQuery(larkScheduledOptions(wsId));
  const role = members.find((m) => m.user_id === userId)?.role;
  const canManageWorkspace = role === "owner" || role === "admin";

  if (!isLoading && scheduled.length === 0) {
    return (
      <CollectionPageState
        icon={AlarmClock}
        title={t(($) => $.feishu_scheduled.empty_title)}
        description={t(($) => $.feishu_scheduled.empty_description)}
      />
    );
  }
  return (
    <div className="overflow-y-auto p-4">
      <LarkScheduledSection wsId={wsId} canManageWorkspace={canManageWorkspace} hideHeader />
    </div>
  );
}
