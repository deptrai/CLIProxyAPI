package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const webSearchAPIModel = "web-search"

// webSearchHandler serves POST /v1/search: a raw web search API backed by the
// web-search plugin executors. The request body is forwarded to the plugin
// executor as-is ({"query": "...", "max_results": N}) and the plugin returns
// plain JSON results.
func (s *Server) webSearchHandler(c *gin.Context) {
	raw, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "read request body: " + errRead.Error(), "type": "invalid_request_error"}})
		return
	}
	var apiReq struct {
		Query      string `json:"query"`
		TimeRange  string `json:"time_range"`
		SafeSearch *int   `json:"safesearch"`
	}
	if errUnmarshal := json.Unmarshal(raw, &apiReq); errUnmarshal != nil || strings.TrimSpace(apiReq.Query) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "query is required", "type": "invalid_request_error"}})
		return
	}
	switch apiReq.TimeRange {
	case "", "day", "week", "month", "year":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "time_range must be one of day|week|month|year", "type": "invalid_request_error"}})
		return
	}
	if apiReq.SafeSearch != nil && (*apiReq.SafeSearch < 0 || *apiReq.SafeSearch > 2) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "safesearch must be 0, 1 or 2", "type": "invalid_request_error"}})
		return
	}
	host := s.pluginHost
	if host == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "plugin host is unavailable", "type": "api_error"}})
		return
	}
	routeResp, handled := host.RouteModel(c.Request.Context(), pluginapi.ModelRouteRequest{
		SourceFormat:   "claude",
		RequestedModel: webSearchAPIModel,
		Stream:         false,
		Body:           raw,
	})
	if !handled || !routeResp.Handled || routeResp.TargetKind != pluginapi.ModelRouteTargetSelf {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "no web search backend available", "type": "api_error"}})
		return
	}
	resp, errExec := host.ExecutePluginExecutor(c.Request.Context(), routeResp.Target, coreexecutor.Request{
		Model:   webSearchAPIModel,
		Payload: raw,
	}, coreexecutor.Options{
		Stream:          false,
		OriginalRequest: raw,
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
	})
	if errExec != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": errExec.Error(), "type": "api_error"}})
		return
	}
	c.Data(http.StatusOK, "application/json", resp.Payload)
}
