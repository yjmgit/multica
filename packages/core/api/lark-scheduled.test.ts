// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

const row = {
  id: "s1",
  kind: "message",
  agent_id: "a1",
  receive_id_type: "chat_id",
  receive_id: "oc_1",
  text: "关煤气",
  mention_open_ids: ["ou_1"],
  fire_at: "2026-09-27T10:00:00Z",
  status: "pending",
};

afterEach(() => vi.unstubAllGlobals());

async function read(body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200 })),
  );
  return new ApiClient("https://api.example.test").listLarkScheduled("ws-1");
}

describe("listLarkScheduled", () => {
  it("returns scheduled rows", async () => {
    expect(await read({ scheduled: [row] })).toEqual([row]);
  });

  it("defaults optional display fields from an older or partial server", async () => {
    const [parsed] = await read({ scheduled: [{ id: "s2", fire_at: "2026-09-27T10:00:00Z" }] });
    expect(parsed).toMatchObject({ id: "s2", kind: "message", text: "", mention_open_ids: [], status: "pending" });
  });

  it.each([null, {}, { scheduled: null }, { scheduled: [{ ...row, id: 1 }] }, { scheduled: [{ ...row, fire_at: undefined }] }])(
    "falls back to an empty list for a malformed response: %j",
    async (body) => {
      expect(await read(body)).toEqual([]);
    },
  );
});
