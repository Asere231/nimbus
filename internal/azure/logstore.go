package azure

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Where a workspace's logs actually live.
//
// `loganalytics.go` used to say queries "would find nothing because nothing
// was ever sent to it", and that was true for eleven milestones: Azure was
// the only cloud here with nowhere to keep what anything printed, while a
// Lambda's output landed in CloudWatch and a Cloud Run service's in its
// own container log.
//
// Same shape as `internal/aws/logs.go`, and the same honest limits:
// bounded, in memory, and NOT persisted even with --persist. A workspace
// survives a restart; its rows do not. CloudWatch Logs' and Pub/Sub's
// entries in docs/ROADMAP.md already say this about themselves, and this
// one says it too rather than leaving someone to discover it.
//
// Keyed by the workspace's `customerId` rather than its ARM id, because
// that is what a client queries with: the data plane knows nothing about
// resource ids, exactly as the storage and Key Vault data planes do not.

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
}

func newLogStore() *logStore { return &logStore{lines: map[string][]logLine{}} }

func (l *logStore) append(customerID string, lines ...logLine) {
	if customerID == "" || len(lines) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := append(l.lines[customerID], lines...)
	if len(kept) > maxWorkspaceLines {
		kept = kept[len(kept)-maxWorkspaceLines:]
	}
	l.lines[customerID] = kept
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
func (l *logStore) forget(customerID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.lines, customerID)
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
