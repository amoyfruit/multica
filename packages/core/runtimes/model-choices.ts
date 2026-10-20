import type { RuntimeModel } from "../types";

// Discovery remains authoritative for execution and capability checks. This
// projection only controls ordinary picker choices, never saved model IDs or
// manual entry, and never invents support absent from the runtime's catalog.
export function modelChoices(
  models: RuntimeModel[],
  selected: string,
): RuntimeModel[] {
  const textID = (model: RuntimeModel) =>
    model.id.toLowerCase().match(
      /(?:^|[/:])(minimax-(?:m\d[^/]*|text-[^/]*)|abab\d[^/]*)$/,
    )?.[1];
  const rank = (id: string) => {
    if (
      id === "minimax-m3.1-flash" ||
      id === "minimax-m3.1-flash:free" ||
      id === "minimax-m3.1-flash-free"
    ) return 0;
    if (id === "minimax-m3") return 1;
    return 2;
  };
  const visible = models.filter((model) => {
    const id = textID(model);
    return !id || rank(id) < 2 || model.id === selected;
  });
  const textModels = visible
    .filter((model) => textID(model))
    .sort((a, b) => rank(textID(a)!) - rank(textID(b)!));
  let index = 0;
  // Retain the order and metadata of every unrelated entry. Among advertised
  // MiniMax text choices, Flash comes first, then M3, then an explicit old pick.
  return visible.map((model) => textID(model) ? textModels[index++]! : model);
}
