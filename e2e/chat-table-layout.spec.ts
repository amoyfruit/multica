import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

// CSS geometry regression: no API/server needed. The fixture mirrors the
// readonly RichContent table > IssueMentionCard > IssueChip DOM with the Tailwind
// utilities the chip uses replaced by equivalent plain CSS. The table rules under
// test come from the production stylesheets.
const prose = readFileSync("packages/views/editor/styles/prose.css", "utf8");
const richContent = readFileSync("packages/views/rich-content/rich-content.css", "utf8");
const title = "A very long issue title with an ExtremelyLongUnbrokenIdentifier1234567890";

const chip = (id: number) => `<a class="issue-mention align-middle" href="#PX-${id}"><span
  class="issue-mention chip"><svg width="14" height="14" aria-hidden="true"><circle cx="7" cy="7" r="5" fill="none" stroke="currentColor" /></svg>
  <span class="identifier">PX-${id}</span><span class="truncate">${title}</span>
</span></a>`;

// Only the chip's utility styles are supplied by the fixture. The chip's max width
// is the same percentage cap the component uses (`max-w-[min(18rem,100%)]`).
const utilities = `
  * { box-sizing: border-box; }
  body { font: 14px Arial; margin: 16px; --text-body: 14px; --border: #ddd; --radius: 6px; }
  a { color: inherit; text-decoration: none; }
  .chip { display: inline-flex; min-width: 0; max-width: min(18rem,100%);
    align-items: center; gap: 6px; border: 1px solid #ddd; margin: 0 2px;
    padding: 2px 8px; font-size: 12px; border-radius: 6px; }
  .identifier, svg { flex-shrink: 0; }
  .truncate { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
`;

for (const width of [360, 640, 960]) {
  test(`chat table keeps every column and every identifier inside its cell at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width: width + 32, height: 800 });
    const rows = Array.from({ length: 6 }, (_, row) => `<tr><td>Agent ${row + 1}</td>${
      Array.from({ length: 3 }, (_, column) => `<td>${
        [0, 1, 2].map((n) => chip(613 + column * 3 + n)).join(" ")
      }</td>`).join("")
    }</tr>`).join("");
    await page.setContent(`<style>${utilities}${prose}${richContent}</style>
      <main style="width:${width}px" data-rich-content class="rich-text-editor rich-content-compact">
        <div class="tableWrapper"><table>
          <thead><tr><th>Agent</th><th>Done</th><th>In progress</th><th>Backlog</th></tr></thead>
          <tbody>${rows}</tbody>
        </table></div>
      </main>`);

    // Every column after the first is at least 180px, so narrower messages scroll
    // inside the table wrapper rather than squeezing a chip past its cell.
    const wrapper = page.locator(".tableWrapper");
    const cellRects = await page.locator("tbody tr").first().locator("td").evaluateAll((cells) =>
      cells.map((cell) => {
        const box = cell.getBoundingClientRect();
        return { left: box.left, right: box.right };
      }),
    );
    for (const cell of cellRects.slice(1)) {
      expect(cell.right - cell.left).toBeGreaterThanOrEqual(179);
    }

    // Four columns fit a message column of 640px or wider without a horizontal scrollbar.
    if (width >= 640) {
      expect(await wrapper.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    }

    // Each chip stays inside its own cell, and its identifier is not clipped.
    const spills = await page.locator("tbody td .issue-mention.chip").evaluateAll((chips) =>
      chips.map((el) => {
        const cell = el.closest("td")!.getBoundingClientRect();
        const chipBox = el.getBoundingClientRect();
        const id = el.querySelector(".identifier")!.getBoundingClientRect();
        return {
          overRight: Math.round(chipBox.right - cell.right),
          idClipped: Math.round(id.right - chipBox.right),
        };
      }),
    );
    for (const spill of spills) {
      expect(spill.overRight).toBeLessThanOrEqual(1);
      expect(spill.idClipped).toBeLessThanOrEqual(1);
    }

    // Titles stay in the DOM (for the hover card) and are truncated, not removed.
    await expect(page.locator("tbody .truncate").first()).toBeAttached();
  });
}
