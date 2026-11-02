// Package sitemap reads page URLs from sitemap.xml files.
//
// It deliberately uses its own HTTP client, separate from the API client, so
// the Seznam API key can never reach a sitemap host.
package sitemap

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

const (
	maxBytes = 100 << 20 // protocol limit is 50 MB uncompressed; allow some slack
	maxDepth = 3
)

// Options limit a fetch. MaxURLs of 0 means no limit.
type Options struct {
	MaxURLs int
	Client  *http.Client
}

// Fetch returns the page URLs of a sitemap, following sitemap indexes.
// truncated reports that MaxURLs cut the list short.
func Fetch(ctx context.Context, sitemapURL string, opts Options) (urls []string, truncated bool, err error) {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 60 * time.Second}
	}
	f := &fetcher{opts: opts, seen: map[string]bool{}}
	if err := f.walk(ctx, sitemapURL, 0); err != nil {
		return nil, false, err
	}
	return f.urls, f.truncated, nil
}

type fetcher struct {
	opts      Options
	seen      map[string]bool
	urls      []string
	truncated bool
}

type document struct {
	XMLName xml.Name
	URLs    []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

func (f *fetcher) walk(ctx context.Context, target string, depth int) error {
	if depth > maxDepth {
		return apperr.Usage("sitemap", "sitemap index nesting exceeds %d levels", maxDepth)
	}
	if f.seen[target] {
		return nil
	}
	f.seen[target] = true
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return apperr.Usage("sitemap", "sitemap URL %q must be absolute http(s)", target)
	}
	body, err := f.get(ctx, target)
	if err != nil {
		return err
	}
	var doc document
	if err := xml.Unmarshal(body, &doc); err != nil {
		return apperr.Usage("sitemap", "%s is not valid sitemap XML: %s", target, err.Error())
	}
	switch doc.XMLName.Local {
	case "urlset":
		for _, e := range doc.URLs {
			loc := strings.TrimSpace(e.Loc)
			if loc == "" {
				continue
			}
			if f.opts.MaxURLs > 0 && len(f.urls) >= f.opts.MaxURLs {
				f.truncated = true
				return nil
			}
			f.urls = append(f.urls, loc)
		}
	case "sitemapindex":
		for _, e := range doc.Sitemaps {
			if f.truncated {
				return nil
			}
			if err := f.walk(ctx, strings.TrimSpace(e.Loc), depth+1); err != nil {
				return err
			}
		}
	default:
		return apperr.Usage("sitemap", "%s has root element <%s>, expected <urlset> or <sitemapindex>", target, doc.XMLName.Local)
	}
	return nil
}

func (f *fetcher) get(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, apperr.Usage("sitemap", "invalid sitemap URL: %s", err.Error())
	}
	resp, err := f.opts.Client.Do(req)
	if err != nil {
		return nil, apperr.New(apperr.CodeNetworkError, "cannot fetch sitemap: %s", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.Usage("sitemap", "fetching %s returned HTTP %d", target, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, apperr.New(apperr.CodeNetworkError, "cannot read sitemap: %s", err.Error())
	}
	// Sniff instead of trusting the extension: servers often decompress .xml.gz.
	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, apperr.Usage("sitemap", "invalid gzip in %s: %s", target, err.Error())
		}
		defer zr.Close()
		if data, err = io.ReadAll(io.LimitReader(zr, maxBytes)); err != nil {
			return nil, apperr.Usage("sitemap", "invalid gzip in %s: %s", target, err.Error())
		}
	}
	return data, nil
}
