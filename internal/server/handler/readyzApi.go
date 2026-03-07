package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ReadyzHandler handles GET /readyz — readiness probe.
func ReadyzHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}
