// Package store handles all PVC-backed file I/O for replic2.
//
// Responsibilities:
//   - Resolving the backup root directory (BACKUP_ROOT env var or default).
//   - Reading a single YAML file from the PVC into a map.
//   - Converting a JSON byte slice to YAML for human-readable storage.
//
// Nothing in this package talks to the Kubernetes API server.
package store

import (
	"bytes"
	"os"

	"k8s.io/apimachinery/pkg/util/yaml"
	k8syaml "sigs.k8s.io/yaml"
)

// DefaultBackupRoot is the fallback path when BACKUP_ROOT is unset.
const DefaultBackupRoot = "/data/backups"

// keep unexported alias so internal callers are unaffected
const defaultBackupRoot = DefaultBackupRoot

// BackupRoot returns the PVC mount path where backups are written.
// Override with the BACKUP_ROOT environment variable.
func BackupRoot() string {
	if v := os.Getenv("BACKUP_ROOT"); v != "" {
		return v
	}
	return defaultBackupRoot
}

// defaultBackupPVCName is the Kubernetes PVC name used when BACKUP_PVC_NAME is unset.
const defaultBackupPVCName = "replic2-backups"

// BackupPVCName returns the name of the PersistentVolumeClaim that replic2
// uses to store backups.  Agent pods mount this PVC directly (not via HostPath)
// so they can write tar archives to the same storage as the controller.
// Override with the BACKUP_PVC_NAME environment variable.
func BackupPVCName() string {
	if v := os.Getenv("BACKUP_PVC_NAME"); v != "" {
		return v
	}
	return defaultBackupPVCName
}

// JSONToYAML converts a JSON byte slice to YAML using the sigs.k8s.io/yaml
// library (already a transitive dep via client-go).
func JSONToYAML(j []byte) ([]byte, error) {
	return k8syaml.JSONToYAML(j)
}

// ReadYAML reads a YAML (or JSON) file from path and decodes it into a
// map[string]interface{} that can be wrapped in an Unstructured object.
func ReadYAML(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var obj map[string]interface{}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	return obj, nil
}
