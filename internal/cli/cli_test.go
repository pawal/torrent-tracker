package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/store"
)

// run invokes Main with a scratch database and captures stdout.
func run(t *testing.T, db string, args ...string) (code int, stdout string) {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	code = Main(append([]string{"--db", db}, args...))

	w.Close()
	os.Stdout = orig
	return code, <-done
}

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cli.db")
}

func TestMainNoArgs(t *testing.T) {
	if code := Main(nil); code != 2 {
		t.Errorf("Main(nil) = %d, want 2", code)
	}
}

func TestMainUnknownCommand(t *testing.T) {
	if code := Main([]string{"--db", tempDB(t), "frobnicate"}); code != 2 {
		t.Errorf("unknown command = %d, want 2", code)
	}
}

func TestMainBadGlobalFlag(t *testing.T) {
	if code := Main([]string{"--nope"}); code != 2 {
		t.Errorf("bad flag = %d, want 2", code)
	}
}

func TestAddListRemoveRoundTrip(t *testing.T) {
	db := tempDB(t)

	// Announce URLs are accepted, not just bare hostnames.
	code, out := run(t, db, "add", "udp://tracker.example.com:1337/announce", "other.example.org")
	if code != 0 {
		t.Fatalf("add = %d: %s", code, out)
	}
	if !strings.Contains(out, "2 added") {
		t.Errorf("add output = %q, want 2 added", out)
	}

	code, out = run(t, db, "list", "--names")
	if code != 0 {
		t.Fatalf("list = %d: %s", code, out)
	}
	for _, want := range []string{"tracker.example.com", "other.example.org"} {
		if !strings.Contains(out, want) {
			t.Errorf("list is missing %q:\n%s", want, out)
		}
	}

	// Adding again is reported, not duplicated.
	_, out = run(t, db, "add", "tracker.example.com")
	if !strings.Contains(out, "already known") {
		t.Errorf("re-add output = %q", out)
	}

	// A soft remove hides it from the default listing but keeps the row.
	if code, out = run(t, db, "rm", "tracker.example.com"); code != 0 {
		t.Fatalf("rm = %d: %s", code, out)
	}
	_, out = run(t, db, "list", "--names")
	if strings.Contains(out, "tracker.example.com") {
		t.Errorf("removed tracker still listed:\n%s", out)
	}
	_, out = run(t, db, "list", "--all", "--names")
	if !strings.Contains(out, "tracker.example.com") {
		t.Errorf("--all should show removed trackers:\n%s", out)
	}
}

func TestAddSkipsUnusableNames(t *testing.T) {
	db := tempDB(t)
	// An IP literal has no DNS history to track; the good name still lands.
	code, out := run(t, db, "add", "http://192.0.2.1:6969/announce", "good.example.com")
	if code != 0 {
		t.Fatalf("add = %d: %s", code, out)
	}
	if !strings.Contains(out, "1 added") {
		t.Errorf("output = %q, want only the hostname added", out)
	}
}

func TestAddRequiresArguments(t *testing.T) {
	if code := Main([]string{"--db", tempDB(t), "add"}); code != 1 {
		t.Errorf("bare add = %d, want 1", code)
	}
	if code := Main([]string{"--db", tempDB(t), "rm"}); code != 1 {
		t.Errorf("bare rm = %d, want 1", code)
	}
}

func TestRemoveMissingTracker(t *testing.T) {
	// A missing name is reported on stderr but is not a fatal error, so a
	// batch removal keeps going.
	if code := Main([]string{"--db", tempDB(t), "rm", "nope.example.com"}); code != 0 {
		t.Errorf("rm missing = %d, want 0", code)
	}
}

func TestRemovePurge(t *testing.T) {
	db := tempDB(t)
	run(t, db, "add", "gone.example.com")
	if code, out := run(t, db, "rm", "--purge", "gone.example.com"); code != 0 {
		t.Fatalf("purge = %d: %s", code, out)
	}
	_, out := run(t, db, "list", "--all", "--names")
	if strings.Contains(out, "gone.example.com") {
		t.Errorf("purged tracker survived:\n%s", out)
	}
}

func TestImportFile(t *testing.T) {
	db := tempDB(t)
	list := filepath.Join(t.TempDir(), "list.txt")
	body := strings.Join([]string{
		"udp://tracker.example.com:1337/announce",
		"http://tracker.example.com:6969/announce", // same host, deduplicated
		"https://other.example.org/announce",
		"http://192.0.2.1:80/announce", // IP literal, skipped
		"", "# comment",
	}, "\n")
	if err := os.WriteFile(list, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := run(t, db, "import", "--file", list)
	if code != 0 {
		t.Fatalf("import = %d: %s", code, out)
	}
	if !strings.Contains(out, "parsed 2 unique hostnames") {
		t.Errorf("output = %q, want 2 unique hostnames", out)
	}
	if !strings.Contains(out, "2 new") {
		t.Errorf("output = %q, want 2 new", out)
	}

	// Re-importing adds nothing.
	_, out = run(t, db, "import", "--file", list)
	if !strings.Contains(out, "0 new, 2 already known") {
		t.Errorf("second import = %q", out)
	}
}

// Existing deployments need endpoints backfilled without their curation being
// undone: a plain import re-enables every name it names, which would resurrect
// the trackers that were removed for being dead or parked.
func TestImportEndpointsOnlyLeavesTheRegistryAlone(t *testing.T) {
	db := tempDB(t)
	list := filepath.Join(t.TempDir(), "list.txt")
	body := strings.Join([]string{
		"udp://kept.example.com:6969/announce",
		"https://kept.example.com/announce",
		"udp://removed.example.com:6969/announce",
		"udp://neverseen.example.com:6969/announce",
	}, "\n")
	if err := os.WriteFile(list, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bare hostnames carry no endpoint, which is the state an older database
	// is in before this backfill.
	run(t, db, "add", "kept.example.com", "removed.example.com")
	run(t, db, "rm", "removed.example.com")

	code, out := run(t, db, "import", "--file", list, "--endpoints-only")
	if code != 0 {
		t.Fatalf("import = %d: %s", code, out)
	}
	if !strings.Contains(out, "1 of 3 hostnames are not in the registry") {
		t.Errorf("output = %q, want the unknown hostname reported", out)
	}

	_, names := run(t, db, "list", "--names")
	if strings.Contains(names, "removed.example.com") {
		t.Errorf("a removed tracker was brought back:\n%s", names)
	}
	if strings.Contains(names, "neverseen.example.com") {
		t.Errorf("a hostname absent from the registry was added:\n%s", names)
	}
	if !strings.Contains(names, "kept.example.com") {
		t.Errorf("the surviving tracker went missing:\n%s", names)
	}

	// The point of the exercise: the kept name can now be probed.
	_, probe := run(t, db, "probe", "--probe-timeout", "10ms")
	if !strings.Contains(probe, "on 2 endpoints") {
		t.Errorf("probe = %q, want both of the kept name's endpoints attached", probe)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	db := tempDB(t)
	list := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(list, []byte("udp://a.example.com:80/announce\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, out := run(t, db, "import", "--file", list, "--dry-run"); code != 0 {
		t.Fatalf("dry run = %d: %s", code, out)
	}
	_, out := run(t, db, "list", "--names")
	if strings.Contains(out, "a.example.com") {
		t.Errorf("dry run wrote to the database:\n%s", out)
	}
}

func TestImportRequiresExactlyOneSource(t *testing.T) {
	db := tempDB(t)
	if code := Main([]string{"--db", db, "import"}); code != 1 {
		t.Errorf("no source = %d, want 1", code)
	}
	if code := Main([]string{"--db", db, "import", "--file", "x", "--url", "y"}); code != 1 {
		t.Errorf("both sources = %d, want 1", code)
	}
}

func TestImportMissingFile(t *testing.T) {
	db := tempDB(t)
	missing := filepath.Join(t.TempDir(), "absent.txt")
	if code := Main([]string{"--db", db, "import", "--file", missing}); code != 1 {
		t.Errorf("missing file = %d, want 1", code)
	}
}

func TestListJSON(t *testing.T) {
	db := tempDB(t)
	run(t, db, "add", "a.example.com")

	code, out := run(t, db, "list", "--json")
	if code != 0 {
		t.Fatalf("list --json = %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Errorf("output is not a JSON array:\n%s", out)
	}
	if !strings.Contains(out, `"name": "a.example.com"`) {
		t.Errorf("JSON missing the tracker:\n%s", out)
	}
}

func TestListTableIncludesStatus(t *testing.T) {
	db := tempDB(t)
	run(t, db, "add", "a.example.com")

	code, out := run(t, db, "list")
	if code != 0 {
		t.Fatalf("list = %d", code)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "STATUS") {
		t.Errorf("table header missing:\n%s", out)
	}
	if !strings.Contains(out, "never") {
		t.Errorf("an unchecked tracker should show 'never':\n%s", out)
	}
	if !strings.Contains(out, "1 trackers") {
		t.Errorf("count line missing:\n%s", out)
	}
}

func TestChangesOutput(t *testing.T) {
	db := tempDB(t)
	run(t, db, "add", "a.example.com")

	code, out := run(t, db, "changes")
	if code != 0 {
		t.Fatalf("changes = %d", code)
	}
	// Adding a tracker is itself a recorded change.
	if !strings.Contains(out, "* a.example.com added") {
		t.Errorf("changes output = %q", out)
	}

	code, out = run(t, db, "changes", "--json")
	if code != 0 || !strings.Contains(out, `"type": "tracker_added"`) {
		t.Errorf("changes --json = %d: %s", code, out)
	}
}

func TestChangesEmpty(t *testing.T) {
	code, out := run(t, tempDB(t), "changes")
	if code != 0 {
		t.Fatalf("changes = %d", code)
	}
	if !strings.Contains(out, "no changes recorded") {
		t.Errorf("output = %q", out)
	}
}

func TestSources(t *testing.T) {
	code, out := run(t, tempDB(t), "sources")
	if code != 0 {
		t.Fatalf("sources = %d", code)
	}
	for _, want := range []string{"ngosang", "xiu2", "newtrackon", "https://"} {
		if !strings.Contains(out, want) {
			t.Errorf("sources output is missing %q:\n%s", want, out)
		}
	}
}

func TestPollWithNoTrackers(t *testing.T) {
	// With an empty registry the resolver is never consulted, so this stays
	// offline and still exercises the command wiring.
	code, out := run(t, tempDB(t), "poll", "--resolver", "127.0.0.1:1")
	if code != 0 {
		t.Fatalf("poll = %d: %s", code, out)
	}
	if !strings.Contains(out, "0 trackers") {
		t.Errorf("poll output = %q", out)
	}
}

func TestDescribe(t *testing.T) {
	tests := []struct {
		name string
		in   store.Change
		want string
	}{
		{
			"added",
			store.Change{Type: store.ChangeIPAdded, Tracker: "a.example.com", IP: "1.2.3.4"},
			"+ a.example.com 1.2.3.4",
		},
		{
			"removed",
			store.Change{Type: store.ChangeIPRemoved, Tracker: "a.example.com", IP: "1.2.3.4"},
			"- a.example.com 1.2.3.4",
		},
		{
			"status",
			store.Change{Type: store.ChangeStatusChanged, Tracker: "a.example.com", Detail: "ok -> nxdomain"},
			"! a.example.com ok -> nxdomain",
		},
		{
			"tracker added",
			store.Change{Type: store.ChangeTrackerAdded, Tracker: "a.example.com", Detail: "list.txt"},
			"* a.example.com added (list.txt)",
		},
		{
			"unknown type",
			store.Change{Type: "weird", Tracker: "a.example.com", Detail: "x"},
			"? a.example.com weird x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describe(tt.in); got != tt.want {
				t.Errorf("describe() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgo(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		in   *time.Time
		want string
	}{
		{"never", nil, "never"},
		{"just now", ptr(now.Add(-10 * time.Second)), "just now"},
		{"minutes", ptr(now.Add(-30 * time.Minute)), "30m ago"},
		{"hours", ptr(now.Add(-5 * time.Hour)), "5h ago"},
		{"days", ptr(now.Add(-72 * time.Hour)), "3d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ago(tt.in); got != tt.want {
				t.Errorf("ago() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinOrDash(t *testing.T) {
	if got := joinOrDash(nil); got != "-" {
		t.Errorf("joinOrDash(nil) = %q, want -", got)
	}
	if got := joinOrDash([]string{"1.2.3.4", "5.6.7.8"}); got != "1.2.3.4,5.6.7.8" {
		t.Errorf("joinOrDash() = %q", got)
	}
}

func TestDefaultDBHonoursEnv(t *testing.T) {
	t.Setenv("TRACKERD_DB", "/tmp/custom.db")
	if got := defaultDB(); got != "/tmp/custom.db" {
		t.Errorf("defaultDB() = %q, want the env override", got)
	}
	t.Setenv("TRACKERD_DB", "")
	if got := defaultDB(); got != "trackers.db" {
		t.Errorf("defaultDB() = %q, want trackers.db", got)
	}
}

func TestEveryCommandIsRegistered(t *testing.T) {
	for _, name := range []string{"serve", "poll", "list", "add", "rm", "import", "changes", "sources"} {
		if _, ok := commands[name]; !ok {
			t.Errorf("command %q is documented but not registered", name)
		}
		if !strings.Contains(usage, name) {
			t.Errorf("command %q is registered but not in the usage text", name)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// A parked name is one resolving only to addresses a control name also
// resolves to. The collector sets the flag; the CLI is where the operator acts
// on it, listing them first and disabling once the list looks right.
func TestParkedListsAndDisables(t *testing.T) {
	db := tempDB(t)
	if code, _ := run(t, db, "add", "parked.example", "alive.example"); code != 0 {
		t.Fatalf("add exited %d", code)
	}
	markParked(t, db, "parked.example")

	code, out := run(t, db, "parked")
	if code != 0 {
		t.Fatalf("parked exited %d", code)
	}
	if !strings.Contains(out, "parked.example") {
		t.Errorf("listing did not mention the parked name:\n%s", out)
	}
	if strings.Contains(out, "alive.example") {
		t.Errorf("listing included a live tracker:\n%s", out)
	}

	// Listing alone must not disable anything.
	if _, out := run(t, db, "list"); !strings.Contains(out, "parked.example") {
		t.Errorf("listing removed the tracker by itself:\n%s", out)
	}

	if code, _ := run(t, db, "parked", "--disable"); code != 0 {
		t.Fatalf("parked --disable exited %d", code)
	}
	_, out = run(t, db, "list")
	if strings.Contains(out, "parked.example") {
		t.Errorf("parked tracker still enabled after --disable:\n%s", out)
	}
	if !strings.Contains(out, "alive.example") {
		t.Errorf("--disable took the live tracker with it:\n%s", out)
	}
}

// markParked does what a collection pass would do on seeing a name answer with
// nothing but parking addresses.
func markParked(t *testing.T, db, name string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tr, err := st.TrackerByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetParked(t.Context(), tr.ID, true, "parking", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestControlMarksAndUnmarks(t *testing.T) {
	db := tempDB(t)
	if code, _ := run(t, db, "add", "canary.example"); code != 0 {
		t.Fatalf("add exited %d", code)
	}

	if code, _ := run(t, db, "control", "canary.example"); code != 0 {
		t.Fatalf("control exited %d", code)
	}
	if _, out := run(t, db, "control"); !strings.Contains(out, "canary.example") {
		t.Errorf("control listing = %q, want the canary", out)
	}
	// A control name is resolved but is not a tracker, so it drops out here.
	if _, out := run(t, db, "list"); strings.Contains(out, "canary.example") {
		t.Errorf("control name still listed as a tracker:\n%s", out)
	}

	if code, _ := run(t, db, "control", "--unset", "canary.example"); code != 0 {
		t.Fatalf("control --unset exited %d", code)
	}
	if _, out := run(t, db, "list"); !strings.Contains(out, "canary.example") {
		t.Errorf("name did not come back as a tracker after --unset:\n%s", out)
	}
}
