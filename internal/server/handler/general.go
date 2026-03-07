package handler

import (
	"net/http"
	"os"
	"replic2/internal/k8s"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func General(c *gin.Context, clients *k8s.Clients, GVR schema.GroupVersionResource) {

	list, err := clients.Dynamic.Resource(GVR).List(c, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	result := make([]Response, 0, len(list.Items))
	for _, item := range list.Items {
		metadata, _ := item.Object["metadata"].(map[string]interface{})
		name, _ := metadata["name"].(string)
		status, _ := item.Object["status"].(map[string]interface{})
		phase, _ := status["phase"].(string)
		completeAt, _ := status["completedAt"].(string)
		if phase == "" {
			phase = "Pending"
		}
		if completeAt == "" {
			completeAt = "N/A"
		}
		// Strip fields that must not be re-applied verbatim.
		result = append(result, Response{
			Name:        name,
			Phase:       phase,
			CompletedAt: completeAt,
		})
	}
	c.JSON(http.StatusOK, result)
}
