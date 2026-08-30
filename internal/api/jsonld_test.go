package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ldGraph pulls the nodes out of a served page, keyed by @type, so a test can
// ask what a page claims to be without walking the whole document.
func ldGraph(t *testing.T, body string) map[string]map[string]any {
	t.Helper()

	const open = `<script type="application/ld+json" id="ld-json">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("no JSON-LD on the page:\n%s", body)
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</script>")
	if j < 0 {
		t.Fatal("the JSON-LD block is not closed")
	}

	var doc struct {
		Context string           `json:"@context"`
		Graph   []map[string]any `json:"@graph"`
	}
	if err := json.Unmarshal([]byte(rest[:j]), &doc); err != nil {
		t.Fatalf("JSON-LD does not parse: %v\n%s", err, rest[:j])
	}
	if doc.Context != "https://schema.org" {
		t.Errorf("@context = %q", doc.Context)
	}

	out := map[string]map[string]any{}
	for _, n := range doc.Graph {
		kind, _ := n["@type"].(string)
		out[kind] = n
	}
	return out
}

// The dashboard is the entry point, and the thing being described is a public
// dataset with the API as its distribution.
func TestDashboardIsADataset(t *testing.T) {
	h, st := realServer(t)
	seed(t, st)

	nodes := ldGraph(t, get(t, h, "/").Body.String())
	if _, ok := nodes["WebSite"]; !ok {
		t.Error("the dashboard does not identify the site")
	}
	ds, ok := nodes["Dataset"]
	if !ok {
		t.Fatalf("no Dataset node, got %v", keys(nodes))
	}
	if ds["license"] == "" || ds["license"] == nil {
		t.Error("the dataset does not name a license")
	}
	if ds["isAccessibleForFree"] != true {
		t.Error("the dataset should be marked free")
	}
	// Structured data is worth little without somewhere to get the data.
	dist, ok := ds["distribution"].([]any)
	if !ok || len(dist) == 0 {
		t.Fatalf("the dataset has no distribution: %v", ds["distribution"])
	}
	for _, d := range dist {
		m := d.(map[string]any)
		if !strings.HasPrefix(m["contentUrl"].(string), "http://example.com/api/") {
			t.Errorf("distribution points outside the API: %v", m["contentUrl"])
		}
	}
	// The dataset starts when the oldest name was added, and has not ended.
	cov, _ := ds["temporalCoverage"].(string)
	if !strings.HasSuffix(cov, "/..") {
		t.Errorf("temporalCoverage = %q, want an open-ended range", cov)
	}
}

// A tracker page is its own dataset, part of the whole, with the endpoint that
// serves it named.
func TestTrackerPageIsADataset(t *testing.T) {
	h, st := realServer(t)
	seed(t, st)

	nodes := ldGraph(t, get(t, h, "/t/a.example.com").Body.String())
	ds, ok := nodes["Dataset"]
	if !ok {
		t.Fatalf("no Dataset node, got %v", keys(nodes))
	}
	if !strings.Contains(ds["name"].(string), "a.example.com") {
		t.Errorf("the dataset is not named after the tracker: %v", ds["name"])
	}
	if ds["url"] != "http://example.com/t/a.example.com" {
		t.Errorf("url = %v", ds["url"])
	}
	part, _ := ds["isPartOf"].(map[string]any)
	if part["@id"] != "http://example.com/#dataset" {
		t.Errorf("the tracker dataset is not part of the whole: %v", ds["isPartOf"])
	}

	// Breadcrumbs are what put a trail under the result in a search listing.
	bc, ok := nodes["BreadcrumbList"]
	if !ok {
		t.Fatal("the tracker page has no breadcrumbs")
	}
	items := bc["itemListElement"].([]any)
	if len(items) != 3 {
		t.Fatalf("got %d crumbs, want Changes > Trackers > the name", len(items))
	}
	if last := items[2].(map[string]any); last["name"] != "a.example.com" {
		t.Errorf("the trail does not end at the tracker: %v", last["name"])
	}
}

func TestListPagesAreCollections(t *testing.T) {
	h, st := realServer(t)
	seed(t, st)

	for _, path := range []string{"/trackers", "/networks"} {
		nodes := ldGraph(t, get(t, h, path).Body.String())
		if _, ok := nodes["CollectionPage"]; !ok {
			t.Errorf("%s is not a CollectionPage, got %v", path, keys(nodes))
		}
		if _, ok := nodes["BreadcrumbList"]; !ok {
			t.Errorf("%s has no breadcrumbs", path)
		}
	}
}

// A page that does not exist describes nothing, and must not offer a crawler
// structured data saying otherwise.
func TestNotFoundHasNoStructuredData(t *testing.T) {
	h, st := realServer(t)
	seed(t, st)

	for _, path := range []string{"/nope", "/t/ghost.example.com"} {
		if body := get(t, h, path).Body.String(); strings.Contains(body, "ld+json") {
			t.Errorf("GET %s carries structured data", path)
		}
	}
}

// A tracker name is data, and the JSON-LD block is the one place it reaches a
// script body. json.Marshal escapes '<', so it cannot close the tag early.
func TestJSONLDCannotEscapeItsScriptTag(t *testing.T) {
	h, st := realServer(t)
	const name = `<script>alert(1)<x.example.com`
	if _, _, err := st.AddTracker(t.Context(), name, "manual", base); err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/t/"+name).Body.String()
	if strings.Contains(body, "<script>alert(1)") {
		t.Errorf("a name reached the page unescaped:\n%s", body)
	}
	// The block still parses, so the escaping did not corrupt it either.
	nodes := ldGraph(t, body)
	if !strings.Contains(nodes["Dataset"]["name"].(string), name) {
		t.Errorf("the name did not survive the round trip: %v", nodes["Dataset"]["name"])
	}
}

// A name carrying a slash cannot address a page at all: it fails the route
// before anything is rendered.
func TestSlashInNameIsNotAPage(t *testing.T) {
	h, st := realServer(t)
	if _, _, err := st.AddTracker(t.Context(), "a/b.example.com", "manual", base); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, h, "/t/a/b.example.com"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /t/a/b.example.com = %d, want 404", rec.Code)
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
