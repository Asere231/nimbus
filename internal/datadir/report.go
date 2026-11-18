// Package datadir says what the on-disk data directory held that Nimbus did
// not recognise.
//
// Every store that persists to disk skips a file it did not write, which is
// the right behaviour -- refusing to START over a stray file nobody asked
// about would be far worse, and a data directory somebody has copied, edited
// or half-migrated is an ordinary thing to find. What was wrong is that the
// skip was SILENT.
//
// Measured in Milestone 19: a junk file dropped at the top level of the data
// directory, a pre-fsname uppercase name, and a name fsname.Decode refuses
// all left the server starting normally with nothing said anywhere. So an
// object that is genuinely not being served reads exactly like one that was
// never uploaded -- and the orphan sits there unmentioned for ever. The file
// is still ignored; it is just no longer ignored quietly.
//
// One counted line per store rather than one line per file, because a
// directory somebody has been poking at can hold a great many of them, and a
// hundred lines of startup noise is its own way of saying nothing.
package datadir

import "log"

// ReportIgnored says how many files a store could not account for. It is
// silent when there are none, which is the normal case, so a clean start
// stays clean.
//
// kind names the store in the words its own clients use ("s3", "blob"), not
// the Go package, because the person reading the line is looking for where
// their object went.
func ReportIgnored(kind, dir string, n int) {
	if n <= 0 || dir == "" {
		return
	}
	noun := "files"
	if n == 1 {
		noun = "file"
	}
	log.Printf("nimbus: %s: ignoring %d %s under %s that Nimbus did not write; "+
		"nothing serves them, and they can be deleted", kind, n, noun, dir)
}
