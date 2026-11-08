package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"cpa-proxy-manager/cpasdk/pluginabi"
	"cpa-proxy-manager/cpasdk/pluginapi"
)

const (
	pluginName            = "cpa-proxy-manager"
	pluginDisplayName     = "代理批量设置"
	pluginVersion         = "0.3.0"
	resourceContentType   = "text/html; charset=utf-8"
	jsonContentType       = "application/json; charset=utf-8"
	managementRoutePrefix = "/plugins/" + pluginName
)

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginReconfigure:
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginDisplayName,
			Version:          pluginVersion,
			Author:           "codexvn",
			GitHubRepository: "https://github.com/codexvn/cpa-proxy-manager",
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: registrationCapabilities{
			UsagePlugin:   false,
			Scheduler:     false, // never monopolize the CPA global scheduler slot
			ManagementAPI: true,
		},
	}
}

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: managementRoutePrefix + "/credentials", Description: "Paged credential inventory under the shared filter."},
			{Method: http.MethodGet, Path: managementRoutePrefix + "/preview", Description: "Paged preview of rendered proxy URLs under the shared filter."},
			{Method: http.MethodPost, Path: managementRoutePrefix + "/plan", Description: "Confirmation counts for a batch over an explicit selection or the filter scope."},
			{Method: http.MethodGet, Path: managementRoutePrefix + "/settings", Description: "Read plugin settings (templates, placeholders, concurrency)."},
			{Method: http.MethodPut, Path: managementRoutePrefix + "/settings", Description: "Save plugin settings."},
			{Method: http.MethodPost, Path: managementRoutePrefix + "/apply", Description: "Start a batch set or clear job over an explicit selection or the filter scope."},
			{Method: http.MethodPost, Path: managementRoutePrefix + "/stop", Description: "Stop dispatching new writes in the running batch."},
			{Method: http.MethodGet, Path: managementRoutePrefix + "/status", Description: "Poll batch progress, counts and failure detail."},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        pluginDisplayName,
				Description: "批量设置与清空凭据的代理地址",
			},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, errors.New("decode management request: " + err.Error())
		}
	}
	// Remember the page-provided management key (memory only, never persisted).
	_ = resolveManagementPassword(req.Headers)
	return okEnvelope(dispatchManagement(req))
}

func dispatchManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case method == http.MethodGet && matchesResourcePath(req.Path, "/status"):
		return htmlResponse(http.StatusOK, renderUIPage())
	case method == http.MethodGet && matchesManagementPath(req.Path, "/credentials"):
		return handleCredentials(req.Query)
	case method == http.MethodGet && matchesManagementPath(req.Path, "/preview"):
		return handlePreview(req.Query)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/plan"):
		return handlePlan(req)
	case method == http.MethodGet && matchesManagementPath(req.Path, "/settings"):
		cfg := configStore()
		return jsonResponse(http.StatusOK, cfg.Snapshot())
	case method == http.MethodPut && matchesManagementPath(req.Path, "/settings"):
		return handleConfigSave(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/apply"):
		return handleApply(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/stop"):
		engine.Stop()
		return jsonResponse(http.StatusOK, engine.Snapshot())
	case method == http.MethodGet && matchesManagementPath(req.Path, "/status"):
		return jsonResponse(http.StatusOK, engine.Snapshot())
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found", "path": req.Path, "method": method})
	}
}

// inventoryPage runs the shared request pipeline: one host.auth.list for
// metadata, the shared filter, then one page of hydrated rows.
func inventoryPage(query url.Values) (rows []CredentialView, f PageFilter, total int, pages int, providers []string, tplName string, samples map[string]string, err error) {
	f = ParsePageFilter(query)
	cfg := configStore()
	snapshot := cfg.Snapshot()
	tpl, _ := snapshot.Selected()
	entries, errList := callHostAuthList()
	if errList != nil {
		return nil, f, 0, 0, nil, "", nil, errList
	}
	providers = distinctProviders(entries)
	filtered := FilterEntries(entries, f.Q, f.Provider, f.Bound)
	total = len(filtered)
	pageEntries, page, pages := PageSlice(filtered, f.Page, f.Size)
	f.Page = page
	views := Hydrate(pageEntries, tpl, snapshot.Placeholders)
	if len(filtered) > 0 {
		samples = placeholderSamplesFor(filtered[0], snapshot.Placeholders)
	} else {
		samples = map[string]string{}
	}
	return views, f, total, pages, providers, tpl.Name, samples, nil
}

func handleCredentials(query url.Values) pluginapi.ManagementResponse {
	views, f, total, pages, providers, tplName, samples, err := inventoryPage(query)
	if err != nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": T("err_host_unable") + "：" + err.Error()})
	}
	// The table does not display rendered previews; strip the preview string
	// so the payload carries only what the page renders. SkipReason stays for
	// the disabled-checkbox tooltip.
	for i := range views {
		views[i].Preview = ""
	}
	cfg := configStore().Snapshot()
	return jsonResponse(http.StatusOK, map[string]any{
		"total":               total,
		"page":                f.Page,
		"size":                f.Size,
		"pages":               pages,
		"providers":           providers,
		"template_id":         cfg.SelectedTemplate,
		"template_name":       tplName,
		"placeholder_samples": samples,
		"rows":                views,
	})
}

func handlePreview(query url.Values) pluginapi.ManagementResponse {
	views, f, total, pages, _, _, _, err := inventoryPage(query)
	if err != nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": T("err_host_unable") + "：" + err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"total": total,
		"page":  f.Page,
		"size":  f.Size,
		"pages": pages,
		"rows":  views,
	})
}

func handlePlan(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, rejection, ok := parseBatchRequest(req)
	if !ok {
		return rejection
	}
	_, counts, err := ResolveBatch(body)
	if err != nil {
		return batchError(err)
	}
	return jsonResponse(http.StatusOK, counts)
}

func handleConfigSave(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": T("err_invalid_json")})
	}
	cfg := &Config{}
	if err := json.Unmarshal(req.Body, cfg); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": T("err_invalid_json")})
	}
	if err := configStore().Save(cfg); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "message": T("saved"), "config": configStore().Snapshot()})
}

// parseBatchRequest decodes and mode-checks the shared plan/apply payload.
// The third return is false when the response carries a rejection.
func parseBatchRequest(req pluginapi.ManagementRequest) (batchRequest, pluginapi.ManagementResponse, bool) {
	var body batchRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return body, jsonResponse(http.StatusBadRequest, map[string]any{"error": T("err_invalid_json")}), false
		}
	}
	body.Mode = strings.TrimSpace(body.Mode)
	if body.Mode != "set" && body.Mode != "clear" {
		return body, jsonResponse(http.StatusBadRequest, map[string]any{"error": T("err_invalid_mode")}), false
	}
	return body, pluginapi.ManagementResponse{}, true
}

// batchError maps resolve failures to their HTTP responses.
func batchError(err error) pluginapi.ManagementResponse {
	var missing *missingSelectionError
	if errors.As(err, &missing) {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": T("err_host_unable") + "：" + err.Error()})
}

func handleApply(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, rejection, ok := parseBatchRequest(req)
	if !ok {
		return rejection
	}

	snapshot := configStore().Snapshot()
	targets, counts, err := ResolveBatch(body)
	if err != nil {
		return batchError(err)
	}
	if counts.WillDo == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": T("err_no_targets")})
	}

	err = engine.Start(body.Mode, targets, snapshot.WriteConcurrency, req.Headers)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrEmptyTargets) {
			status = http.StatusBadRequest
		}
		return jsonResponse(status, map[string]any{"error": err.Error()})
	}
	return jsonResponse(http.StatusAccepted, map[string]any{
		"ok":       true,
		"accepted": true,
		"mode":     body.Mode,
		"total":    counts.WillDo,
		"skipped":  counts.Skipped,
	})
}

func matchesManagementPath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return path == managementRoutePrefix+suffix ||
		path == "/v0/management"+managementRoutePrefix+suffix
}

func matchesResourcePath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return path == "/v0/resource/plugins/"+pluginName+suffix
}

func htmlResponse(statusCode int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"Content-Type": []string{resourceContentType}},
		Body:       body,
	}
}

func jsonResponse(statusCode int, payload any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(payload)
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"Content-Type": []string{jsonContentType}},
		Body:       raw,
	}
}
