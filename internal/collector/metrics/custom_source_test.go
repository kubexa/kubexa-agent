package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestSafeURLStripsUserinfoAndQuery pins the log-side leak.
//
// collect.metrics.kube_state_metrics.url and custom_endpoints[].url are
// operator-supplied, `http://exporter/metrics?api_key=...` is legal config,
// and the agent collects its own namespace's logs -- so a raw URL in a warning
// is a credential in a searchable log store. The registry and the heartbeat
// already keep the URL off the wire; this is the log and error-string side.
func TestSafeURLStripsUserinfoAndQuery(t *testing.T) {
	const raw = "https://scraper:s3cr3t@exporter.metrics.svc:8443/metrics?api_key=AKIA-DEADBEEF&x=1"

	got := safeURL(raw)

	if got != "https://exporter.metrics.svc:8443/metrics" {
		t.Fatalf("safeURL = %q, want the scheme, host and path alone", got)
	}
	for _, secret := range []string{"s3cr3t", "AKIA-DEADBEEF", "api_key", "scraper:"} {
		if strings.Contains(got, secret) {
			t.Errorf("safeURL leaked %q: %q", secret, got)
		}
	}
}

// A URL that will not parse is exactly the input most likely to have been
// pasted by hand, so returning the original there would defeat the point.
func TestSafeURLDoesNotFallBackToTheRawValue(t *testing.T) {
	const raw = "http://%zz/metrics?token=hunter2"
	got := safeURL(raw)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("safeURL = %q, want no query string on an unparseable URL", got)
	}
	if safeURL("") != "" {
		t.Errorf("safeURL(\"\") = %q, want empty", safeURL(""))
	}
}

// The error strings ScrapeTarget returns are logged one frame up, verbatim, by
// c.log.Warn(logger.F("error", err.Error())). So an error that names the raw
// URL is the same leak as a log site that does. This drives the real scraper
// against a real server so it pins the call sites, not the helper.
func TestScrapeErrorsCarryNoCredentialFromTheURL(t *testing.T) {
	const secret = "AKIA-DEADBEEF"

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "5xx",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "4xx",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
		},
		{
			name: "unparseable body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("this is not prometheus exposition {{{\n"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			m, err := newScraperMetrics(prometheus.NewRegistry())
			if err != nil {
				t.Fatalf("newScraperMetrics: %v", err)
			}
			s := newCustomScraper(m)
			target := ScrapeTarget{
				Name:    "app",
				Kind:    KindCustom,
				URL:     srv.URL + "/metrics?api_key=" + secret,
				Timeout: 2 * time.Second,
			}

			_, scrapeErr := s.ScrapeTarget(context.Background(), target, nil)
			if scrapeErr == nil {
				t.Fatal("ScrapeTarget returned no error")
			}
			if strings.Contains(scrapeErr.Error(), secret) {
				t.Fatalf("the scrape error carries the URL credential: %q", scrapeErr.Error())
			}
			if strings.Contains(scrapeErr.Error(), "api_key") {
				t.Fatalf("the scrape error carries the query string: %q", scrapeErr.Error())
			}
		})
	}
}
