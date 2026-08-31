import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * These tests exist because of a bug no other kind of test could see.
 *
 * CodeMirror ships a base theme, injected at runtime and scoped by a generated
 * class, which puts its rules at a specificity of 0,2,0. Our stylesheet was
 * written with bare class selectors at 0,1,0, so the rules for the caret, the
 * active line, and the editor's horizontal padding were being written and then
 * silently overridden. The caret rendered as CodeMirror's default 1.2px black
 * hairline, which on a page of rendered markdown is very hard to find.
 *
 * These assertions are deliberately static rather than computed-style checks.
 * jsdom's getComputedStyle does not implement cascade specificity, so it reports
 * CodeMirror's values whatever we write; a test that passed there would be
 * measuring the quirk, not the stylesheet. The invariant that actually prevents
 * the bug is a property of the source, so that is what is checked.
 */

const css = readFileSync(join(process.cwd(), "src", "style.css"), "utf8");

/** The stylesheet with comments removed, so selector scanning sees only rules. */
const rules = css.replace(/\/\*[\s\S]*?\*\//g, "");

/** Every selector in the stylesheet, split on commas. */
function selectors(): string[] {
  const out: string[] = [];
  for (const m of rules.matchAll(/(?:^|\})([^{}]+)\{/g)) {
    const sel = m[1].trim();
    if (!sel || sel.startsWith("@")) continue;
    out.push(...sel.split(",").map((s) => s.trim()).filter(Boolean));
  }
  return out;
}

describe("styles that have to beat CodeMirror's base theme", () => {
  it("scopes every CodeMirror override so it outranks the base theme", () => {
    // Our own live-preview classes need no scoping: CodeMirror has no rules for
    // them. Autocomplete tooltips are rendered outside the editor element, so
    // they cannot be scoped that way and double their class instead.
    const offenders = selectors().filter((sel) => {
      if (!sel.includes(".cm-")) return false;
      if (sel.startsWith(".cm-fol-")) return false;
      if (sel.includes(".cm-editor")) return false;
      if (sel.includes(".cm-tooltip.cm-tooltip")) return false;
      return true;
    });
    expect(offenders).toEqual([]);
  });

  it("gives the caret a width CodeMirror's hairline cannot win against", () => {
    const rule = /\.cm-editor \.cm-cursorLayer \.cm-cursor[^{]*\{([^}]*)\}/.exec(rules);
    expect(rule, "no scoped rule for the caret").not.toBeNull();

    const width = /border-left-width:\s*([\d.]+)px/.exec(rule![1]);
    expect(width, "the caret needs an explicit width").not.toBeNull();
    // CodeMirror's default is 1.2px, which is what was rendering before.
    expect(Number(width![1])).toBeGreaterThan(1.2);
  });

  it("colours the caret from a token defined in both themes", () => {
    expect(rules).toMatch(/border-left-color:\s*var\(--cursor\)/);
    expect(rules).toMatch(/:root\s*\{[^}]*--cursor:/s);
    expect(rules).toMatch(/prefers-color-scheme:\s*dark[\s\S]*?--cursor:/);
  });

  it("marks the active line and lets a selection show through it", () => {
    expect(rules).toMatch(/\.cm-editor \.cm-line\.cm-activeLine\s*\{[^}]*--active-line/);
    expect(rules).toMatch(/\.cm-editor \.cm-activeLine \.cm-selectionBackground\s*\{[^}]*--selection/);
  });

  it("keeps horizontal padding on the content, which CodeMirror also sets", () => {
    const rule = /\.cm-editor \.cm-content\s*\{([^}]*)\}/.exec(rules);
    expect(rule, "the content rule must be scoped or its padding is dropped").not.toBeNull();
    expect(rule![1]).toMatch(/padding:/);
  });

  it("defines every colour token it uses in both themes", () => {
    const used = new Set([...rules.matchAll(/var\((--[a-z-]+)\)/g)].map((m) => m[1]));
    const light = rules.slice(rules.indexOf(":root"), rules.indexOf("@media"));
    const dark = rules.slice(rules.indexOf("prefers-color-scheme: dark"));

    const missingLight = [...used].filter((t) => !light.includes(t + ":"));
    expect(missingLight, "tokens with no light-mode value").toEqual([]);

    // Dark mode only overrides colours, so a token defined once in :root and
    // never overridden is fine. The ones that must differ are the ones a dark
    // background would otherwise swallow.
    for (const t of ["--cursor", "--selection", "--active-line", "--text", "--bg"]) {
      expect(dark, `${t} must be redefined for dark mode`).toContain(t + ":");
    }
  });

  it("respects a request for reduced motion", () => {
    // CodeMirror blinks the cursor layer and does not check this itself.
    expect(rules).toMatch(/prefers-reduced-motion[\s\S]*?cm-cursorLayer[^}]*animation:\s*none/);
  });
});
