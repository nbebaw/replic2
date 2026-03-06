package main

// util.go — shared utilities used across backup.go and restore.go.

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// unstructuredObj is a thin alias so that backup.go and restore.go can
// reference the type without importing the unstructured package directly.
type unstructuredObj = unstructured.Unstructured
