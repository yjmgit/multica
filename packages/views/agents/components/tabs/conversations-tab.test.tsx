// @vitest-environment jsdom

import { describe, it, expect, vi, afterEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Agent, ChatMessage, ChatSession, MemberWithUser } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../../locales/en/common.json";
import enAgents from "../../../locales/en/agents.json";

const apiMock = vi.hoisted(() => ({
  listAgentConversations: vi.fn(),
  listAgentConversationMessages: vi.fn(),
}));

vi.mock("@multica/core/api", () => ({ api: apiMock }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("../../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("../../../chat/components/chat-message-list", () => ({
  ChatMessageList: ({ messages }: { messages: ChatMessage[] }) => (
    <ol data-testid="transcript">
      {messages.map((m) => (
        <li key={m.id}>{m.content}</li>
      ))}
    </ol>
  ),
  ChatMessageSkeleton: () => <div>loading</div>,
}));

import { ConversationsTab } from "./conversations-tab";

const agent = { id: "agent-1" } as Agent;
const members = [
  { user_id: "user-2", name: "Wang Wei", avatar_url: null },
] as MemberWithUser[];

function session(id: string, title: string, extra: Partial<ChatSession> = {}): ChatSession {
  return {
    id,
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-2",
    title,
    status: "active",
    has_unread: false,
    created_at: "2026-06-01T00:00:00Z",
    updated_at: "2026-06-01T00:00:00Z",
    ...extra,
  };
}

function message(id: string, content: string): ChatMessage {
  return { id, chat_session_id: "s", role: "user", content, task_id: null, created_at: "2026-06-01T00:00:00Z" } as ChatMessage;
}

function renderTab() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { common: enCommon, agents: enAgents } }}>
        <ConversationsTab agent={agent} members={members} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ConversationsTab", () => {
  it("lists other members' Chats and opens the selected transcript", async () => {
    apiMock.listAgentConversations.mockResolvedValue([
      session("s1", "Tenant count", {
        channel_source: { channel_type: "lark", installation_id: "i1", route_revision: 1 },
      }),
      session("s2", "Deploy plan"),
    ]);
    apiMock.listAgentConversationMessages.mockImplementation(async (_agentId: string, sessionId: string) =>
      sessionId === "s1" ? [message("m1", "how many tenants?")] : [message("m2", "ship it")],
    );

    renderTab();

    expect(await screen.findByText("Tenant count")).toBeTruthy();
    expect(screen.getAllByText("Wang Wei")).toHaveLength(2);
    expect(screen.getByText("Feishu")).toBeTruthy();
    expect(await screen.findByText("how many tenants?")).toBeTruthy();

    await userEvent.click(screen.getByText("Deploy plan"));
    expect(await screen.findByText("ship it")).toBeTruthy();
    expect(apiMock.listAgentConversationMessages).toHaveBeenLastCalledWith("agent-1", "s2");
  });

  it("says so when no one has chatted with the agent", async () => {
    apiMock.listAgentConversations.mockResolvedValue([]);
    renderTab();
    expect(await screen.findByText(enAgents.conversations.empty)).toBeTruthy();
  });
});
