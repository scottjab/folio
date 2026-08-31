// jsdom has no layout engine, so it does not implement the measurement APIs
// CodeMirror calls to work out line heights and cursor positions. Without these
// stubs every editor test finishes with a pile of unhandled errors from a
// requestAnimationFrame callback, which drowns out real failures.
//
// The numbers are arbitrary and unused: these tests assert on the DOM the editor
// produces, never on geometry.

const emptyRect: DOMRect = {
  x: 0, y: 0, top: 0, left: 0, bottom: 0, right: 0, width: 0, height: 0,
  toJSON: () => ({}),
};

function rectList(): DOMRectList {
  const list = [emptyRect] as unknown as DOMRectList;
  (list as unknown as { item: (i: number) => DOMRect | null }).item = (i) =>
    i === 0 ? emptyRect : null;
  return list;
}

Range.prototype.getClientRects = () => rectList();
Range.prototype.getBoundingClientRect = () => emptyRect;

if (!Element.prototype.getClientRects) {
  Element.prototype.getClientRects = () => rectList();
}

// CodeMirror only considers itself focused when the *document* has focus as
// well as the content element. jsdom always reports the document as unfocused,
// so without this the editor never gets .cm-focused and never draws a cursor,
// and the tests that check the cursor is visible could not tell a configuration
// mistake from an environment limitation.
document.hasFocus = () => true;

// CodeMirror measures against these; jsdom reports 0 for everything, which is
// fine as long as the properties exist.
if (!window.matchMedia) {
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as typeof window.matchMedia;
}
