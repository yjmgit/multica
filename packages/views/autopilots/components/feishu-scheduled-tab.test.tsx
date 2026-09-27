import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enAutopilots from "../../locales/en/autopilots.json";
import enSettings from "../../locales/en/settings.json";

const scheduledRef = vi.hoisted(() => ({ current: [] as unknown[] }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[] }) => {
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("scheduled")) return { data: scheduledRef.current, isLoading: false };
    if (key.includes("members")) return { data: [{ user_id: "user-1", role: "owner" }], isLoading: false };
    return { data: [], isLoading: false };
  },
}));
vi.mock("@multica/core/lark", () => ({
  larkScheduledOptions: () => ({ queryKey: ["lark", "scheduled"] }),
  useCancelLarkScheduled: () => ({ mutate: vi.fn(), isPending: false, variables: undefined }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"] }),
  agentListOptions: () => ({ queryKey: ["agents"] }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getAgentName: () => "Helper" }),
}));
vi.mock("@multica/core/auth", () => {
  const state = { user: { id: "user-1" } };
  const useAuthStore = Object.assign(
    (sel?: (s: typeof state) => unknown) => (sel ? sel(state) : state),
    { getState: () => state },
  );
  return { useAuthStore };
});
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { FeishuScheduledTab } from "./feishu-scheduled-tab";

function renderTab() {
  return render(
    <I18nProvider
      locale="en"
      resources={{ en: { common: enCommon, autopilots: enAutopilots, settings: enSettings } }}
    >
      <FeishuScheduledTab wsId="ws-1" />
    </I18nProvider>,
  );
}

describe("FeishuScheduledTab", () => {
  beforeEach(() => {
    scheduledRef.current = [];
  });

  it("explains how to schedule from Feishu when nothing is pending", () => {
    renderTab();
    expect(screen.getByText("Nothing scheduled in Feishu")).toBeInTheDocument();
  });

  it("lists pending reminders with a cancel action for admins", () => {
    scheduledRef.current = [
      {
        id: "s1",
        kind: "message",
        agent_id: "agent-1",
        receive_id_type: "chat_id",
        receive_id: "oc_1",
        text: "Hand in homework",
        mention_open_ids: [],
        fire_at: "2026-09-27T10:00:00Z",
        status: "pending",
      },
    ];
    renderTab();
    expect(screen.getByText("Hand in homework")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
    expect(screen.queryByText("Scheduled")).toBeNull();
  });
});
