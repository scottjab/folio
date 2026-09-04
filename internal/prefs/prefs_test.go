package prefs_test

import (
	"errors"
	"testing"

	"github.com/scottjab/folio/internal/prefs"
)

func TestAttachmentsDir(t *testing.T) {
	tests := []struct {
		name string
		mode prefs.AttachmentMode
		fold string
		note string
		want string
	}{
		{"vault root ignores the note", prefs.AttachVault, "attachments", "Daily/2026-09-03.md", ""},
		{"one folder for everything", prefs.AttachFolder, "attachments", "Daily/2026-09-03.md", "attachments"},
		{"one folder, note at the root", prefs.AttachFolder, "files", "n.md", "files"},
		{"beside the note", prefs.AttachCurrent, "attachments", "Daily/2026-09-03.md", "Daily"},
		{"beside a note at the root", prefs.AttachCurrent, "attachments", "n.md", ""},
		{"subfolder of the note's folder", prefs.AttachSubfolder, "files", "Daily/2026-09-03.md", "Daily/files"},
		{"subfolder when the note is at the root", prefs.AttachSubfolder, "files", "n.md", "files"},

		// A caller with nothing open still has to get an answer, because the
		// terminal client can attach without a note in front of it.
		{"no note, vault root", prefs.AttachVault, "attachments", "", ""},
		{"no note, fixed folder", prefs.AttachFolder, "attachments", "", "attachments"},
		{"no note, beside the note", prefs.AttachCurrent, "attachments", "", ""},
		{"no note, subfolder", prefs.AttachSubfolder, "files", "", "files"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := prefs.Attachments{Mode: tc.mode, Folder: tc.fold}
			if got := a.Dir(tc.note); got != tc.want {
				t.Errorf("Dir(%q) = %q, want %q", tc.note, got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	ok := []prefs.Attachments{
		{Mode: prefs.AttachFolder, Folder: "attachments"},
		{Mode: prefs.AttachFolder, Folder: "assets/images"},
		{Mode: prefs.AttachSubfolder, Folder: "files"},
		{Mode: prefs.AttachVault, Folder: ""},
		// The folder is kept across a mode change so switching away and back
		// does not lose it, which means it has to validate while unused.
		{Mode: prefs.AttachVault, Folder: "attachments"},
		{Mode: prefs.AttachCurrent, Folder: "attachments"},
	}
	for _, a := range ok {
		if err := (prefs.Prefs{Attachments: a}).Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", a, err)
		}
	}

	bad := []struct {
		name string
		a    prefs.Attachments
	}{
		{"unknown mode", prefs.Attachments{Mode: "elsewhere", Folder: "attachments"}},
		{"empty mode", prefs.Attachments{Mode: "", Folder: "attachments"}},
		{"folder mode with no folder", prefs.Attachments{Mode: prefs.AttachFolder}},
		{"folder mode with only spaces", prefs.Attachments{Mode: prefs.AttachFolder, Folder: "  "}},
		{"subfolder mode with no folder", prefs.Attachments{Mode: prefs.AttachSubfolder}},
		{"escaping the vault", prefs.Attachments{Mode: prefs.AttachFolder, Folder: "../elsewhere"}},
		{"absolute", prefs.Attachments{Mode: prefs.AttachFolder, Folder: "/etc"}},
		{"trailing slash is not canonical", prefs.Attachments{Mode: prefs.AttachFolder, Folder: "attachments/"}},
		{"hidden folders are never served", prefs.Attachments{Mode: prefs.AttachFolder, Folder: ".secret"}},
		{"our own sidecar", prefs.Attachments{Mode: prefs.AttachFolder, Folder: ".folio"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := (prefs.Prefs{Attachments: tc.a}).Validate()
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.a)
			}
			if !errors.Is(err, prefs.ErrInvalidPref) {
				t.Errorf("error %v does not wrap ErrInvalidPref", err)
			}
		})
	}
}

func TestDefaultIsValid(t *testing.T) {
	if err := prefs.Default().Validate(); err != nil {
		t.Fatalf("the default preferences do not validate: %v", err)
	}
	if got := prefs.Default().Attachments.Dir("Daily/x.md"); got != "attachments" {
		t.Errorf("default attachment dir = %q, want %q", got, "attachments")
	}
}
