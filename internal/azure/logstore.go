package azure

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/datadir"
	"github.com/Asere231/nimbus/internal/fsname"
)

// Where a workspace's logs actually live.
//
// `loganalytics.go` used to say queries "would find nothing because nothing
// was ever sent to it", and that was true for eleven milestones: Azure was
// the only cloud here with nowhere to keep what anything printed, while a
// Lambda's output landed in CloudWatch and a Cloud Run service's in its
// own container log.
//
// Same shape as `internal/aws/logs.go`, and bounded the same way -- but
// unlike it, these rows DO survive a restart when --persist is on.
//
// This file used to say they did not, and being honest about the gap was
// better than hiding it. The gap was still worth closing: an app's console
// is the only record of what it did, and losing it did not even fail
// loudly. A query against a reloaded workspace simply returned nothing,
// which is precisely the ambiguity logquery.go exists to avoid -- an empty
// result is indistinguishable from "nothing has been logged yet", the
// ordinary state of a workspace somebody is looking at for the first time.
// So the rows vanished and the workspace just looked unused.
//
// Keyed by the workspace's `customerId` rather than its ARM id, because
// that is what a client queries with: the data plane knows nothing about
// resource ids, exactly as the storage and Key Vault data planes do not.
// The customerId is also the FILENAME, through internal/fsname, for the
// reason every other store goes through it -- a GUID happens to be safe on
// every filesystem, and relying on that is how the next key that is not
// gets written straight to disk.

// maxWorkspaceLines bounds what one workspace keeps. Generous enough for
// anything an example produces, small enough that a chatty container
// cannot fill the machine.
const maxWorkspaceLines = 10000

// logLine is one row, in the shape a query answers with.
//
// The column names are Azure's, suffix and all: `TimeGenerated`,
// `ContainerAppName_s`, `Log_s`. The `_s` is Log Analytics' own convention
// for a string column on a custom table, and a query somebody copied from
// a tutorial names them exactly like this -- so they are not tidied.
type logLine struct {
	Time     string
	Table    string
	App      string
	Revision string
	Stream   string
	Message  string
}

// logStore holds every workspace's rows.
type logStore struct {
	mu    sync.Mutex
	lines map[string][]logLine
	dir   string // empty = memory only, which is the default
}

func newLogStore() *logStore { return &logStore{lines: map[string][]logLine{}} }

// Persist turns on the on-disk copy and reads back whatever is there.
//
// Called once at startup, like the storage and Key Vault stores' own
// Persist, and the same shape: set the directory, make it, load.
func (l *logStore) Persist(dir string) error {
	l.mu.Lock()
	l.dir = dir
	l.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	l.load()
	return nil
}

func (l *logStore) append(customerID string, lines ...logLine) {
	if customerID == "" || len(lines) == 0 {
		return
	}
	l.mu.Lock()
	kept := append(l.lines[customerID], lines...)
	if len(kept) > maxWorkspaceLines {
		kept = kept[len(kept)-maxWorkspaceLines:]
	}
	l.lines[customerID] = kept
	// Snapshot and directory are taken under the lock; the write happens
	// outside it, so a slow disk cannot stall the ingestion goroutine
	// that holds this lock every three seconds.
	snapshot := make([]logLine, len(kept))
	copy(snapshot, kept)
	dir := l.dir
	l.mu.Unlock()

	if dir != "" {
		writeWorkspaceLines(dir, customerID, snapshot)
	}
}

// all returns a workspace's rows, newest last, as a copy.
//
// A copy because a query sorts and filters what it is given, and the
// registry's own slice is appended to by the ingestion goroutine -- which
// is the data race every operation registry in this project has already
// taught, five times over. A reader gets its own.
func (l *logStore) all(customerID string) []logLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	src := l.lines[customerID]
	out := make([]logLine, len(src))
	copy(out, src)
	return out
}

// count is what the inspector shows without fetching every row.
func (l *logStore) count(customerID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines[customerID])
}

// forget drops a workspace's rows, when the workspace is deleted.
//
// Off disk too: a customerId that comes round again -- a destroy followed
// by an apply -- would otherwise surface the previous workspace's lines,
// which is worse than losing them.
func (l *logStore) forget(customerID string) {
	l.mu.Lock()
	delete(l.lines, customerID)
	dir := l.dir
	l.mu.Unlock()

	if dir != "" {
		if name, ok := workspaceFileName(customerID); ok {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// --- on disk ---------------------------------------------------------------

// workspaceFileSuffix is what a workspace's rows are stored under.
//
// It ends in "%" so it can never collide with an encoded customerId:
// fsname.Encode escapes a percent sign, so no name it produces ends in a
// bare one. The S3 object store learned this the expensive way -- its
// sidecar was ".json" until every object key ending in .json was dropped
// on load.
const workspaceFileSuffix = ".rows%"

func workspaceFileName(customerID string) (string, bool) {
	if customerID == "" {
		return "", false
	}
	return fsname.Encode(strings.ToLower(customerID)) + workspaceFileSuffix, true
}

func writeWorkspaceLines(dir, customerID string, lines []logLine) {
	name, ok := workspaceFileName(customerID)
	if !ok {
		return
	}
	data, err := json.Marshal(lines)
	if err != nil {
		return
	}
	// Written through a temporary file and renamed, because this is
	// rewritten every three seconds by the ingestion goroutine: a process
	// killed mid-write would otherwise leave a truncated file where a
	// whole one was, and the next start would read nothing.
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("nimbus: could not persist workspace %s logs: %v", customerID, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("nimbus: could not persist workspace %s logs: %v", customerID, err)
	}
}

// load reads every workspace's rows back.
//
// A file this did not write is ignored and counted, as every other store's
// data directory is -- ignoring it is right, and ignoring it in SILENCE is
// what made an orphan sit unmentioned.
func (l *logStore) load() {
	l.mu.Lock()
	dir := l.dir
	l.mu.Unlock()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	ignored := 0
	loaded := map[string][]logLine{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), workspaceFileSuffix) {
			// A .tmp left by a process killed mid-write is expected
			// rather than stray, so it is removed quietly instead of
			// being reported as something a person should look at.
			if strings.HasSuffix(e.Name(), ".tmp") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
				continue
			}
			ignored++
			continue
		}
		customerID, ok := fsname.Decode(strings.TrimSuffix(e.Name(), workspaceFileSuffix))
		if !ok {
			ignored++
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			ignored++
			continue
		}
		var lines []logLine
		if json.Unmarshal(raw, &lines) != nil {
			ignored++
			continue
		}
		loaded[customerID] = lines
	}
	datadir.ReportIgnored("log analytics", dir, ignored)

	l.mu.Lock()
	defer l.mu.Unlock()
	for id, lines := range loaded {
		l.lines[id] = lines
	}
}

// --- ingestion -------------------------------------------------------------

// logTailInterval is how often a running app's console is read.
//
// The same 3 seconds the k3s watcher polls a cluster at, and for the same
// reason: Docker has no push for this, the alternative is a long-lived
// attach per container, and three seconds is imperceptible to somebody
// watching a feed.
const logTailInterval = 3 * time.Second

// tailContainerApps copies every running app's console into the workspace
// its environment names, until the context is cancelled.
//
// An app whose environment names no workspace is skipped, and its output
// stays in the container's own console. That is what real Azure does --
// `appLogsConfiguration` on the environment is where logs are configured,
// and with none there is nowhere for them to go. Inventing a workspace
// would hide the one piece of configuration that makes logging work.
func (s *Server) tailContainerApps(ctx context.Context) {
	ticker := time.NewTicker(logTailInterval)
	defer ticker.Stop()
	// since remembers how far each app has been read, so a line is
	// ingested once. Docker's --since is second-granular, so a line can
	// repeat within a second; the dedupe below is what stops that.
	seen := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, svc := range s.apps.All() {
			s.ingestAppLogs(ctx, svc.ResourceID, svc.Name, seen)
		}
	}
}

// ingestAppLogs reads one app's console and appends what is new.
func (s *Server) ingestAppLogs(ctx context.Context, resourceID, name string, seen map[string]string) {
	customerID := s.workspaceForApp(resourceID)
	if customerID == "" {
		return
	}
	svc := s.apps.Get(resourceID)
	if svc == nil {
		return
	}
	raw, err := s.apps.Logs(ctx, svc, time.Time{})
	if err != nil {
		return
	}

	// Everything after the last line already ingested. Comparing on the
	// last line rather than on a timestamp, because `docker logs --since`
	// is second-granular and a busy container writes several lines in one
	// second -- a timestamp cursor would drop or repeat them.
	text := string(raw)
	if last := seen[resourceID]; last != "" {
		if i := strings.LastIndex(text, last); i >= 0 {
			text = text[i+len(last):]
		}
	}
	var lines []logLine
	now := time.Now().UTC().Format(time.RFC3339)
	var lastLine string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, logLine{
			Time: now, Table: tableContainerAppConsole,
			App: name, Stream: "stdout", Message: line,
		})
		lastLine = line
	}
	if lastLine != "" {
		seen[resourceID] = lastLine
	}
	s.logs.append(customerID, lines...)
}

// workspaceForApp finds the customerId of the workspace an app's
// environment logs to, or "" when there is none.
func (s *Server) workspaceForApp(appID string) string {
	res, err := s.store.Get(appID)
	if err != nil {
		return ""
	}
	var body map[string]any
	if json.Unmarshal(res.Body, &body) != nil {
		return ""
	}
	props, _ := body["properties"].(map[string]any)
	envID := str(props["managedEnvironmentId"])
	if envID == "" {
		return ""
	}

	env, err := s.store.Get(envID)
	if err != nil {
		return ""
	}
	var envBody map[string]any
	if json.Unmarshal(env.Body, &envBody) != nil {
		return ""
	}
	envProps, _ := envBody["properties"].(map[string]any)
	logsCfg, _ := envProps["appLogsConfiguration"].(map[string]any)
	laCfg, _ := logsCfg["logAnalyticsConfiguration"].(map[string]any)
	return str(laCfg["customerId"])
}

// workspaceCustomerIDs maps every workspace's customerId to its ARM id, so
// a query can tell "no such workspace" from "nothing logged yet".
func (s *Server) workspaceCustomerIDs() map[string]string {
	out := map[string]string{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return eq(r.Type, typeLogAnalyticsWorkspace)
	}) {
		var body map[string]any
		if json.Unmarshal(res.Body, &body) != nil {
			continue
		}
		props, _ := body["properties"].(map[string]any)
		if id := str(props["customerId"]); id != "" {
			out[strings.ToLower(id)] = res.ID
		}
	}
	return out
}

// sortLines puts rows in time order, oldest first, which is what every
// client expects of a log.
func sortLines(lines []logLine) {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time < lines[j].Time })
}

// ListLogs is the visualizer's view of a workspace: the most recent lines,
// newest last, optionally narrowed to one app.
//
// The shape is internal/ui's rather than this package's, which is why the
// adapter lives in cmd/nimbus -- the same arrangement the blob and secret
// browsers have, so internal/ui knows nothing about Log Analytics and
// internal/azure knows nothing about the visualizer.
func (s *Server) ListLogs(workspace, app string, limit int) []logLine {
	lines := s.logs.all(workspace)
	sortLines(lines)
	if app != "" {
		var narrowed []logLine
		for _, l := range lines {
			if eq(l.App, app) {
				narrowed = append(narrowed, l)
			}
		}
		lines = narrowed
	}
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

// LogLine is one row, exported so cmd/nimbus can adapt it to the
// visualizer's shape without this package importing internal/ui.
type LogLine struct {
	Time    string
	App     string
	Stream  string
	Message string
}

// Logs returns a workspace's lines in the exported shape.
func (s *Server) Logs(workspace, app string, limit int) []LogLine {
	in := s.ListLogs(workspace, app, limit)
	out := make([]LogLine, 0, len(in))
	for _, l := range in {
		out = append(out, LogLine{Time: l.Time, App: l.App, Stream: l.Stream, Message: l.Message})
	}
	return out
}
