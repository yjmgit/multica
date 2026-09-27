import type { ReactNode } from "react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const scheduledRef = vi.hoisted(() => ({ current: [] as unknown[] }));
const agentsRef = vi.hoisted(() => ({ current: [] as unknown[] }));
const mockMutate = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[] }) => {
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("scheduled")) return { data: scheduledRef.current };
    if (key.includes("agents")) return { data: agentsRef.current };
    return { data: undefined };
  },
}));

vi.mock("@multica/core/lark", () => ({
  larkScheduledOptions: () => ({ queryKey: ["lark", "scheduled"] }),
  useCancelLarkScheduled: () => ({ mutate: mockMutate, isPending: false, variables: undefined }),
}));

vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getAgentName: (id: string) => (id === "agent-1" ? "Helper" : "Other") }),
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

import { LarkScheduledSection } from "./lark-scheduled-section";

function wrap(children: ReactNode) {
  return (
    <I18nProvider locale="en" resources={{ en: { common: enCommon, settings: enSettings } }}>
      {children}
    </I18nProvider>
  );
}

const message = {
  id: "s1",
  kind: "message",
  agent_id: "agent-1",
  receive_id_type: "chat_id",
  receive_id: "oc_1",
  text: "Turn off the gas",
  mention_open_ids: [],
  fire_at: "2026-09-27T10:00:00Z",
  status: "pending",
};

describe("LarkScheduledSection", () => {
  beforeEach(() => {
    scheduledRef.current = [];
    agentsRef.current = [];
    mockMutate.mockReset();
  });

  it("renders nothing when nothing is scheduled", () => {
    const { container } = render(wrap(<LarkScheduledSection wsId="ws-1" canManageWorkspace />));
    expect(container).toBeEmptyDOMElement();
  });

  it("lists scheduled items and lets an admin cancel them", async () => {
    scheduledRef.current = [message, { ...message, id: "s2", kind: "agent_run", text: "Daily summary", receive_id_type: "open_id" }];
    render(wrap(<LarkScheduledSection wsId="ws-1" canManageWorkspace />));
    expect(screen.getByText("Scheduled")).toBeInTheDocument();
    expect(screen.getByText("Turn off the gas")).toBeInTheDocument();
    expect(screen.getByText(/Helper · Agent run · Direct message/)).toBeInTheDocument();
    await userEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
    expect(mockMutate).toHaveBeenCalledWith("s1", expect.anything());
  });

  it("offers cancel to the agent's owner but not to other members", () => {
    scheduledRef.current = [message, { ...message, id: "s2", agent_id: "agent-2", text: "Someone else's" }];
    agentsRef.current = [{ id: "agent-1", owner_id: "user-1" }, { id: "agent-2", owner_id: "user-2" }];
    render(wrap(<LarkScheduledSection wsId="ws-1" canManageWorkspace={false} />));
    expect(screen.getAllByRole("button", { name: "Cancel" })).toHaveLength(1);
  });
});
