package handler

import (
	"replic2/internal/k8s"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// RestoreGVR is the GroupVersionResource for the Restore CRD.
var RestoreGVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "restores",
}

func RestoreHandler(c *gin.Context, clients *k8s.Clients) {
	General(c, clients, RestoreGVR)
}
