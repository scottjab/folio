// Lezer markdown extensions for the two syntaxes Obsidian adds to CommonMark:
// [[wikilinks]] and #tags.
//
// These are real parser extensions rather than a regex pass over the text. That
// matters for three things: the live-preview decorations get proper syntax nodes
// to attach to, autocomplete can ask "am I inside a wikilink right now", and
// neither is ever matched inside a code span or fenced block, because the
// markdown parser has already claimed those.

import type { MarkdownConfig, InlineContext } from "@lezer/markdown";
import { tags as t, Tag } from "@lezer/highlight";

// Highlight tags for the nodes we add, so a theme can style them.
//
// Each parent must be a *base* tag. Tag.define refuses to derive from a modified
// one (anything built by t.special, t.definition and friends) and throws while
// the module is still loading, which takes the whole bundle down and leaves the
// browser showing a blank page.
export const wikilinkTag = Tag.define(t.link);
export const wikilinkMarkTag = Tag.define(t.processingInstruction);
export const hashtagTag = Tag.define(t.labelName);

const OPEN = 91; // [
const BANG = 33; // !
const HASH = 35; // #

/**
 * Parses `[[Target#anchor|alias]]` and its `![[embed]]` form.
 *
 * Registered before Link so that `[[x]]` is a wikilink rather than a link whose
 * text happens to be `[x]`.
 */
export const Wikilink: MarkdownConfig = {
  defineNodes: [
    { name: "Wikilink", style: wikilinkTag },
    { name: "WikilinkMark", style: wikilinkMarkTag },
    { name: "WikilinkTarget", style: wikilinkTag },
    { name: "WikilinkAnchor", style: wikilinkTag },
    { name: "WikilinkAlias", style: wikilinkTag },
    { name: "WikilinkEmbed", style: wikilinkTag },
  ],
  parseInline: [
    {
      name: "Wikilink",
      before: "Link",
      parse(cx: InlineContext, next: number, pos: number): number {
        const isEmbed = next === BANG;
        const start = pos;
        if (isEmbed) pos++;
        if (cx.char(pos) !== OPEN || cx.char(pos + 1) !== OPEN) return -1;

        const relStart = pos + 2 - cx.offset;
        const close = cx.text.indexOf("]]", relStart);
        if (close < 0) return -1;

        const innerStart = pos + 2;
        const innerEnd = close + cx.offset;
        const inner = cx.text.slice(relStart, close);
        // A wikilink never spans lines, and an unclosed one should not swallow
        // the rest of the paragraph.
        if (inner.includes("\n") || inner.includes("[")) return -1;
        if (inner.trim() === "") return -1;

        const children = [
          cx.elt("WikilinkMark", start, innerStart),
          ...splitWikilinkParts(cx, inner, innerStart),
          cx.elt("WikilinkMark", innerEnd, innerEnd + 2),
        ];
        return cx.addElement(
          cx.elt(isEmbed ? "WikilinkEmbed" : "Wikilink", start, innerEnd + 2, children),
        );
      },
    },
  ],
};

/** Splits a wikilink's interior into target, anchor, and alias nodes. */
function splitWikilinkParts(cx: InlineContext, inner: string, from: number) {
  const out = [];
  const hash = inner.indexOf("#");
  const pipe = inner.indexOf("|");

  const targetEnd = firstPositive(hash, pipe, inner.length);
  out.push(cx.elt("WikilinkTarget", from, from + targetEnd));

  if (hash >= 0) {
    const anchorEnd = pipe > hash ? pipe : inner.length;
    out.push(cx.elt("WikilinkAnchor", from + hash, from + anchorEnd));
  }
  if (pipe >= 0) {
    out.push(cx.elt("WikilinkAlias", from + pipe, from + inner.length));
  }
  return out;
}

function firstPositive(...vals: number[]): number {
  for (const v of vals) if (v >= 0) return v;
  return vals[vals.length - 1];
}

/**
 * Parses `#tag`, including nested forms like `#work/urgent`.
 *
 * A tag must contain at least one non-digit, so an issue reference such as
 * `#123` stays plain text, and must follow whitespace or the start of a line,
 * so `a#b` and a URL fragment are not tags.
 */
export const Hashtag: MarkdownConfig = {
  defineNodes: [{ name: "Hashtag", style: hashtagTag }],
  parseInline: [
    {
      name: "Hashtag",
      parse(cx: InlineContext, next: number, pos: number): number {
        if (next !== HASH) return -1;
        const before = pos > cx.offset ? cx.char(pos - 1) : -1;
        if (before >= 0 && !isTagBoundary(before)) return -1;

        let end = pos + 1;
        let hasLetter = false;
        while (end < cx.end) {
          const ch = cx.char(end);
          if (!isTagChar(ch)) break;
          if (!isDigit(ch)) hasLetter = true;
          end++;
        }
        if (end === pos + 1 || !hasLetter) return -1;
        // A trailing separator belongs to the sentence, not the tag.
        while (end > pos + 1 && isTrailingPunctuation(cx.char(end - 1))) end--;
        if (end === pos + 1) return -1;

        return cx.addElement(cx.elt("Hashtag", pos, end));
      },
    },
  ],
};

function isDigit(ch: number) {
  return ch >= 48 && ch <= 57;
}

function isTagChar(ch: number) {
  return (
    (ch >= 97 && ch <= 122) || // a-z
    (ch >= 65 && ch <= 90) || // A-Z
    isDigit(ch) ||
    ch === 95 || // _
    ch === 45 || // -
    ch === 47 || // /
    ch > 127 // let non-ASCII letters through; tags are not English-only
  );
}

function isTrailingPunctuation(ch: number) {
  return ch === 45 || ch === 47; // a tag should not end in - or /
}

function isTagBoundary(ch: number) {
  // Start of line, whitespace, or an opening bracket: anywhere a word could
  // start. Notably not a letter, so "a#b" is not a tag.
  return (
    ch === 32 || ch === 9 || ch === 10 || ch === 13 ||
    ch === 40 || ch === 91 || ch === 62 || ch === 123
  );
}

/** Every markdown extension folio adds. */
export const folioMarkdown: MarkdownConfig[] = [Wikilink, Hashtag];
