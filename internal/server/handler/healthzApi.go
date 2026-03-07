package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// HealthzHandler handles GET /healthz — liveness probe.
func HealthzHandler(c *gin.Context, startTime time.Time) {
	c.JSON(http.StatusOK, HealthResponse{
		Status:   "ok",
		Uptime:   time.Since(startTime).Round(time.Second).String(),
		Hostname: Hostname(),
	})
}
