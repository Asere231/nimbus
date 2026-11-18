package datadir

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// capture swaps the standard logger's output for the duration of a test.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	})
	return &buf
}

// An unrecognised file is reported, naming the directory it is in.
//
// This is the whole point of the package: before it, every store skipped a
// file it did not write in silence, so an object that was genuinely not being
// served read exactly like one that had never been uploaded.
func TestAnIgnoredFileIsReportedWithItsDirectory(t *testing.T) {
	buf := capture(t)
	ReportIgnored("s3", "/data/s3", 1)

	got := buf.String()
	if !strings.Contains(got, "/data/s3") {
		t.Errorf("the report does not say which directory: %q", got)
	}
	if !strings.Contains(got, "s3:") {
		t.Errorf("the report does not say which store: %q", got)
	}
	// Singular, because "1 files" reads as a bug in the thing reporting it.
	if !strings.Contains(got, "1 file ") {
		t.Errorf("one file should be reported in the singular: %q", got)
	}
}

func TestSeveralIgnoredFilesAreCountedInOneLine(t *testing.T) {
	buf := capture(t)
	ReportIgnored("blob", "/data/blob", 12)

	got := buf.String()
	if strings.Count(strings.TrimSpace(got), "\n") != 0 {
		t.Errorf("expected a single line, got %q", got)
	}
	if !strings.Contains(got, "12 files") {
		t.Errorf("the count is missing: %q", got)
	}
}

// A clean start stays clean.
//
// A diagnostic that prints on every start is one people learn to scroll past,
// which is the same argument this repo already made about the activity feed
// crying wolf over an unimplemented action that was working.
func TestNothingIsSaidWhenEveryFileIsAccountedFor(t *testing.T) {
	buf := capture(t)
	ReportIgnored("s3", "/data/s3", 0)
	ReportIgnored("s3", "/data/s3", -1)
	if got := buf.String(); got != "" {
		t.Errorf("expected silence, got %q", got)
	}
}

// Persistence off means there is no directory to report about.
func TestNothingIsSaidWithNoDataDirectory(t *testing.T) {
	buf := capture(t)
	ReportIgnored("s3", "", 3)
	if got := buf.String(); got != "" {
		t.Errorf("expected silence with no data directory, got %q", got)
	}
}
