// Unit tests for lib/legal-copy.ts (legal-pages unit). Pure data; no server, no network.
// Also writes the owner's to-do list (every rendered owner-text marker) to output/legal-pages/.
import test from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { legalFooterLinks, legalPage, legalSlugs, pendingOwnerText } from "../lib/legal-copy.ts";

const locales = ["zh-TW", "zh-CN", "en"];

test("every locale x page has a title, an updated line and at least one section", () => {
  for (const l of locales) {
    for (const s of legalSlugs) {
      const p = legalPage(l, s);
      assert.ok(p.title.length > 0, `${l}/${s} title`);
      assert.ok(p.updated.text.length > 0, `${l}/${s} updated`);
      assert.ok(p.sections.length >= 1, `${l}/${s} sections`);
      for (const sec of p.sections) {
        assert.ok(sec.heading && sec.body.length >= 1, `${l}/${s}/${sec.heading}`);
      }
    }
  }
});

test("locales share the section count and per-section body count of every page", () => {
  for (const s of legalSlugs) {
    const shape = (l) => legalPage(l, s).sections.map((x) => x.body.length).join(",");
    for (const l of locales) assert.equal(shape(l), shape("en"), `${l}/${s} differs from en`);
  }
});

test("engineering ships no final text: every text is a draft or pending marker", () => {
  for (const l of locales)
    for (const s of legalSlugs) {
      const p = legalPage(l, s);
      for (const t of [p.updated, ...p.sections.flatMap((x) => x.body)]) assert.notEqual(t.kind, "final");
    }
});

test("footer has 6 unique hrefs: 5 legal slugs plus data-deletion, per locale", () => {
  for (const l of locales) {
    const links = legalFooterLinks(l);
    assert.equal(new Set(links.map((x) => x.href)).size, 6);
    assert.deepEqual(
      links.map((x) => x.href),
      [...legalSlugs.map((s) => `/${l}/legal/${s}`), `/${l}/data-deletion`],
    );
    assert.ok(links.every((x) => x.label.length > 0));
  }
});

test("data-deletion path is named in the privacy page of every locale", () => {
  for (const l of locales) {
    const text = legalPage(l, "privacy").sections.flatMap((x) => x.body.map((t) => t.text)).join("\n");
    assert.ok(text.includes(`/${l}/data-deletion`));
    assert.ok(text.includes(`/${l}/privacy`));
  }
});

test("pendingOwnerText lists exactly the rendered markers and is written for the owner", () => {
  let expected = 0;
  for (const l of locales)
    for (const s of legalSlugs) {
      const p = legalPage(l, s);
      expected += [p.updated, ...p.sections.flatMap((x) => x.body)].filter((t) => t.kind !== "final").length;
    }
  const list = pendingOwnerText();
  assert.equal(list.length, expected);
  assert.ok(list.length > 0);
  for (const kind of ["pending", "draft"]) assert.ok(list.some((x) => x.kind === kind));
  const en = list.filter((x) => x.locale === "en" && x.kind === "pending").map((x) => x.what.toLowerCase());
  for (const need of ["legal entity name", "registered business address", "support email", "support phone",
    "refund window", "return conditions", "cancellation rule", "shipping regions", "carriers", "lead time",
    "shipping fees", "governing law", "effective date", "business description"])
    assert.ok(en.some((w) => w.includes(need)), `owner list lacks: ${need}`);

  // Default: this checkout's output/legal-pages (gitignored). Set LEGAL_OWNER_LIST_DIR to the MAIN
  // checkout's output dir (PROCESS §4) when running from a worktree.
  const dir = process.env.LEGAL_OWNER_LIST_DIR ?? new URL("../../../output/legal-pages", import.meta.url).pathname;
  mkdirSync(dir, { recursive: true });
  const lines = list.map((x) => `${x.kind.toUpperCase()}\t${x.locale}\t${x.page}\t${x.what}`);
  writeFileSync(`${dir}/owner-text-needed.txt`,
    `# Owner text to supply (PENDING) or approve (DRAFT): ${list.length} markers\n# kind\tlocale\tpage\twhat\n${lines.join("\n")}\n`);
});
