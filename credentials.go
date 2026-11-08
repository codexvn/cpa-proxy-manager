package main

// credentials.go builds the credential inventory in real time. Every request
// starts from one host.auth.list call for metadata, applies the shared filter
// (the table page, the preview page and batch execution all consume the same
// FilterEntries function), then reads only the files it needs to answer: page
// rows for the table and preview, the full filtered set for execution. Filtering
// by proxy binding is the exception: proxy_url is not in the list metadata, so
// that filter reads every candidate file before paging. Files are read directly
// from disk through the entry path because the plugin runs in-process with the
// host; host.auth.get stays only as the fallback for entries without a path.
// Nothing is cached between requests.
import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"cpa-proxy-manager/cpasdk/pluginapi"
)

// Credential is the plugin-side view of one auth entry plus its raw file JSON.
// JSON never leaves the plugin: the browser view only carries the pre-rendered
// preview and the current proxy value.
type Credential struct {
	AuthIndex  string
	Name       string
	Provider   string
	Email      string
	Account    string
	ProxyURL   string
	ProxyKnown bool
	Disabled   bool
	JSON       map[string]any
}

// CredentialView is the JSON shape served to the page.
type CredentialView struct {
	AuthIndex  string `json:"auth_index"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Email      string `json:"email,omitempty"`
	Account    string `json:"account,omitempty"`
	ProxyURL   string `json:"proxy_url"`
	ProxyKnown bool   `json:"proxy_known"`
	Disabled   bool   `json:"disabled,omitempty"`
	Preview    string `json:"preview,omitempty"`
	Renderable bool   `json:"renderable"`
	SkipReason string `json:"skip_reason,omitempty"`
}

func callHostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := callHost("host.auth.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", errUnmarshal)
	}
	return resp.Files, nil
}

func callHostAuthGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	raw, err := callHost("host.auth.get", pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return pluginapi.HostAuthGetResponse{}, err
	}
	var resp pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("decode host.auth.get result: %w", errUnmarshal)
	}
	return resp, nil
}

// PageFilter carries the query parameters shared by every data endpoint.
type PageFilter struct {
	Q        string
	Provider string
	Bound    string // "", "bound" or "unbound"
	Page     int
	Size     int
}

const (
	defaultPageSize = 50
	maxPageSize     = 500
)

// ParsePageFilter reads and clamps the shared query parameters. Unknown or
// out-of-range values fall back to defaults instead of failing the request.
func ParsePageFilter(query url.Values) PageFilter {
	f := PageFilter{
		Q:        strings.TrimSpace(query.Get("q")),
		Provider: strings.TrimSpace(query.Get("provider")),
		Bound:    normalizeBound(query.Get("bound")),
		Page:     1,
		Size:     defaultPageSize,
	}
	if v, err := strconv.Atoi(query.Get("page")); err == nil && v >= 1 {
		f.Page = v
	}
	if v, err := strconv.Atoi(query.Get("size")); err == nil && v >= 1 {
		f.Size = v
	}
	if f.Size > maxPageSize {
		f.Size = maxPageSize
	}
	return f
}

// MatchFilter reports whether one inventory entry matches the query filter:
// provider compares exactly, q matches case-insensitively against email or
// file name. Proxy binding is applied later by FilterEntries, because
// proxy_url is not on the list entry. The table page, the preview page and
// batch execution all call FilterEntries so the three views cannot diverge.
func MatchFilter(entry pluginapi.HostAuthFileEntry, q, provider string) bool {
	if provider != "" && entry.Provider != provider {
		return false
	}
	if q == "" {
		return true
	}
	needle := strings.ToLower(q)
	return strings.Contains(strings.ToLower(entry.Email), needle) ||
		strings.Contains(strings.ToLower(entry.Name), needle)
}

// normalizeBound accepts only the two explicit proxy-binding filters.
// Anything else, including empty, means "do not filter on binding".
func normalizeBound(v string) string {
	switch strings.TrimSpace(v) {
	case "bound", "unbound":
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

// proxyBound reports whether an auth file has a known, non-empty proxy_url.
// known is false when the file cannot be read; those rows are neither bound
// nor unbound.
func proxyBound(entry pluginapi.HostAuthFileEntry) (known, bound bool) {
	_, proxyURL, known := readAuthDoc(entry)
	if !known {
		return false, false
	}
	return true, strings.TrimSpace(proxyURL) != ""
}

// filterByProxyBound keeps entries whose auth file matches the binding filter.
// Order is preserved. Reads use the same concurrency cap as Hydrate, because
// proxy_url is not part of the host.auth.list metadata.
func filterByProxyBound(entries []pluginapi.HostAuthFileEntry, bound string) []pluginapi.HostAuthFileEntry {
	keep := make([]bool, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, fileReadConcurrency)
	for i := range entries {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			known, has := proxyBound(entries[idx])
			keep[idx] = known && ((bound == "bound" && has) || (bound == "unbound" && !has))
		}(i)
	}
	wg.Wait()
	out := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for i, entry := range entries {
		if keep[i] {
			out = append(out, entry)
		}
	}
	return out
}

// FilterEntries drops runtime-only entries and keeps the matching ones.
// bound is "", "bound" (non-empty proxy_url) or "unbound" (a readable file
// whose proxy_url is empty). Unreadable files match neither binding filter.
func FilterEntries(entries []pluginapi.HostAuthFileEntry, q, provider, bound string) []pluginapi.HostAuthFileEntry {
	bound = normalizeBound(bound)
	out := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.RuntimeOnly || entry.AuthIndex == "" {
			continue
		}
		if MatchFilter(entry, q, provider) {
			out = append(out, entry)
		}
	}
	if bound == "" {
		return out
	}
	return filterByProxyBound(out, bound)
}

// PageSlice clamps page into range and returns that slice plus the adjusted
// page and total page count.
func PageSlice[T any](items []T, page, size int) (pageItems []T, adjusted int, pages int) {
	total := len(items)
	if size < 1 {
		size = 1
	}
	pages = (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	return items[start:end], page, pages
}

// distinctProviders lists the provider keys present in the inventory.
func distinctProviders(entries []pluginapi.HostAuthFileEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.RuntimeOnly || entry.AuthIndex == "" || entry.Provider == "" {
			continue
		}
		if _, dup := seen[entry.Provider]; dup {
			continue
		}
		seen[entry.Provider] = struct{}{}
		out = append(out, entry.Provider)
	}
	sort.Strings(out)
	return out
}

// readAuthDoc returns the parsed auth file plus its root proxy_url. A direct
// disk read through the entry path keeps this local IO; host.auth.get is the
// fallback when the entry carries no path or the read fails.
func readAuthDoc(entry pluginapi.HostAuthFileEntry) (map[string]any, string, bool) {
	if path := strings.TrimSpace(entry.Path); path != "" {
		if raw, errRead := os.ReadFile(path); errRead == nil {
			doc := map[string]any{}
			_ = json.Unmarshal(raw, &doc)
			proxyURL, _ := doc["proxy_url"].(string)
			return doc, proxyURL, true
		}
	}
	resp, errGet := callHostAuthGet(entry.AuthIndex)
	if errGet != nil {
		return nil, "", false
	}
	doc := map[string]any{}
	_ = json.Unmarshal(resp.JSON, &doc)
	proxyURL, _ := doc["proxy_url"].(string)
	return doc, proxyURL, true
}

// hydrateOne builds the plugin-side credential view of one inventory entry.
func hydrateOne(entry pluginapi.HostAuthFileEntry) Credential {
	doc, proxyURL, known := readAuthDoc(entry)
	return Credential{
		AuthIndex:  entry.AuthIndex,
		Name:       entry.Name,
		Provider:   entry.Provider,
		Email:      entry.Email,
		Account:    entry.Account,
		ProxyURL:   proxyURL,
		ProxyKnown: known,
		Disabled:   entry.Disabled,
		JSON:       doc,
	}
}

// fileReadConcurrency bounds parallel auth file reads.
const fileReadConcurrency = 32

// Hydrate reads the given inventory entries with bounded concurrency and
// renders each one against the current template. Views stay aligned with the
// input entries by index.
func Hydrate(entries []pluginapi.HostAuthFileEntry, tpl Template, phs []Placeholder) []CredentialView {
	creds := make([]Credential, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, fileReadConcurrency)
	for i := range entries {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			creds[idx] = hydrateOne(entries[idx])
		}(i)
	}
	wg.Wait()

	out := make([]CredentialView, 0, len(creds))
	for i := range creds {
		cred := &creds[i]
		view := CredentialView{
			AuthIndex:  cred.AuthIndex,
			Name:       cred.Name,
			Provider:   cred.Provider,
			Email:      cred.Email,
			Account:    cred.Account,
			ProxyURL:   cred.ProxyURL,
			ProxyKnown: cred.ProxyKnown,
			Disabled:   cred.Disabled,
		}
		view.Preview, view.SkipReason = renderPreview(tpl.URL, phs, cred)
		view.Renderable = view.Preview != ""
		out = append(out, view)
	}
	return out
}

// placeholderSamplesFor resolves the sample value of every placeholder against
// one credential's file, used by the template management dialog.
func placeholderSamplesFor(entry pluginapi.HostAuthFileEntry, phs []Placeholder) map[string]string {
	out := map[string]string{}
	if len(phs) == 0 {
		return out
	}
	cred := hydrateOne(entry)
	if cred.JSON == nil {
		return out
	}
	for _, ph := range phs {
		if value, ok := jsonPathValue(cred.JSON, ph.Path); ok {
			out[ph.Name] = value
		}
	}
	return out
}

// PlanCounts is the confirmation payload: how many entries the selection
// resolved to, how many the batch will write, and how many it will skip.
type PlanCounts struct {
	Matched int `json:"matched"`
	WillDo  int `json:"will_do"`
	Skipped int `json:"skipped"`
}

// batchRequest is the selection payload shared by the plan and apply
// endpoints. A non-empty Indexes picks an explicit selection (manual
// checkboxes); otherwise the shared filter scope minus Exclude applies (the
// select-all marker with per-row exceptions).
type batchRequest struct {
	Mode     string   `json:"mode"`
	Indexes  []string `json:"indexes"`
	Q        string   `json:"q"`
	Provider string   `json:"provider"`
	Bound    string   `json:"bound"`
	Exclude  []string `json:"exclude"`
}

// missingSelectionError reports explicitly requested indexes that no longer
// exist in the inventory.
type missingSelectionError struct {
	Indexes []string
}

func (e *missingSelectionError) Error() string {
	return T("err_unknown_target") + strings.Join(e.Indexes, ", ")
}

// resolveSelection maps a batch request to hydrated views. Selection order
// follows the request in explicit mode and the inventory order in filter mode.
func resolveSelection(req batchRequest) ([]CredentialView, []string, error) {
	entries, errList := callHostAuthList()
	if errList != nil {
		return nil, nil, errList
	}
	snapshot := configStore().Snapshot()
	tpl, _ := snapshot.Selected()

	var picked []pluginapi.HostAuthFileEntry
	var missing []string
	if len(req.Indexes) > 0 {
		byIndex := make(map[string]pluginapi.HostAuthFileEntry, len(entries))
		for _, entry := range entries {
			if entry.RuntimeOnly || entry.AuthIndex == "" {
				continue
			}
			byIndex[entry.AuthIndex] = entry
		}
		for _, index := range req.Indexes {
			if entry, ok := byIndex[index]; ok {
				picked = append(picked, entry)
			} else {
				missing = append(missing, index)
			}
		}
	} else {
		excluded := make(map[string]struct{}, len(req.Exclude))
		for _, index := range req.Exclude {
			excluded[index] = struct{}{}
		}
		for _, entry := range FilterEntries(entries, req.Q, req.Provider, req.Bound) {
			if _, skip := excluded[entry.AuthIndex]; skip {
				continue
			}
			picked = append(picked, entry)
		}
	}
	views := Hydrate(picked, tpl, snapshot.Placeholders)
	return views, missing, nil
}

// targetsFromViews materializes the write set for one mode: set writes the
// rendered preview, clear writes an empty value for rows with a known proxy.
func targetsFromViews(mode string, views []CredentialView) (targets []EngineTarget, skipped int) {
	for i := range views {
		view := views[i]
		label := view.Email + " · " + view.Name
		switch mode {
		case "set":
			if view.Preview == "" {
				skipped++
				continue
			}
			targets = append(targets, EngineTarget{FileName: view.Name, Value: view.Preview, Label: label})
		case "clear":
			if !view.ProxyKnown || strings.TrimSpace(view.ProxyURL) == "" {
				skipped++
				continue
			}
			targets = append(targets, EngineTarget{FileName: view.Name, Value: "", Label: label})
		}
	}
	return targets, skipped
}

// ResolveBatch computes the write set and confirmation counts for one batch
// request. The plan endpoint (confirmation numbers) and the apply endpoint
// (execution) both run this, so the numbers the user confirms are produced by
// the same code that picks the targets.
func ResolveBatch(req batchRequest) ([]EngineTarget, PlanCounts, error) {
	views, missing, errSel := resolveSelection(req)
	if errSel != nil {
		return nil, PlanCounts{}, errSel
	}
	if len(missing) > 0 {
		return nil, PlanCounts{}, &missingSelectionError{Indexes: missing}
	}
	targets, skipped := targetsFromViews(req.Mode, views)
	return targets, PlanCounts{Matched: len(views), WillDo: len(targets), Skipped: skipped}, nil
}
