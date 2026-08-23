package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	mustAdd(t, s1, "a.example.com")
	s1.Close()

	// Re-opening must reapply no migrations and keep the data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	got, err := s2.ListTrackers(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a.example.com" {
		t.Errorf("after reopen got %v, want the original tracker", got)
	}
}

func TestInMemoryStoresAreIsolated(t *testing.T) {
	a, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	mustAdd(t, a, "only-in-a.example.com")

	got, err := b.ListTrackers(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("second in-memory store saw %v, want an empty database", got)
	}
}

func TestAddTracker(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	tr, created, err := s.AddTracker(ctx, "a.example.com", "list.txt", base)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("first add should report created")
	}
	if tr.Source != "list.txt" || !tr.Enabled {
		t.Errorf("got %+v, want an enabled tracker sourced from list.txt", tr)
	}

	// Adding again is a no-op.
	_, created, err = s.AddTracker(ctx, "a.example.com", "other", base)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second add should not report created")
	}

	// One tracker_added change, not two.
	changes, err := s.RecentChanges(ctx, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Type != ChangeTrackerAdded {
		t.Errorf("changes = %+v, want exactly one tracker_added", changes)
	}
}

func TestRemoveTrackerKeepsHistory(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	plan := Plan{Status: StatusOK, StatusChanged: true, Actions: []Action{
		{IP: "1.2.3.4", Family: 4, Kind: ActionAdd},
	}}
	if err := s.ApplyPlan(ctx, tr.ID, plan, base); err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveTracker(ctx, "a.example.com", false); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.ListTrackers(ctx, false); len(got) != 0 {
		t.Errorf("removed tracker still listed: %v", got)
	}
	all, _ := s.ListTrackers(ctx, true)
	if len(all) != 1 || all[0].Enabled {
		t.Errorf("with includeDisabled got %v, want one disabled tracker", all)
	}
	records, _ := s.RecordsFor(ctx, tr.ID)
	if len(records) != 1 {
		t.Errorf("history lost on soft remove: %v", records)
	}

	// Re-adding revives it without duplicating the row or losing history.
	revived, created, err := s.AddTracker(ctx, "a.example.com", "test", base)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("reviving should not report created")
	}
	if revived.ID != tr.ID || !revived.Enabled {
		t.Errorf("revived = %+v, want the original id, enabled", revived)
	}
	if records, _ := s.RecordsFor(ctx, tr.ID); len(records) != 1 {
		t.Error("history lost when reviving")
	}
}

func TestRemoveTrackerPurge(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	plan := Plan{Status: StatusOK, Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionAdd}}}
	if err := s.ApplyPlan(ctx, tr.ID, plan, base); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTracker(ctx, "a.example.com", true); err != nil {
		t.Fatal(err)
	}

	all, _ := s.ListTrackers(ctx, true)
	if len(all) != 0 {
		t.Errorf("purged tracker still present: %v", all)
	}
	// ON DELETE CASCADE must take the history with it.
	records, _ := s.RecordsFor(ctx, tr.ID)
	if len(records) != 0 {
		t.Errorf("purge left %d orphaned ip_records", len(records))
	}
	changes, _ := s.RecentChanges(ctx, time.Time{}, 100)
	if len(changes) != 0 {
		t.Errorf("purge left %d orphaned changes", len(changes))
	}
}

// fails records one pass that produced no address.
func fails(t *testing.T, s *Store, trackerID int64, at time.Time, status Status) {
	t.Helper()
	must(t, s.ApplyPlan(t.Context(), trackerID, Plan{Status: status}, at))
}

func TestApplyPlanTracksFailingStreak(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	fails(t, s, tr.ID, base, StatusNXDomain)
	fails(t, s, tr.ID, base.Add(time.Hour), StatusServFail)

	got, err := s.TrackerByName(ctx, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResolveFails != 2 {
		t.Errorf("resolve_fails = %d, want 2", got.ResolveFails)
	}
	// The streak is dated from its first failure, not its latest.
	if got.FailingSince == nil || !got.FailingSince.Equal(base) {
		t.Errorf("failing_since = %v, want %v", got.FailingSince, base)
	}

	apply(t, s, tr.ID, base.Add(2*time.Hour), adds("1.2.3.4")...)
	if got, _ = s.TrackerByName(ctx, "a.example.com"); got.ResolveFails != 0 || got.FailingSince != nil {
		t.Errorf("after resolving: fails=%d since=%v, want 0 and nil", got.ResolveFails, got.FailingSince)
	}
}

func TestRetireStale(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	month := 30 * 24 * time.Hour

	never := mustAdd(t, s, "never.example.com")
	young := mustAdd(t, s, "young.example.com")
	quiet := mustAdd(t, s, "quiet.example.com")

	fails(t, s, never.ID, base, StatusNXDomain)
	fails(t, s, young.ID, base.Add(20*24*time.Hour), StatusNXDomain)
	// Resolved once, then went quiet: a different fact, not a retirement.
	apply(t, s, quiet.ID, base, adds("1.2.3.4")...)
	fails(t, s, quiet.ID, base.Add(time.Hour), StatusNXDomain)

	now := base.Add(month + time.Hour)
	retired, err := s.RetireStale(ctx, now.Add(-month), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0].Name != "never.example.com" {
		t.Fatalf("retired = %+v, want only never.example.com", retired)
	}

	left, _ := s.ListTrackers(ctx, false)
	if len(left) != 2 {
		t.Errorf("%d trackers still collected, want young and quiet", len(left))
	}
	if all, _ := s.ListTrackers(ctx, true); len(all) != 3 {
		t.Errorf("retirement lost a row: %d of 3 left", len(all))
	}
	if got := countKind(t, s, never.ID, ChangeTrackerRetired); got != 1 {
		t.Errorf("%d tracker_retired entries, want 1", got)
	}

	// Nothing is left to retire on the next pass.
	again, err := s.RetireStale(ctx, now.Add(-month), now)
	if err != nil || len(again) != 0 {
		t.Errorf("second pass retired %+v (err %v), want nothing", again, err)
	}
}

func TestReAddingClearsTheFailingStreak(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")
	fails(t, s, tr.ID, base, StatusNXDomain)
	must(t, s.RemoveTracker(ctx, "a.example.com", false))

	// A re-imported name starts its month over rather than being retired again
	// on the next pass.
	if _, _, err := s.AddTracker(ctx, "a.example.com", "list.txt", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.TrackerByName(ctx, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResolveFails != 0 || got.FailingSince != nil {
		t.Errorf("revived with fails=%d since=%v, want 0 and nil", got.ResolveFails, got.FailingSince)
	}
}

func TestRemoveMissingTracker(t *testing.T) {
	s := testStore(t)
	err := s.RemoveTracker(t.Context(), "nope.example.com", false)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestTrackerByName(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	mustAdd(t, s, "a.example.com")

	got, err := s.TrackerByName(ctx, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a.example.com" {
		t.Errorf("got %q", got.Name)
	}
	if _, err := s.TrackerByName(ctx, "missing.example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The full interval lifecycle: an address appears, persists, is missed, is
// retired, and later comes back as a fresh interval.
func TestApplyPlanIntervalLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	t1 := base
	t2 := base.Add(time.Hour)
	t3 := base.Add(2 * time.Hour)
	t4 := base.Add(3 * time.Hour)
	t5 := base.Add(4 * time.Hour)

	// 1. First sighting.
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status: StatusOK, StatusChanged: true,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionAdd}},
	}, t1))

	rec := activeOne(t, s, tr.ID)
	if !rec.FirstSeen.Equal(t1) || !rec.LastSeen.Equal(t1) {
		t.Errorf("after add: first=%v last=%v, want both %v", rec.FirstSeen, rec.LastSeen, t1)
	}

	// 2. Still there an hour later: last_seen advances, first_seen does not.
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status:  StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionRefresh}},
	}, t2))

	rec = activeOne(t, s, tr.ID)
	if !rec.FirstSeen.Equal(t1) {
		t.Errorf("first_seen moved to %v, want it pinned at %v", rec.FirstSeen, t1)
	}
	if !rec.LastSeen.Equal(t2) {
		t.Errorf("last_seen = %v, want %v", rec.LastSeen, t2)
	}

	// 3. Absent once: counter ticks, interval stays open.
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status:  StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionMiss}},
	}, t3))

	rec = activeOne(t, s, tr.ID)
	if rec.MissCount != 1 {
		t.Errorf("miss_count = %d, want 1", rec.MissCount)
	}
	if !rec.LastSeen.Equal(t2) {
		t.Errorf("a miss advanced last_seen to %v; it must stay at %v", rec.LastSeen, t2)
	}

	// 4. Retired.
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status:  StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionRemove}},
	}, t4))

	if got, _ := s.ActiveRecords(ctx, tr.ID); len(got) != 0 {
		t.Errorf("address still active after removal: %v", got)
	}
	all, _ := s.RecordsFor(ctx, tr.ID)
	if len(all) != 1 || all[0].Active {
		t.Fatalf("records = %+v, want one closed interval", all)
	}

	// 5. It comes back: a new interval, not a resurrection of the old one.
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status:  StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionAdd}},
	}, t5))

	all, _ = s.RecordsFor(ctx, tr.ID)
	if len(all) != 2 {
		t.Fatalf("got %d intervals, want 2 (the old closed one plus a new one)", len(all))
	}
	rec = activeOne(t, s, tr.ID)
	if !rec.FirstSeen.Equal(t5) {
		t.Errorf("new interval first_seen = %v, want %v", rec.FirstSeen, t5)
	}
	if rec.MissCount != 0 {
		t.Errorf("new interval carries miss_count %d, want 0", rec.MissCount)
	}
}

// A refresh must clear the miss counter, or intermittent absences accumulate
// until a perfectly healthy address is retired.
func TestApplyPlanRefreshClearsMisses(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	must(t, s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionAdd}}}, base))
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionMiss}}}, base.Add(time.Hour)))
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionRefresh}}}, base.Add(2*time.Hour)))

	if got := activeOne(t, s, tr.ID).MissCount; got != 0 {
		t.Errorf("miss_count = %d after a refresh, want 0", got)
	}
}

// A silent add opens its interval like any other, but the feed hears only the
// mode change that caused it.
func TestApplyPlanSilentAddSkipsTheFeed(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status: StatusOK,
		Actions: []Action{
			{IP: "2600:9000:2094::/48", Family: 6, Kind: ActionAdd, Prefix: true, Silent: true},
		},
		States: []FamilyState{{Family: 6, Rolling: true, ModeChanged: true}},
	}, base))

	if got, _ := s.ActiveRecords(ctx, tr.ID); len(got) != 1 || !got[0].IsPrefix {
		t.Errorf("active records = %+v, want the prefix recorded", got)
	}
	changes, err := s.ChangesFor(ctx, tr.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	// tracker_added, then ips_rolling — no prefix_added.
	if len(changes) != 2 || changes[0].Type != ChangeIPsRolling {
		t.Errorf("changes = %+v, want tracker_added and ips_rolling only", changes)
	}
}

func TestApplyPlanWritesChangeFeed(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status: StatusOK, PrevStatus: "", StatusChanged: true,
		Actions: []Action{
			{IP: "1.2.3.4", Family: 4, Kind: ActionAdd},
			{IP: "2001:db8::1", Family: 6, Kind: ActionAdd},
		},
	}, base))

	changes, err := s.ChangesFor(ctx, tr.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	// tracker_added, status_changed, two ip_added.
	if len(changes) != 4 {
		t.Fatalf("got %d changes, want 4: %+v", len(changes), changes)
	}

	counts := map[string]int{}
	for _, c := range changes {
		counts[c.Type]++
		if c.Tracker != "a.example.com" {
			t.Errorf("change is missing the joined tracker name: %+v", c)
		}
	}
	if counts[ChangeIPAdded] != 2 || counts[ChangeStatusChanged] != 1 || counts[ChangeTrackerAdded] != 1 {
		t.Errorf("change types = %v", counts)
	}

	// Misses stay out of the feed entirely.
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionMiss}}}, base.Add(time.Hour)))
	after, _ := s.ChangesFor(ctx, tr.ID, 100)
	if len(after) != 4 {
		t.Errorf("a miss leaked into the change feed: %d entries", len(after))
	}
}

func TestApplyPlanStatusDetail(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	must(t, s.ApplyPlan(ctx, tr.ID, Plan{
		Status: StatusNXDomain, PrevStatus: StatusOK, StatusChanged: true,
	}, base))

	changes, _ := s.ChangesFor(ctx, tr.ID, 10)
	var detail string
	for _, c := range changes {
		if c.Type == ChangeStatusChanged {
			detail = c.Detail
		}
	}
	if detail != "ok -> nxdomain" {
		t.Errorf("detail = %q, want %q", detail, "ok -> nxdomain")
	}

	// An unchecked tracker should read as "unchecked", not an empty string.
	tr2 := mustAdd(t, s, "b.example.com")
	must(t, s.ApplyPlan(ctx, tr2.ID, Plan{Status: StatusOK, PrevStatus: "", StatusChanged: true}, base))
	changes, _ = s.ChangesFor(ctx, tr2.ID, 10)
	for _, c := range changes {
		if c.Type == ChangeStatusChanged && c.Detail != "unchecked -> ok" {
			t.Errorf("detail = %q, want %q", c.Detail, "unchecked -> ok")
		}
	}
}

func TestApplyPlanUpdatesTrackerStatus(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	must(t, s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusServFail, StatusChanged: true}, base))

	got, err := s.TrackerByName(ctx, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatus != StatusServFail {
		t.Errorf("last_status = %q, want servfail", got.LastStatus)
	}
	if got.LastCheckedAt == nil || !got.LastCheckedAt.Equal(base) {
		t.Errorf("last_checked_at = %v, want %v", got.LastCheckedAt, base)
	}
}

func TestApplyPlanRejectsUnknownAction(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	err := s.ApplyPlan(ctx, tr.ID, Plan{
		Status:  StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionKind("bogus")}},
	}, base)
	if err == nil {
		t.Fatal("want an error for an unknown action kind")
	}
	// The transaction must have rolled back cleanly.
	if got, _ := s.RecordsFor(ctx, tr.ID); len(got) != 0 {
		t.Errorf("failed plan left %d records behind", len(got))
	}
}

func TestListTrackerViews(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	a := mustAdd(t, s, "a.example.com")
	mustAdd(t, s, "b.example.com")

	apply(t, s, a.ID, base, adds("1.2.3.4", "5.6.7.8", "2001:db8::1")...)

	views, err := s.ListTrackerViews(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	if views[0].Name != "a.example.com" {
		t.Errorf("views are not sorted by name: %q first", views[0].Name)
	}
	if len(views[0].IPv4) != 2 || len(views[0].IPv6) != 1 {
		t.Errorf("a: ipv4=%v ipv6=%v, want 2 and 1", views[0].IPv4, views[0].IPv6)
	}
	// A tracker with no addresses must serialise as [] rather than null.
	if views[1].IPv4 == nil || views[1].IPv6 == nil {
		t.Error("empty address lists must be non-nil slices")
	}

	// A retired address drops out of the view.
	apply(t, s, a.ID, base.Add(time.Hour),
		Action{IP: "1.2.3.4", Family: 4, Kind: ActionRemove})
	views, _ = s.ListTrackerViews(ctx, false)
	if len(views[0].IPv4) != 1 || views[0].IPv4[0] != "5.6.7.8" {
		t.Errorf("ipv4 = %v, want only 5.6.7.8", views[0].IPv4)
	}
}

func TestRecentChangesSinceAndLimit(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	for i := 0; i < 5; i++ {
		must(t, s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusOK, Actions: []Action{
			{IP: "1.2.3." + string(rune('0'+i)), Family: 4, Kind: ActionAdd},
		}}, base.Add(time.Duration(i)*time.Hour)))
	}

	all, err := s.RecentChanges(ctx, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 { // 5 additions + tracker_added
		t.Fatalf("got %d changes, want 6", len(all))
	}
	// Newest first.
	for i := 1; i < len(all); i++ {
		if all[i-1].ObservedAt.Before(all[i].ObservedAt) {
			t.Fatalf("changes are not in descending time order at %d", i)
		}
	}

	recent, err := s.RecentChanges(ctx, base.Add(3*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Errorf("since=+3h gave %d changes, want 2", len(recent))
	}

	limited, _ := s.RecentChanges(ctx, time.Time{}, 3)
	if len(limited) != 3 {
		t.Errorf("limit=3 gave %d changes", len(limited))
	}
}

func TestRuns(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	id, err := s.StartRun(ctx, base, 10)
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := s.RecentRuns(ctx, 10)
	if len(runs) != 1 || runs[0].FinishedAt != nil {
		t.Fatalf("runs = %+v, want one unfinished run", runs)
	}

	if err := s.FinishRun(ctx, id, base.Add(time.Minute), 8, 2, 5); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.RecentRuns(ctx, 10)
	r := runs[0]
	if r.FinishedAt == nil || r.OKCount != 8 || r.ErrorCount != 2 || r.ChangeCount != 5 || r.TrackerCount != 10 {
		t.Errorf("run = %+v, want the finished tallies", r)
	}
}

func TestStats(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	a := mustAdd(t, s, "a.example.com")
	b := mustAdd(t, s, "b.example.com")
	mustAdd(t, s, "c.example.com")
	d := mustAdd(t, s, "d.example.com")

	apply(t, s, a.ID, base, adds("1.2.3.4", "5.6.7.8")...)
	// The same address behind two trackers, the CDN shape issue 7 is about.
	apply(t, s, d.ID, base, adds("5.6.7.8")...)
	must(t, s.ApplyPlan(ctx, b.ID, Plan{Status: StatusNXDomain, StatusChanged: true}, base))
	// Retire one address so active and total diverge.
	apply(t, s, a.ID, base.Add(time.Hour),
		Action{IP: "1.2.3.4", Family: 4, Kind: ActionRemove})

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Trackers != 4 || st.EnabledTrackers != 4 {
		t.Errorf("trackers=%d enabled=%d, want 4 and 4", st.Trackers, st.EnabledTrackers)
	}
	if st.ActiveIPs != 1 {
		t.Errorf("active_ips = %d, want 1 distinct address", st.ActiveIPs)
	}
	if st.ActiveIPRecords != 2 {
		t.Errorf("active_ip_records = %d, want 2 tracker-address pairs", st.ActiveIPRecords)
	}
	if st.TotalIPs != 2 {
		t.Errorf("total_ips = %d, want 2", st.TotalIPs)
	}
	// The dashboard and the networks page must agree on "live now".
	cov, err := s.Coverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.ActiveIPs != st.ActiveIPs {
		t.Errorf("coverage active_ips = %d, stats says %d", cov.ActiveIPs, st.ActiveIPs)
	}
	if st.ByStatus[StatusOK] != 2 || st.ByStatus[StatusNXDomain] != 1 || st.ByStatus["unchecked"] != 1 {
		t.Errorf("by_status = %v, want two ok and one each of nxdomain/unchecked", st.ByStatus)
	}
}

func TestFamily(t *testing.T) {
	if got := Family("1.2.3.4"); got != 4 {
		t.Errorf("Family(v4) = %d", got)
	}
	if got := Family("2001:db8::1"); got != 6 {
		t.Errorf("Family(v6) = %d", got)
	}
}

func TestStatusResolved(t *testing.T) {
	resolved := []Status{StatusOK, StatusNoData, StatusNXDomain}
	unresolved := []Status{StatusServFail, StatusTimeout, StatusError, ""}
	for _, s := range resolved {
		if !s.Resolved() {
			t.Errorf("%q.Resolved() = false, want true", s)
		}
	}
	for _, s := range unresolved {
		if s.Resolved() {
			t.Errorf("%q.Resolved() = true, want false", s)
		}
	}
}

func TestPlanChanges(t *testing.T) {
	p := Plan{StatusChanged: true, Actions: []Action{
		{Kind: ActionAdd}, {Kind: ActionRemove}, {Kind: ActionRefresh}, {Kind: ActionMiss},
	}}
	if got := p.Changes(); got != 3 { // add + remove + status
		t.Errorf("Changes() = %d, want 3", got)
	}
}

// Concurrent writers must not fail with SQLITE_BUSY. Each of these
// transactions reads before it writes, which is exactly the pattern that
// deadlocks on lock upgrade unless transactions begin as immediate.
func TestConcurrentWritersDoNotDeadlock(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	// Give every address an initial row so PutIPInfo takes its read-then-write
	// path rather than a plain insert.
	ips := make([]string, 24)
	for i := range ips {
		ips[i] = fmt.Sprintf("192.0.2.%d", i+1)
		apply(t, s, tr.ID, base, adds(ips[i])...)
		must(t, s.PutIPInfo(ctx, IPInfo{IP: ips[i], Family: 4, ASN: 64500}, base))
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			err := s.PutIPInfo(ctx, IPInfo{
				IP: ip, Family: 4, ASN: 64501 + i, ASName: "changed",
			}, base.Add(time.Hour))
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i, ip)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d/%d concurrent writes failed, first: %v", len(errs), len(ips), errs[0])
	}

	// Every AS change should have produced a change-feed entry.
	changes, err := s.ChangesFor(ctx, tr.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	var asn int
	for _, c := range changes {
		if c.Type == ChangeASNChanged {
			asn++
		}
	}
	if asn != len(ips) {
		t.Errorf("got %d asn_changed events, want %d", asn, len(ips))
	}
}
