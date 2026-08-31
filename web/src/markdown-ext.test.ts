import { describe, expect, it } from "vitest";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { ensureSyntaxTree } from "@codemirror/language";
import { EditorState } from "@codemirror/state";

import { folioMarkdown } from "./markdown-ext";

/** Parses a document and returns every syntax node name in it. */
function nodeNames(doc: string): string[] {
  const state = EditorState.create({
    doc,
    extensions: [markdown({ base: markdownLanguage, extensions: folioMarkdown })],
  });
  const tree = ensureSyntaxTree(state, doc.length, 5000);
  if (!tree) throw new Error("the document did not parse");

  const names: string[] = [];
  tree.iterate({ enter: (n) => void names.push(n.name) });
  return names;
}

/** Returns the source text of every node with the given name. */
function textOf(doc: string, nodeName: string): string[] {
  const state = EditorState.create({
    doc,
    extensions: [markdown({ base: markdownLanguage, extensions: folioMarkdown })],
  });
  const tree = ensureSyntaxTree(state, doc.length, 5000)!;
  const out: string[] = [];
  tree.iterate({
    enter: (n) => {
      if (n.name === nodeName) out.push(doc.slice(n.from, n.to));
    },
  });
  return out;
}

describe("the extensions load at all", () => {
  it("defines its highlight tags without throwing", async () => {
    // Tag.define refuses to derive from a *modified* tag, and it throws at
    // module load. When that happened the whole bundle failed to evaluate and
    // the app was a blank page with one console error. Importing the module is
    // the entire test.
    const mod = await import("./markdown-ext");
    expect(mod.wikilinkTag).toBeDefined();
    expect(mod.hashtagTag).toBeDefined();
    expect(mod.folioMarkdown.length).toBeGreaterThan(0);
  });

  it("parses a plain document", () => {
    expect(nodeNames("# Hello\n\nSome text.\n")).toContain("ATXHeading1");
  });
});

describe("wikilinks", () => {
  it("parses the plain form", () => {
    expect(textOf("See [[Projects/folio]] today.\n", "Wikilink")).toEqual([
      "[[Projects/folio]]",
    ]);
    expect(textOf("See [[Projects/folio]] today.\n", "WikilinkTarget")).toEqual([
      "Projects/folio",
    ]);
  });

  it("splits the anchor and the alias", () => {
    const doc = "[[a/b#Some Heading|the alias]]\n";
    expect(textOf(doc, "WikilinkTarget")).toEqual(["a/b"]);
    expect(textOf(doc, "WikilinkAnchor")).toEqual(["#Some Heading"]);
    expect(textOf(doc, "WikilinkAlias")).toEqual(["|the alias"]);
  });

  it("parses the embed form separately", () => {
    expect(textOf("![[attachments/diagram.png]]\n", "WikilinkEmbed")).toEqual([
      "![[attachments/diagram.png]]",
    ]);
    expect(nodeNames("![[a.png]]\n")).not.toContain("Wikilink");
  });

  it("wins over the markdown link parser", () => {
    // Registered before Link, so [[x]] is one wikilink rather than a link whose
    // text happens to be [x].
    expect(nodeNames("[[x]]\n")).toContain("Wikilink");
  });

  it("leaves real markdown links alone", () => {
    const names = nodeNames("[text](url)\n");
    expect(names).toContain("Link");
    expect(names).not.toContain("Wikilink");
  });

  it("is not fooled by unclosed or empty brackets", () => {
    for (const doc of ["[[unclosed\n", "[[]]\n", "[[ ]]\n", "[[a\nb]]\n"]) {
      expect(nodeNames(doc)).not.toContain("Wikilink");
    }
  });

  it("never matches inside code", () => {
    // A fenced example showing [[Some/Path]] is documentation, not a link.
    expect(nodeNames("`[[nope]]`\n")).not.toContain("Wikilink");
    expect(nodeNames("```\n[[nope]]\n```\n")).not.toContain("Wikilink");
    expect(nodeNames("    [[nope]]\n")).not.toContain("Wikilink");
  });

  it("handles several on one line", () => {
    expect(textOf("[[a]] and [[b]] and [[c]]\n", "WikilinkTarget")).toEqual(["a", "b", "c"]);
  });
});

describe("hashtags", () => {
  it("parses simple and nested tags", () => {
    expect(textOf("#go127 and #work/urgent\n", "Hashtag")).toEqual(["#go127", "#work/urgent"]);
  });

  it("accepts dashes and underscores", () => {
    expect(textOf("#with-dash #with_underscore\n", "Hashtag")).toEqual([
      "#with-dash",
      "#with_underscore",
    ]);
  });

  it("requires at least one non-digit, so issue references stay text", () => {
    expect(nodeNames("#123\n")).not.toContain("Hashtag");
  });

  it("requires a word boundary before the hash", () => {
    // Otherwise "a#b" and a URL fragment would both become tags.
    expect(nodeNames("a#b\n")).not.toContain("Hashtag");
    expect(nodeNames("See https://example.com/page#frag\n")).not.toContain("Hashtag");
  });

  it("does not treat a heading as a tag", () => {
    const names = nodeNames("# Heading\n");
    expect(names).toContain("ATXHeading1");
    expect(names).not.toContain("Hashtag");
  });

  it("does not match a bare hash", () => {
    expect(nodeNames("# \n")).not.toContain("Hashtag");
    expect(nodeNames("a # b\n")).not.toContain("Hashtag");
  });

  it("stops before trailing punctuation", () => {
    expect(textOf("Ends the sentence with #tag.\n", "Hashtag")).toEqual(["#tag"]);
    expect(textOf("A #tag, then more\n", "Hashtag")).toEqual(["#tag"]);
  });

  it("never matches inside code", () => {
    expect(nodeNames("`#nope`\n")).not.toContain("Hashtag");
    expect(nodeNames("```\n#nope\n```\n")).not.toContain("Hashtag");
  });

  it("allows non-ASCII letters", () => {
    expect(textOf("#café and #日本語\n", "Hashtag")).toEqual(["#café", "#日本語"]);
  });
});
