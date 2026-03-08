// manifests.go — Kubernetes manifest serialisation helpers.
//
// This file is responsible for serialising each resource type to YAML and
// uploading it to S3, and for discovering additional CRD types via the API
// discovery client so that third-party operators (cert-manager, Prometheus,
// Argo CD, …) are included automatically.
//
// S3 key layout:
//
//	<keyPrefix>/<resource>/<name>.yaml
//
// Example:
//
//	my-app/my-backup-01/deployments/web.yaml
package backup

import (
	"context" // for cancellation / deadlines
	"fmt"     // for error wrapping
	"log"     // for structured logging
	"strings" // for Contains (skip sub-resources like "pods/log")

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // ListOptions, Verbs
	"k8s.io/apimachinery/pkg/runtime/schema"      // GroupVersionResource, ParseGroupVersion

	"replic2/internal/k8s"   // Kubernetes client wrapper
	"replic2/internal/store" // JSONToYAML, PutObject — S3 upload helpers
)

// backupResourceType lists every resource of the given GVR in namespace ns and
// uploads one YAML object per resource instance to S3 under keyPrefix/<resource>/<name>.yaml.
//
// Fields that must not be re-applied verbatim (resourceVersion, uid, …) are
// stripped before serialisation so that a restore can apply the objects cleanly.
func backupResourceType(ctx context.Context, c *k8s.Clients, ns, keyPrefix string, gvr schema.GroupVersionResource) error {
	// List all instances of this resource type in the target namespace.
	list, err := c.Dynamic.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err // caller logs and continues — a missing API is non-fatal
	}

	for _, item := range list.Items {
		// Strip server-managed fields so the object can be re-applied via SSA.
		item.SetManagedFields(nil)               // managed-field entries are server-side metadata
		item.SetResourceVersion("")              // must be blank for a clean apply
		item.SetUID("")                          // UID is assigned by the server, not us
		item.SetCreationTimestamp(metav1.Time{}) // creation time is server-assigned
		item.SetGeneration(0)                    // generation counter is server-assigned
		delete(item.Object, "status")            // status is reconciled by the controller, not stored

		// Marshal the stripped object to JSON first (the Unstructured API gives us JSON).
		raw, err := item.MarshalJSON()
		if err != nil {
			log.Printf("backup: marshal %s/%s: %v", gvr.Resource, item.GetName(), err)
			continue // skip this object, keep writing others
		}

		// Convert JSON → YAML for human-readability in S3.
		yamlBytes, err := store.JSONToYAML(raw)
		if err != nil {
			log.Printf("backup: json→yaml %s/%s: %v", gvr.Resource, item.GetName(), err)
			continue // skip this object, keep writing others
		}

		// Build the S3 key: <keyPrefix>/<resource>/<name>.yaml
		key := fmt.Sprintf("%s/%s/%s.yaml", keyPrefix, gvr.Resource, item.GetName())

		// Upload the YAML bytes directly to S3.
		// Guard: if c.S3 is nil (e.g. unit tests with fake clients), skip the upload.
		if c.S3 != nil {
			if err := store.PutObject(ctx, c.S3, key, yamlBytes); err != nil {
				log.Printf("backup: put s3 %s: %v", key, err)
				// Non-fatal: log and continue with the next object.
			}
		}
	}
	return nil
}

// discoverCRDTypes asks the API server's discovery endpoint for every
// namespace-scoped resource that supports the "list" verb, then filters out:
//   - groups in systemGroups (infrastructure / replic2 internals)
//   - groups already covered by coreResourceTypes ("", "apps", "networking.k8s.io")
//   - sub-resources (names containing "/", e.g. "pods/log")
//
// The returned slice is appended to coreResourceTypes in process.go so that
// third-party CRDs are captured automatically without any code changes.
func discoverCRDTypes(c *k8s.Clients) ([]schema.GroupVersionResource, error) {
	// ServerPreferredNamespacedResources may return partial results when some
	// API groups are temporarily unavailable — that is normal; we log and carry on.
	lists, err := c.Discovery.ServerPreferredNamespacedResources()
	if err != nil {
		log.Printf("backup: discovery partial error (continuing): %v", err)
		// err is intentionally not returned — partial results are still useful
	}

	// Build a fast-lookup set of the GVRs already in coreResourceTypes so we
	// do not duplicate them in the extra list.
	coreSet := make(map[schema.GroupVersionResource]bool, len(coreResourceTypes))
	for _, gvr := range coreResourceTypes {
		coreSet[gvr] = true
	}

	var extra []schema.GroupVersionResource // discovered CRD types to return
	for _, list := range lists {
		// Parse "apps/v1" or "v1" into a GroupVersion.
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue // skip unparseable group versions
		}

		// Skip groups we never want to back up.
		if systemGroups[gv.Group] {
			continue
		}

		// Skip groups already fully handled by coreResourceTypes.
		if gv.Group == "" || gv.Group == "apps" || gv.Group == "networking.k8s.io" {
			continue
		}

		for _, r := range list.APIResources {
			// Sub-resources (e.g. "pods/log", "deployments/scale") are not
			// top-level objects and cannot be listed independently.
			if strings.Contains(r.Name, "/") {
				continue
			}

			// Only include resources that can actually be listed.
			if !verbSupported(r.Verbs, "list") {
				continue
			}

			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: r.Name,
			}

			// Avoid duplicates with coreResourceTypes.
			if !coreSet[gvr] {
				extra = append(extra, gvr)
			}
		}
	}
	return extra, nil
}

// verbSupported returns true if verb appears anywhere in the verbs slice.
// It is used to check whether a discovered resource supports "list".
func verbSupported(verbs metav1.Verbs, verb string) bool {
	for _, v := range verbs { // iterate over every supported verb
		if v == verb {
			return true // found it
		}
	}
	return false // verb not in the list
}

// VerbSupported is the exported version used in tests.
func VerbSupported(verbs metav1.Verbs, verb string) bool { return verbSupported(verbs, verb) }
