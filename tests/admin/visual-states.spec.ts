import { test, expect } from "@playwright/test";
import { mkdir, writeFile } from "node:fs/promises";

test("ledger selection caret and scroll surface are authored and active", async ({
  page,
}) => {
  await mkdir("output/playwright/ledger-review", { recursive: true });
  await page.goto("/en");
  const input = page.locator(".search-field input");
  await input.fill("Visible selection");
  await input.selectText();
  const styles = await input.evaluate((element) => {
    const field = element as HTMLInputElement;
    return {
      selectionStart: field.selectionStart,
      selectionEnd: field.selectionEnd,
      selectionBackground: getComputedStyle(field, "::selection")
        .backgroundColor,
      selectionColor: getComputedStyle(field, "::selection").color,
      caretColor: getComputedStyle(field).caretColor,
      focused: document.activeElement === field,
    };
  });
  expect(styles).toMatchObject({
    selectionStart: 0,
    selectionEnd: 17,
    selectionBackground: "rgb(201, 230, 222)",
    selectionColor: "rgb(20, 41, 66)",
    caretColor: "rgb(36, 121, 101)",
    focused: true,
  });
  await input.screenshot({
    path: "output/playwright/ledger-review/selection-active.png",
    caret: "initial",
  });
  await input.press("ArrowRight");
  const caret = await input.evaluate((element) => ({
    start: (element as HTMLInputElement).selectionStart,
    end: (element as HTMLInputElement).selectionEnd,
    focused: document.activeElement === element,
  }));
  expect(caret).toEqual({ start: 17, end: 17, focused: true });
  await input.screenshot({
    path: "output/playwright/ledger-review/caret-active.png",
    caret: "initial",
  });
  await input.fill("");

  // Use real responsive widths; never inject a fake overflow or scrollbar.
  const table = page.locator(".table-scroll");
  let overflowWidth = 0;
  for (const width of [1100, 900, 820, 740, 681]) {
    await page.setViewportSize({ width, height: 992 });
    if (
      await table.evaluate(
        (element) => element.scrollWidth > element.clientWidth,
      )
    ) {
      overflowWidth = width;
      break;
    }
  }
  expect(overflowWidth).toBeGreaterThan(0);
  await table.hover();
  await page.mouse.wheel(120, 0);
  await expect
    .poll(() => table.evaluate((element) => element.scrollLeft))
    .toBeGreaterThan(0);
  const scroll = await table.evaluate((element) => ({
    scrollWidth: element.scrollWidth,
    clientWidth: element.clientWidth,
    scrollLeft: element.scrollLeft,
    scrollbarColor: getComputedStyle(element).scrollbarColor,
    scrollbarWidth: getComputedStyle(element).scrollbarWidth,
  }));
  expect(scroll.scrollbarColor).toBe("rgb(113, 134, 158) rgb(237, 242, 247)");
  expect(scroll.scrollbarWidth).toBe("thin");
  await table.screenshot({ path: "output/playwright/ledger-review/scrollbar-active.png" });
  await writeFile(
    "output/playwright/ledger-review/active-style-evidence.json",
    JSON.stringify({ styles, caret, overflowWidth, scroll }, null, 2),
  );
  // Next retains hidden dev-tool DOM; only visible chrome can cover the UI.
  await expect(
    page.locator("nextjs-portal").locator("button:visible"),
  ).toHaveCount(0);
});
