// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package retrieval

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/httputil"
)

// TargetHealth describes the health state of a target.
type TargetHealth string

// The possible health states of a target based on the last performed scrape.
const (
	HealthUnknown TargetHealth = "unknown"
	HealthGood    TargetHealth = "up"
	HealthBad     TargetHealth = "down"
)

// Target refers to a singular HTTP or HTTPS endpoint.
type Target struct {
	// Labels before any processing.
	discoveredLabels labels.Labels
	// Any labels that are added to this target and its metrics.
	labels labels.Labels
	// Additional URL parmeters that are part of the target URL.
	params url.Values

	mtx        sync.RWMutex
	lastError  error
	lastScrape time.Time
	health     TargetHealth
}

// NewTarget creates a reasonably configured target for querying.
func NewTarget(labels, discoveredLabels labels.Labels, params url.Values) *Target {
	return &Target{
		labels:           labels,
		discoveredLabels: discoveredLabels,
		params:           params,
		health:           HealthUnknown,
	}
}

// NewHTTPClient returns a new HTTP client configured for the given scrape configuration.
func NewHTTPClient(cfg config.HTTPClientConfig) (*http.Client, error) {
	tlsConfig, err := httputil.NewTLSConfig(cfg.TLSConfig)
	if err != nil {
		return nil, err
	}
	// The only timeout we care about is the configured scrape timeout.
	// It is applied on request. So we leave out any timings here.
	var rt http.RoundTripper = &http.Transport{
		Proxy:              http.ProxyURL(cfg.ProxyURL.URL),
		MaxIdleConns:       10000,
		TLSClientConfig:    tlsConfig,
		DisableCompression: true,
	}

	// If a bearer token is provided, create a round tripper that will set the
	// Authorization header correctly on each request.
	bearerToken := cfg.BearerToken
	if len(bearerToken) == 0 && len(cfg.BearerTokenFile) > 0 {
		b, err := ioutil.ReadFile(cfg.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read bearer token file %s: %s", cfg.BearerTokenFile, err)
		}
		bearerToken = strings.TrimSpace(string(b))
	}

	if len(bearerToken) > 0 {
		rt = httputil.NewBearerAuthRoundTripper(bearerToken, rt)
	}

	if cfg.BasicAuth != nil {
		rt = httputil.NewBasicAuthRoundTripper(cfg.BasicAuth.Username, cfg.BasicAuth.Password, rt)
	}

	// Return a new client with the configured round tripper.
	return httputil.NewClient(rt), nil
}

func (t *Target) String() string {
	return t.URL().String()
}

// hash returns an identifying hash for the target.
func (t *Target) hash() uint64 {
	h := fnv.New64a()
	h.Write([]byte(fmt.Sprintf("%016d", t.labels.Hash())))
	h.Write([]byte(t.URL().String()))

	return h.Sum64()
}

// offset returns the time until the next scrape cycle for the target.
func (t *Target) offset(interval time.Duration) time.Duration {
	now := time.Now().UnixNano()

	var (
		base   = now % int64(interval)
		offset = t.hash() % uint64(interval)
		next   = base + int64(offset)
	)

	if next > int64(interval) {
		next -= int64(interval)
	}
	return time.Duration(next)
}

// Labels returns a copy of the set of all public labels of the target.
func (t *Target) Labels() labels.Labels {
	lset := make(labels.Labels, 0, len(t.labels))
	for _, l := range t.labels {
		if !strings.HasPrefix(l.Name, model.ReservedLabelPrefix) {
			lset = append(lset, l)
		}
	}
	return lset
}

// DiscoveredLabels returns a copy of the target's labels before any processing.
func (t *Target) DiscoveredLabels() labels.Labels {
	lset := make(labels.Labels, len(t.discoveredLabels))
	copy(lset, t.discoveredLabels)
	return lset
}

// URL returns a copy of the target's URL.
func (t *Target) URL() *url.URL {
	params := url.Values{}

	for k, v := range t.params {
		params[k] = make([]string, len(v))
		copy(params[k], v)
	}
	for _, l := range t.labels {
		if !strings.HasPrefix(l.Name, model.ParamLabelPrefix) {
			continue
		}
		ks := l.Name[len(model.ParamLabelPrefix):]

		if len(params[ks]) > 0 {
			params[ks][0] = string(l.Value)
		} else {
			params[ks] = []string{l.Value}
		}
	}

	return &url.URL{
		Scheme:   string(t.labels.Get(model.SchemeLabel)),
		Host:     string(t.labels.Get(model.AddressLabel)),
		Path:     string(t.labels.Get(model.MetricsPathLabel)),
		RawQuery: params.Encode(),
	}
}

func (t *Target) report(start time.Time, dur time.Duration, err error) {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	if err == nil {
		t.health = HealthGood
	} else {
		t.health = HealthBad
	}

	t.lastError = err
	t.lastScrape = start
}

// LastError returns the error encountered during the last scrape.
func (t *Target) LastError() error {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	return t.lastError
}

// LastScrape returns the time of the last scrape.
func (t *Target) LastScrape() time.Time {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	return t.lastScrape
}

// Health returns the last known health state of the target.
func (t *Target) Health() TargetHealth {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	return t.health
}

// Targets is a sortable list of targets.
type Targets []*Target

func (ts Targets) Len() int           { return len(ts) }
func (ts Targets) Less(i, j int) bool { return ts[i].URL().String() < ts[j].URL().String() }
func (ts Targets) Swap(i, j int)      { ts[i], ts[j] = ts[j], ts[i] }

// limitAppender limits the number of total appended samples in a batch.
type limitAppender struct {
	storage.Appender

	limit int
	i     int
}

func (app *limitAppender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	if app.i+1 > app.limit {
		return 0, errors.New("sample limit exceeded")
	}
	ref, err := app.Appender.Add(lset, t, v)
	if err != nil {
		return 0, fmt.Errorf("sample limit of %d exceeded", app.limit)
	}
	app.i++
	return ref, nil
}

func (app *limitAppender) AddFast(ref uint64, t int64, v float64) error {
	if app.i+1 > app.limit {
		return errors.New("sample limit exceeded")
	}

	if err := app.Appender.AddFast(ref, t, v); err != nil {
		return fmt.Errorf("sample limit of %d exceeded", app.limit)
	}
	app.i++
	return nil
}

// Merges the ingested sample's metric with the label set. On a collision the
// value of the ingested label is stored in a label prefixed with 'exported_'.
type ruleLabelsAppender struct {
	storage.Appender
	labels labels.Labels
}

func (app ruleLabelsAppender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	lb := labels.NewBuilder(lset)

	for _, l := range app.labels {
		lv := lset.Get(l.Name)
		if lv != "" {
			lb.Set(model.ExportedLabelPrefix+l.Name, lv)
		}
		lb.Set(l.Name, l.Value)
	}

	return app.Appender.Add(lb.Labels(), t, v)
}

type honorLabelsAppender struct {
	storage.Appender
	labels labels.Labels
}

// Merges the sample's metric with the given labels if the label is not
// already present in the metric.
// This also considers labels explicitly set to the empty string.
func (app honorLabelsAppender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	lb := labels.NewBuilder(lset)

	for _, l := range app.labels {
		if lv := lset.Get(l.Name); lv == "" {
			lb.Set(l.Name, l.Value)
		}
	}
	return app.Appender.Add(lb.Labels(), t, v)
}

// Applies a set of relabel configurations to the sample's metric
// before actually appending it.
type relabelAppender struct {
	storage.Appender
	relabelings []*config.RelabelConfig
}

var errSeriesDropped = errors.New("series dropped")

func (app relabelAppender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	lset = relabel.Process(lset, app.relabelings...)
	if lset == nil {
		return 0, errSeriesDropped
	}
	return app.Appender.Add(lset, t, v)
}

// populateLabels builds a label set from the given label set and scrape configuration.
// It returns a label set before relabeling was applied as the second return value.
// Returns a nil label set if the target is dropped during relabeling.
func populateLabels(lset labels.Labels, cfg *config.ScrapeConfig) (res, orig labels.Labels, err error) {
	if v := lset.Get(model.AddressLabel); v == "" {
		return nil, nil, fmt.Errorf("no address")
	}
	// Copy labels into the labelset for the target if they are not set already.
	scrapeLabels := []labels.Label{
		{Name: model.JobLabel, Value: cfg.JobName},
		{Name: model.MetricsPathLabel, Value: cfg.MetricsPath},
		{Name: model.SchemeLabel, Value: cfg.Scheme},
	}
	lb := labels.NewBuilder(lset)

	for _, l := range scrapeLabels {
		if lv := lset.Get(l.Name); lv == "" {
			lb.Set(l.Name, l.Value)
		}
	}
	// Encode scrape query parameters as labels.
	for k, v := range cfg.Params {
		if len(v) > 0 {
			lb.Set(model.ParamLabelPrefix+k, v[0])
		}
	}

	preRelabelLabels := lb.Labels()
	lset = relabel.Process(preRelabelLabels, cfg.RelabelConfigs...)

	// Check if the target was dropped.
	if lset == nil {
		return nil, nil, nil
	}

	lb = labels.NewBuilder(lset)

	// addPort checks whether we should add a default port to the address.
	// If the address is not valid, we don't append a port either.
	addPort := func(s string) bool {
		// If we can split, a port exists and we don't have to add one.
		if _, _, err := net.SplitHostPort(s); err == nil {
			return false
		}
		// If adding a port makes it valid, the previous error
		// was not due to an invalid address and we can append a port.
		_, _, err := net.SplitHostPort(s + ":1234")
		return err == nil
	}
	addr := lset.Get(model.AddressLabel)
	// If it's an address with no trailing port, infer it based on the used scheme.
	if addPort(addr) {
		// Addresses reaching this point are already wrapped in [] if necessary.
		switch lset.Get(model.SchemeLabel) {
		case "http", "":
			addr = addr + ":80"
		case "https":
			addr = addr + ":443"
		default:
			return nil, nil, fmt.Errorf("invalid scheme: %q", cfg.Scheme)
		}
		lb.Set(model.AddressLabel, addr)
	}

	if err := config.CheckTargetAddress(model.LabelValue(addr)); err != nil {
		return nil, nil, err
	}

	// Meta labels are deleted after relabelling. Other internal labels propagate to
	// the target which decides whether they will be part of their label set.
	for _, l := range lset {
		if strings.HasPrefix(l.Name, model.MetaLabelPrefix) {
			lb.Del(l.Name)
		}
	}

	// Default the instance label to the target address.
	if v := lset.Get(model.InstanceLabel); v == "" {
		lb.Set(model.InstanceLabel, addr)
	}
	return lb.Labels(), preRelabelLabels, nil
}

// targetsFromGroup builds targets based on the given TargetGroup and config.
func targetsFromGroup(tg *config.TargetGroup, cfg *config.ScrapeConfig) ([]*Target, error) {
	targets := make([]*Target, 0, len(tg.Targets))

	for i, tlset := range tg.Targets {
		lbls := make([]labels.Label, 0, len(tlset)+len(tg.Labels))

		for ln, lv := range tlset {
			lbls = append(lbls, labels.Label{Name: string(ln), Value: string(lv)})
		}
		for ln, lv := range tg.Labels {
			if _, ok := tlset[ln]; !ok {
				lbls = append(lbls, labels.Label{Name: string(ln), Value: string(lv)})
			}
		}

		lset := labels.New(lbls...)

		lbls, origLabels, err := populateLabels(lset, cfg)
		if err != nil {
			return nil, fmt.Errorf("instance %d in group %s: %s", i, tg, err)
		}
		if lbls != nil {
			targets = append(targets, NewTarget(lbls, origLabels, cfg.Params))
		}
	}
	return targets, nil
}
