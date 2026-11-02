package sitemap

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func urlset(urls ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range urls {
		b.WriteString("<url><loc>" + u + "</loc></url>")
	}
	b.WriteString("</urlset>")
	return b.String()
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte(s))
	w.Close()
	return buf.Bytes()
}

func serve(t *testing.T, routes func(base string) map[string][]byte) string {
	t.Helper()
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes(base)[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	return base
}

func TestFetchFollowsIndexAndGzip(t *testing.T) {
	base := serve(t, func(base string) map[string][]byte {
		return map[string][]byte{
			"/index.xml": []byte(`<sitemapindex><sitemap><loc>` + base + `/a.xml</loc></sitemap><sitemap><loc>` + base + `/b.xml.gz</loc></sitemap><sitemap><loc>` + base + `/a.xml</loc></sitemap></sitemapindex>`),
			"/a.xml":     []byte(urlset("https://e/1", "https://e/2")),
			"/b.xml.gz":  gz(t, urlset("https://e/3")),
		}
	})
	urls, truncated, err := Fetch(context.Background(), base+"/index.xml", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || strings.Join(urls, ",") != "https://e/1,https://e/2,https://e/3" {
		t.Fatalf("urls=%v truncated=%v", urls, truncated)
	}
}

func TestFetchHonorsMaxURLs(t *testing.T) {
	base := serve(t, func(string) map[string][]byte {
		return map[string][]byte{"/s.xml": []byte(urlset("https://e/1", "https://e/2", "https://e/3"))}
	})
	urls, truncated, err := Fetch(context.Background(), base+"/s.xml", Options{MaxURLs: 2})
	if err != nil || len(urls) != 2 || !truncated {
		t.Fatalf("urls=%v truncated=%v err=%v", urls, truncated, err)
	}
}

func TestFetchSurvivesSelfReferencingIndex(t *testing.T) {
	base := serve(t, func(base string) map[string][]byte {
		return map[string][]byte{"/loop.xml": []byte(`<sitemapindex><sitemap><loc>` + base + `/loop.xml</loc></sitemap></sitemapindex>`)}
	})
	urls, _, err := Fetch(context.Background(), base+"/loop.xml", Options{})
	if err != nil || len(urls) != 0 {
		t.Fatalf("urls=%v err=%v", urls, err)
	}
}

func TestFetchErrors(t *testing.T) {
	base := serve(t, func(string) map[string][]byte {
		return map[string][]byte{"/html": []byte("<html><body>hi</body></html>"), "/bad": []byte("<urlset><url>")}
	})
	for _, target := range []string{base + "/missing", base + "/html", base + "/bad", "file:///etc/passwd", "not a url"} {
		if _, _, err := Fetch(context.Background(), target, Options{}); err == nil {
			t.Errorf("%s: expected error", target)
		}
	}
}
