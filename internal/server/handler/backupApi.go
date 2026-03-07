package handler

import (
	"replic2/internal/k8s"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// BackupGVR is the GroupVersionResource for the Backup CRD.
var BackupGVR = schema.GroupVersionResource{
	Group:    "replic2.io",
	Version:  "v1alpha1",
	Resource: "backups",
}

func BackupHandler(c *gin.Context, clients *k8s.Clients) {
	General(c, clients, BackupGVR)
}
