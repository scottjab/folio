package httpapi

import "errors"

// errMarkdownViaAttachments keeps notes going through the note endpoints, which
// are the ones that index, emit events, and honour compare-and-swap.
var errMarkdownViaAttachments = errors.New("markdown files must be written through the notes endpoint, not attachments")

// errNoFlush means the response writer cannot stream, which server-sent events
// require.
var errNoFlush = errors.New("streaming is not supported by this connection")

var errNoEmbedTarget = errors.New("an embed needs a target")
