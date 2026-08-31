package api

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/pawal/torrent-tracker/internal/store"
)

const (
	repoURL    = "https://github.com/pawal/torrent-tracker"
	licenseURL = "https://opensource.org/license/bsd-2-clause"
)

// jsonLD is the structured data for a page. Marshalling escapes '<', so the
// result is safe in a script tag.
func (s *Server) jsonLD(ctx context.Context, base, path, country string, t *store.Tracker) []byte {
	var nodes []any

	switch {
	case path == pathDashboard:
		nodes = append(nodes, website(base), s.dataset(ctx, base))

	case path == pathTrackers:
		loc := base + pathTrackers + canonicalQuery(country)
		title, desc := pageMeta(path, country, nil)
		nodes = append(nodes,
			page("CollectionPage", loc, title, desc),
			crumbs(base, crumb{"Trackers", loc}))

	case path == pathNetworks:
		title, desc := pageMeta(path, "", nil)
		nodes = append(nodes,
			page("CollectionPage", base+pathNetworks, title, desc),
			crumbs(base, crumb{"Networks", base + pathNetworks}))

	case path == pathLists:
		title, desc := pageMeta(path, "", nil)
		nodes = append(nodes,
			page("CollectionPage", base+pathLists, title, desc),
			crumbs(base, crumb{"Lists", base + pathLists}))

	case t != nil:
		loc := base + trackerPrefix + url.PathEscape(t.Name)
		nodes = append(nodes,
			trackerDataset(base, loc, *t),
			crumbs(base, crumb{"Trackers", base + pathTrackers}, crumb{t.Name, loc}))

	default:
		return nil
	}

	b, err := json.Marshal(map[string]any{
		"@context": "https://schema.org",
		"@graph":   nodes,
	})
	if err != nil {
		s.logger().Error("encode json-ld", "err", err)
		return nil
	}
	return b
}

func website(base string) map[string]any {
	return map[string]any{
		"@type": "WebSite",
		"@id":   base + "/#website",
		"name":  "torrent-tracker",
		"url":   base + "/",
	}
}

// dataset describes the collection as a whole; the lists and the API are its
// distributions.
func (s *Server) dataset(ctx context.Context, base string) map[string]any {
	d := map[string]any{
		"@type":               "Dataset",
		"@id":                 base + "/#dataset",
		"name":                "BitTorrent tracker DNS and reachability history",
		"description":         defaultDesc,
		"url":                 base + "/",
		"license":             licenseURL,
		"isAccessibleForFree": true,
		"creator": map[string]any{
			"@type": "Person",
			"name":  "Patrik Wallström",
			"url":   repoURL,
		},
		"keywords": []string{
			"BitTorrent", "tracker", "DNS", "uptime", "BEP 15", "BEP 34", "BEP 48",
		},
		"distribution": []any{
			download("application/json", base+"/api/trackers"),
			download("application/json", base+"/api/changes"),
			download("text/plain", base+"/api/list"),
		},
	}
	// Open-ended: collection is still running.
	if from, err := s.Store.EarliestTracker(ctx); err != nil {
		s.logger().Error("earliest tracker", "err", err)
	} else if !from.IsZero() {
		d["temporalCoverage"] = isoDate(from) + "/.."
	}
	return d
}

func trackerDataset(base, loc string, t store.Tracker) map[string]any {
	_, desc := pageMeta(trackerPrefix+t.Name, "", &t)
	return map[string]any{
		"@type":               "Dataset",
		"name":                t.Name + " — address and reachability history",
		"description":         desc,
		"url":                 loc,
		"license":             licenseURL,
		"isAccessibleForFree": true,
		"isPartOf":            map[string]any{"@id": base + "/#dataset"},
		"temporalCoverage":    isoDate(t.CreatedAt) + "/..",
		"distribution": []any{
			download("application/json", base+"/api/trackers/"+url.PathEscape(t.Name)),
		},
	}
}

func download(format, contentURL string) map[string]any {
	return map[string]any{
		"@type":          "DataDownload",
		"encodingFormat": format,
		"contentUrl":     contentURL,
	}
}

func page(kind, loc, name, desc string) map[string]any {
	return map[string]any{
		"@type":       kind,
		"url":         loc,
		"name":        name,
		"description": desc,
	}
}

type crumb struct {
	name string
	url  string
}

// crumbs builds the trail, always rooted at the change feed.
func crumbs(base string, rest ...crumb) map[string]any {
	all := append([]crumb{{"Changes", base + "/"}}, rest...)
	items := make([]any, 0, len(all))
	for i, c := range all {
		items = append(items, map[string]any{
			"@type":    "ListItem",
			"position": i + 1,
			"name":     c.name,
			"item":     c.url,
		})
	}
	return map[string]any{"@type": "BreadcrumbList", "itemListElement": items}
}
