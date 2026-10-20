import { describe, expect, it } from "vitest";
import type { RuntimeModel } from "../types";
import { modelChoices } from "./model-choices";

const models = (...ids: string[]): RuntimeModel[] =>
  ids.map((id) => ({ id, label: id }));
const ids = (choices: RuntimeModel[]) => choices.map((model) => model.id);

describe("modelChoices", () => {
  it("shows advertised Flash first, then M3, while retaining unrelated choices", () => {
    const catalog = models(
      "other-model", "MiniMax-M2.7", "MiniMax-M3", "MiniMax-M3.1-Flash",
      "MiniMax-Text-01", "abab6.5s-chat", "MiniMax-M3-Preview", "last-model",
    );
    expect(ids(modelChoices(catalog, ""))).toEqual([
      "other-model", "MiniMax-M3.1-Flash", "MiniMax-M3", "last-model",
    ]);
    expect(catalog).toHaveLength(8);
  });

  it("retains exact gateway IDs without inventing routes", () => {
    const catalog = models(
      "minimax/minimax-m3", "minimax/minimax-m2.7",
      "minimax/minimax-m3.1-flash:free", "opencode/minimax-m3.1-flash-free",
    );
    expect(ids(modelChoices(catalog, ""))).toEqual([
      "minimax/minimax-m3.1-flash:free", "opencode/minimax-m3.1-flash-free",
      "minimax/minimax-m3",
    ]);
    expect(ids(modelChoices(models("codebuddy/minimax-m3"), ""))).toEqual([
      "codebuddy/minimax-m3",
    ]);
    expect(modelChoices([], "")).toEqual([]);
  });

  it("retains an explicit saved legacy model and its capability metadata", () => {
    const legacy: RuntimeModel = {
      id: "minimax/minimax-m2.7",
      label: "Saved model",
      thinking: { supported_levels: [{ value: "high", label: "High" }] },
    };
    const catalog = [legacy, ...models("MiniMax-M3", "MiniMax-M3.1-Flash")];
    const choices = modelChoices(catalog, legacy.id);
    expect(ids(choices)).toEqual([
      "MiniMax-M3.1-Flash", "MiniMax-M3", legacy.id,
    ]);
    expect(choices[2]).toBe(legacy);
    expect(catalog[0]).toBe(legacy);
  });

  it("preserves media and custom choices and never synthesizes missing selections", () => {
    const catalog = models(
      "minimax/speech-02-hd", "minimax/image-01", "minimax/video-01",
      "my-custom-model", "other-provider/other-model",
    );
    expect(modelChoices(catalog, "my-custom-model")).toEqual(catalog);
    expect(modelChoices(catalog, "saved-private-model")).toEqual(catalog);
  });
});
