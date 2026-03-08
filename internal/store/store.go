// Package store provides S3 object I/O helpers for replic2.
//
// Responsibilities:
//   - Uploading a byte slice as an S3 object (PutObject).
//   - Downloading an S3 object into a map (GetObject → decode YAML/JSON).
//   - Listing S3 keys under a prefix (ListObjectsV2, paginated).
//   - Deleting all S3 objects under a prefix (bulk delete).
//   - Converting a JSON byte slice to YAML for human-readable storage.
//
// Nothing in this package talks to the Kubernetes API server.
package store

import (
	"bytes"   // for bytes.NewReader (wrap byte slice as io.Reader for PutObject)
	"context" // for S3 API call deadlines
	"fmt"     // for error wrapping

	"k8s.io/apimachinery/pkg/util/yaml" // NewYAMLOrJSONDecoder
	k8syaml "sigs.k8s.io/yaml"          // JSONToYAML

	"github.com/aws/aws-sdk-go-v2/aws"              // aws.String, aws.ToString helpers
	"github.com/aws/aws-sdk-go-v2/service/s3"       // PutObject, GetObject, ListObjectsV2, DeleteObjects
	"github.com/aws/aws-sdk-go-v2/service/s3/types" // ObjectIdentifier (bulk delete)

	s3store "replic2/internal/s3" // Config (client + bucket)
)

// JSONToYAML converts a JSON byte slice to YAML using the sigs.k8s.io/yaml
// library (already a transitive dep via client-go).
func JSONToYAML(j []byte) ([]byte, error) {
	return k8syaml.JSONToYAML(j) // delegate to the upstream library
}

// DecodeYAML decodes a YAML (or JSON) byte slice into a map[string]interface{}
// that can be wrapped in an Unstructured object.
// This replaces the old ReadYAML(path) which read from the PVC filesystem.
func DecodeYAML(data []byte) (map[string]interface{}, error) {
	var obj map[string]interface{}
	// NewYAMLOrJSONDecoder handles both YAML and JSON inputs transparently.
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	if err := dec.Decode(&obj); err != nil {
		return nil, err // surface decode error to caller
	}
	return obj, nil
}

// PutObject uploads data as an S3 object at the given key in cfg.Bucket.
// The content-type is set to "application/octet-stream" for all objects.
func PutObject(ctx context.Context, cfg *s3store.Config, key string, data []byte) error {
	_, err := cfg.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(cfg.Bucket),                 // target bucket
		Key:         aws.String(key),                        // object key (path within the bucket)
		Body:        bytes.NewReader(data),                  // object content
		ContentType: aws.String("application/octet-stream"), // generic binary content-type
	})
	if err != nil {
		return fmt.Errorf("s3 put %q: %w", key, err)
	}
	return nil
}

// GetObject downloads an S3 object and decodes it as YAML/JSON into a
// map[string]interface{}.  Used by the restore controller to read manifests.
func GetObject(ctx context.Context, cfg *s3store.Config, key string) (map[string]interface{}, error) {
	out, err := cfg.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(cfg.Bucket), // source bucket
		Key:    aws.String(key),        // object key to retrieve
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %q: %w", key, err)
	}
	defer out.Body.Close() // always release the HTTP response body

	// Read the full body into memory so we can decode it.
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(out.Body); err != nil {
		return nil, fmt.Errorf("read s3 object %q: %w", key, err)
	}

	return DecodeYAML(buf.Bytes())
}

// ListKeys returns all S3 object keys that begin with prefix in cfg.Bucket.
// Results are paginated; this function collects all pages before returning.
func ListKeys(ctx context.Context, cfg *s3store.Config, prefix string) ([]string, error) {
	var keys []string // accumulator for all keys found

	paginator := s3.NewListObjectsV2Paginator(cfg.Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(cfg.Bucket), // bucket to list
		Prefix: aws.String(prefix),     // only return keys with this prefix
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3 prefix %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key)) // collect each key
		}
	}
	return keys, nil
}

// DeletePrefix removes every S3 object whose key starts with prefix.
// It issues bulk delete requests (up to 1000 objects per request — S3 limit).
// Non-fatal: if no objects exist under the prefix, the function succeeds silently.
func DeletePrefix(ctx context.Context, cfg *s3store.Config, prefix string) error {
	keys, err := ListKeys(ctx, cfg, prefix) // find all objects under prefix
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil // nothing to delete — not an error
	}

	// Build the identifier list for the bulk delete call.
	// S3 limits each DeleteObjects request to 1000 identifiers.
	const batchSize = 1000
	for i := 0; i < len(keys); i += batchSize {
		end := i + batchSize
		if end > len(keys) {
			end = len(keys) // cap to avoid index-out-of-range on the last batch
		}

		batch := make([]types.ObjectIdentifier, 0, end-i)
		for _, k := range keys[i:end] {
			batch = append(batch, types.ObjectIdentifier{
				Key: aws.String(k), // each key to delete
			})
		}

		_, err := cfg.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(cfg.Bucket), // target bucket
			Delete: &types.Delete{
				Objects: batch,          // the objects to remove in this batch
				Quiet:   aws.Bool(true), // suppress per-object success responses
			},
		})
		if err != nil {
			return fmt.Errorf("delete s3 objects under %q: %w", prefix, err)
		}
	}
	return nil
}
