package api

import (
	"html"
	"strings"

	"github.com/pawal/torrent-tracker/internal/store"
)

// The placeholders web/index.html ships with. Every page's own metadata is
// substituted for them before the shell goes out.
const (
	defaultTitle = "torrent-tracker — BitTorrent tracker DNS history"
	defaultDesc  = "Which BitTorrent trackers still answer, and where they live. " +
		"Hourly DNS collection, BEP 15 and BEP 48 probes, and an append-only feed of every change."
	defaultURL   = "https://tracker.evilbit.de/"
	defaultImage = "https://tracker.evilbit.de/og-image.png"
	robotsIndex  = `content="index, follow"`
	robotsNone   = `content="noindex, follow"`
)

// canonicalTag is dropped whole on a page that is not to be indexed: naming a
// canonical URL for a 404 contradicts the noindex beside it.
const canonicalTag = `<link rel="canonical" href="` + defaultURL + `" />`

// head is the metadata one page carries.
type head struct {
	Title       string
	Description string
	URL         string
	Image       string
	NoIndex     bool
	// LD is the page's JSON-LD, or nil for none.
	LD []byte
	// Body is the rendered page for clients that run no JS, or nil for none.
	Body []byte
}

// pageMeta is the title and description for a page. It mirrors pageMeta in
// web/src/lib/meta.js, which keeps them current as the client navigates.
func pageMeta(path, country string, t *store.Tracker) (title, desc string) {
	switch {
	case path == pathDashboard:
		return defaultTitle, defaultDesc

	case path == pathTrackers && country == "unknown":
		return "Trackers with no country on record — torrent-tracker",
			"BitTorrent trackers whose addresses have no country on record, with " +
				"their DNS status, reachability and origin networks."

	case path == pathTrackers && country != "":
		return "Trackers in " + country + " — torrent-tracker",
			"BitTorrent trackers with an address in " + country + ", with their " +
				"DNS status, reachability and origin networks."

	case path == pathTrackers:
		return "Known trackers — torrent-tracker",
			"Every tracked BitTorrent tracker: DNS status, whether it answers, origin " +
				"AS, country and the addresses it resolves to."

	case path == pathLists:
		return "Announce lists — torrent-tracker",
			"BitTorrent announce URLs worth pasting into a client, filtered by measured " +
				"uptime, by transport and by how many trackers share one origin AS."

	case path == pathNetworks:
		return "Networks — torrent-tracker",
			"Where the tracked BitTorrent trackers are hosted: origin AS, RIR, country " +
				"and the tracker software behind each endpoint."

	case t != nil:
		return t.Name + " — torrent-tracker",
			t.Name + " " + trackerState(*t) + ". Address history, reachability and DNS " +
				"status, collected hourly and probed every six hours."
	}
	return "Not found — torrent-tracker", "No page at this address."
}

// trackerState is the clause a detail page's description opens with. Mirrors
// trackerState in web/src/lib/meta.js.
func trackerState(t store.Tracker) string {
	switch {
	case t.BEP34Denies:
		return "publishes a BEP 34 record naming no tracker and is no longer probed"
	case t.Parked:
		return "resolves only to parking addresses"
	case t.Reach == store.ReachLive:
		return "resolves and answers the tracker protocol"
	case t.Reach == store.ReachPartial:
		return "answers on some of its addresses"
	case t.LastStatus != "" && t.LastStatus != store.StatusOK:
		return "does not resolve (" + string(t.LastStatus) + ")"
	case t.Reach == store.ReachDead:
		return "resolves but answers nothing"
	}
	return "has not been probed yet"
}

// renderShell substitutes a page's metadata into index.html.
func renderShell(shell []byte, h head) []byte {
	esc := html.EscapeString
	src := string(shell)
	if h.NoIndex {
		src = strings.Replace(src, canonicalTag, "", 1)
	}
	// The image URL is replaced first: the page URL is a prefix of it.
	out := strings.NewReplacer(
		defaultImage, esc(h.Image),
		defaultURL, esc(h.URL),
		defaultTitle, esc(h.Title),
		defaultDesc, esc(h.Description),
	).Replace(src)

	if h.NoIndex {
		out = strings.Replace(out, robotsIndex, robotsNone, 1)
	}
	if len(h.LD) > 0 {
		out = strings.Replace(out, headEnd,
			`<script type="application/ld+json" id="ld-json">`+string(h.LD)+"</script>\n"+headEnd, 1)
	}
	if len(h.Body) > 0 {
		out = strings.Replace(out, bodyEnd, string(h.Body)+bodyEnd, 1)
	}
	return []byte(out)
}

// Where the JSON-LD and the no-JS body go.
const (
	headEnd = "</head>"
	bodyEnd = "</body>"
)
