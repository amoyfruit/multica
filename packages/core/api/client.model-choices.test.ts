import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { modelChoices } from "../runtimes/model-choices";

// Exercise the real owned HTTP boundary. Inference is delegated to external
// agent CLIs; this test does not simulate or claim to validate their provider API.
afterEach(() => vi.unstubAllGlobals());

describe("model choices at the agent settings HTTP boundary", () => {
  it("serializes the first advertised Flash choice exactly when creating an agent", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const choices = modelChoices([
      { id: "minimax/minimax-m2.7", label: "Legacy" },
      { id: "minimax/minimax-m3", label: "M3" },
      { id: "minimax/minimax-m3.1-flash:free", label: "Flash" },
    ], "");
    await new ApiClient("https://api.example.test").createAgent({
      name: "Test agent", runtime_id: "runtime-1", model: choices[0]!.id,
    });
    const [url, request] = fetchMock.mock.calls[0]!;
    expect(url).toBe("https://api.example.test/api/agents");
    expect(request.method).toBe("POST");
    expect(JSON.parse(request.body)).toEqual({
      name: "Test agent", runtime_id: "runtime-1", model: "minimax/minimax-m3.1-flash:free",
    });
  });

  it.each([
    "", "MiniMax-M3.1-Flash", "MiniMax-M3", "minimax/minimax-m2.7",
    "opencode/minimax-m3.1-flash-free", "private-custom-model",
  ])("preserves an explicit model %j and runtime-native settings", async (model) => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const update = { model, thinking_level: "high", custom_args: ["--custom-option"] };
    await new ApiClient("https://api.example.test").updateAgent("agent-1", update);
    const [url, request] = fetchMock.mock.calls[0]!;
    expect(url).toBe("https://api.example.test/api/agents/agent-1");
    expect(request.method).toBe("PUT");
    expect(JSON.parse(request.body)).toEqual(update);
  });
});
