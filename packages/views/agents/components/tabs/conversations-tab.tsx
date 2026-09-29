"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  agentConversationMessagesOptions,
  agentConversationsOptions,
} from "@multica/core/chat/queries";
import type { Agent, ChatSession, MemberWithUser } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { ActorAvatar } from "../../../common/actor-avatar";
import { ChatMessageList, ChatMessageSkeleton } from "../../../chat/components/chat-message-list";
import { useLocale, useT } from "../../../i18n";

function formatTime(dateStr: string, locale: string): string {
  const d = new Date(dateStr);
  if (d.toDateString() === new Date().toDateString()) {
    return d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString(locale, { month: "numeric", day: "numeric" });
}

function preview(content: string): string {
  return content.replace(/```[\s\S]*?```/g, " ").replace(/[#*`>~]/g, "").replace(/\s+/g, " ").trim();
}

/**
 * Read-only history of every Chat anyone in the workspace has had with this
 * agent, including conversations that arrived through Feishu or another
 * channel. Replying stays in the Chat owner's own Chat page.
 */
export function ConversationsTab({
  agent,
  members,
}: {
  agent: Agent;
  members: MemberWithUser[];
}) {
  const { t } = useT("agents");
  const locale = useLocale();
  const wsId = useWorkspaceId();
  const { data: sessions = [], isLoading } = useQuery(agentConversationsOptions(wsId, agent.id));
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const active = sessions.find((s) => s.id === selectedId) ?? sessions[0] ?? null;
  const { data: messages, isLoading: messagesLoading } = useQuery(
    agentConversationMessagesOptions(wsId, agent.id, active?.id ?? ""),
  );
  const membersById = useMemo(
    () => new Map(members.map((m) => [m.user_id, m])),
    [members],
  );

  if (isLoading) {
    return <div className="p-6 text-body text-muted-foreground">{t(($) => $.conversations.loading)}</div>;
  }
  if (sessions.length === 0) {
    return <div className="p-6 text-body text-muted-foreground">{t(($) => $.conversations.empty)}</div>;
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col md:flex-row">
      <aside
        className="max-h-60 shrink-0 overflow-y-auto border-b border-surface-border md:max-h-none md:w-80 md:border-b-0 md:border-r"
        aria-label={t(($) => $.conversations.list_aria)}
      >
        <ul className="flex flex-col gap-0.5 p-2">
          {sessions.map((session) => (
            <ConversationRow
              key={session.id}
              session={session}
              creator={membersById.get(session.creator_id) ?? null}
              active={session.id === active?.id}
              locale={locale}
              onSelect={() => setSelectedId(session.id)}
            />
          ))}
        </ul>
      </aside>
      <section className="flex min-h-[420px] min-w-0 flex-1 flex-col">
        {messagesLoading || !messages ? (
          <ChatMessageSkeleton />
        ) : messages.length === 0 ? (
          <div className="p-6 text-body text-muted-foreground">{t(($) => $.conversations.no_messages)}</div>
        ) : (
          <ChatMessageList
            key={active?.id}
            messages={messages}
            pendingTask={null}
            availability={undefined}
          />
        )}
      </section>
    </div>
  );
}

function ConversationRow({
  session,
  creator,
  active,
  locale,
  onSelect,
}: {
  session: ChatSession;
  creator: MemberWithUser | null;
  active: boolean;
  locale: string;
  onSelect: () => void;
}) {
  const { t } = useT("agents");
  const channel = session.channel_source?.channel_type;
  const channelLabel =
    channel === "lark" ? t(($) => $.conversations.channel_lark) : channel;
  const creatorName = creator?.name ?? t(($) => $.conversations.unknown_member);
  const at = session.last_message?.created_at ?? session.updated_at;
  return (
    <li>
      <button
        type="button"
        onClick={onSelect}
        aria-current={active ? "true" : undefined}
        className={cn(
          "flex w-full flex-col gap-1 rounded-md px-2.5 py-2 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          active
            ? "bg-surface-selected text-surface-selected-foreground hover:bg-surface-selected"
            : "hover:bg-surface-hover",
        )}
      >
        <div className="flex min-w-0 items-center gap-2">
          <span className="min-w-0 flex-1 truncate text-body font-medium">
            {session.title || t(($) => $.conversations.untitled)}
          </span>
          <span className="shrink-0 text-caption text-muted-foreground">{formatTime(at, locale)}</span>
        </div>
        <div className="flex min-w-0 items-center gap-1.5 text-caption text-muted-foreground">
          <ActorAvatar
            actorType="member"
            actorId={session.creator_id}
            name={creatorName}
            avatarUrl={creator?.avatar_url ?? null}
            size="xs"
          />
          <span className="truncate">{creatorName}</span>
          {channelLabel && (
            <span className="shrink-0 rounded border border-surface-border px-1">{channelLabel}</span>
          )}
        </div>
        {session.last_message?.content && (
          <p className="line-clamp-1 text-caption text-muted-foreground">
            {preview(session.last_message.content)}
          </p>
        )}
      </button>
    </li>
  );
}
