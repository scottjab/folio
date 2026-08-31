package notes

import "testing"

func TestAppendToNote(t *testing.T) {
	tests := []struct {
		name, content, text, heading, want string
		wantErr                            bool
	}{
		{
			name:    "end of an empty note",
			content: "",
			text:    "new line",
			want:    "new line\n",
		},
		{
			name:    "end of a note",
			content: "# Title\n\nExisting.\n",
			text:    "new line",
			want:    "# Title\n\nExisting.\n\nnew line\n",
		},
		{
			name:    "collapses extra trailing blank lines",
			content: "# Title\n\nExisting.\n\n\n\n",
			text:    "new line",
			want:    "# Title\n\nExisting.\n\nnew line\n",
		},
		{
			name:    "under a heading, before the next section",
			content: "# Title\n\n## Tasks\n\n- one\n\n## Notes\n\nsomething\n",
			text:    "- two",
			heading: "Tasks",
			want:    "# Title\n\n## Tasks\n\n- one\n- two\n\n## Notes\n\nsomething\n",
		},
		{
			name:    "under the last heading",
			content: "# Title\n\n## Tasks\n\n- one\n",
			text:    "- two",
			heading: "Tasks",
			want:    "# Title\n\n## Tasks\n\n- one\n- two\n",
		},
		{
			name:    "under an empty section",
			content: "# Title\n\n## Tasks\n\n## Notes\n\nx\n",
			text:    "- first",
			heading: "Tasks",
			want:    "# Title\n\n## Tasks\n\n- first\n\n## Notes\n\nx\n",
		},
		{
			name:    "a deeper subheading does not end the section",
			content: "# Title\n\n## Tasks\n\n### Today\n\n- one\n\n## Notes\n\nx\n",
			text:    "- two",
			heading: "Tasks",
			want:    "# Title\n\n## Tasks\n\n### Today\n\n- one\n- two\n\n## Notes\n\nx\n",
		},
		{
			name:    "heading match ignores the hashes the caller typed",
			content: "# Title\n\n## Tasks\n\n- one\n",
			text:    "- two",
			heading: "## Tasks",
			want:    "# Title\n\n## Tasks\n\n- one\n- two\n",
		},
		{
			name:    "heading match is case insensitive",
			content: "## Tasks\n\n- one\n",
			text:    "- two",
			heading: "tasks",
			want:    "## Tasks\n\n- one\n- two\n",
		},
		{
			name:    "a heading inside a code fence is not a heading",
			content: "## Tasks\n\n```sh\n# Tasks\necho hi\n```\n\n- one\n",
			text:    "- two",
			heading: "Tasks",
			want:    "## Tasks\n\n```sh\n# Tasks\necho hi\n```\n\n- one\n- two\n",
		},
		{
			name:    "multi-line text",
			content: "## Tasks\n\n- one\n",
			text:    "- two\n- three",
			heading: "Tasks",
			want:    "## Tasks\n\n- one\n- two\n- three\n",
		},
		{
			name:    "empty text is a no-op",
			content: "# Title\n",
			text:    "",
			want:    "# Title\n",
		},
		{
			name:    "missing heading is an error",
			content: "# Title\n",
			text:    "x",
			heading: "Nope",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := appendToNote(tt.content, tt.text, tt.heading)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("appendToNote: %v", err)
			}
			if got != tt.want {
				t.Errorf("appendToNote:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestHeadingLevel(t *testing.T) {
	tests := map[string]int{
		"# One":         1,
		"## Two":        2,
		"###### Six":    6,
		"####### Seven": 0, // past the maximum
		"#hashtag":      0, // a tag, not a heading
		"#":             0,
		"text":          0,
		"":              0,
		"#\tTabbed":     1,
	}
	for line, want := range tests {
		if got := headingLevel(line); got != want {
			t.Errorf("headingLevel(%q) = %d, want %d", line, got, want)
		}
	}
}
