package markdown_test

import (
	"testing"

	"github.com/scottjab/folio/internal/markdown"
)

const sectionDoc = `# Title

Intro text.

## Tasks

- one
- two

### Subtasks

- nested stays in

## Notes

Not part of Tasks.

## Code

` + "```" + `md
## Not a heading, it is an example
` + "```" + `

Still Code.
`

func TestSection(t *testing.T) {
	tests := []struct {
		name    string
		heading string
		want    string
	}{
		{
			// The heading line itself is included, which is what Obsidian renders.
			name:    "stops at the next heading of the same level",
			heading: "Tasks",
			want:    "## Tasks\n\n- one\n- two\n\n### Subtasks\n\n- nested stays in\n\n",
		},
		{
			name:    "a deeper heading is nested content, not a boundary",
			heading: "Subtasks",
			want:    "### Subtasks\n\n- nested stays in\n\n",
		},
		{
			name:    "the top heading runs to the next one of its level",
			heading: "Title",
			want:    "# Title\n\nIntro text.\n\n## Tasks\n\n- one\n- two\n\n### Subtasks\n\n- nested stays in\n\n## Notes\n\nNot part of Tasks.\n\n## Code\n\n```md\n## Not a heading, it is an example\n```\n\nStill Code.\n",
		},
		{
			// A heading inside a fence is an example. Treating it as a boundary
			// would truncate the section at the code block.
			name:    "a heading inside a fence is not a boundary",
			heading: "Code",
			want:    "## Code\n\n```md\n## Not a heading, it is an example\n```\n\nStill Code.\n",
		},
		{
			name:    "matching ignores case",
			heading: "tASKS",
			want:    "## Tasks\n\n- one\n- two\n\n### Subtasks\n\n- nested stays in\n\n",
		},
		{
			// A link copied out of a rendered page carries the slug.
			name:    "a slug matches too",
			heading: "subtasks",
			want:    "### Subtasks\n\n- nested stays in\n\n",
		},
		{
			name:    "leading hashes in the anchor are tolerated",
			heading: "## Tasks",
			want:    "## Tasks\n\n- one\n- two\n\n### Subtasks\n\n- nested stays in\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := markdown.Section([]byte(sectionDoc), tc.heading)
			if !ok {
				t.Fatalf("Section(%q) reported not found", tc.heading)
			}
			if string(got) != tc.want {
				t.Errorf("Section(%q) =\n%q\nwant\n%q", tc.heading, got, tc.want)
			}
		})
	}
}

func TestSectionMisses(t *testing.T) {
	for _, heading := range []string{"Nope", "", "   ", "Not a heading, it is an example"} {
		if got, ok := markdown.Section([]byte(sectionDoc), heading); ok {
			t.Errorf("Section(%q) = %q, want not found", heading, got)
		}
	}
}

func TestSectionIsByteExact(t *testing.T) {
	// Joining the captured lines has to reproduce the source, or an embed shows
	// subtly different text from the note it came from.
	src := "## A\r\nwindows line\r\n\r\n## B\r\n"
	got, ok := markdown.Section([]byte(src), "A")
	if !ok {
		t.Fatal("Section reported not found")
	}
	if want := "## A\r\nwindows line\r\n\r\n"; string(got) != want {
		t.Errorf("Section = %q, want %q", got, want)
	}
}

func TestSectionSkipsWhatLooksLikeAHeading(t *testing.T) {
	// "#tag" and an indented code block must not register as headings, or a
	// section ends in the wrong place.
	src := "## A\n\n#tag on its own line\n\n    # indented code\n\n## B\n"
	got, ok := markdown.Section([]byte(src), "A")
	if !ok {
		t.Fatal("Section reported not found")
	}
	if want := "## A\n\n#tag on its own line\n\n    # indented code\n\n"; string(got) != want {
		t.Errorf("Section = %q, want %q", got, want)
	}
}
