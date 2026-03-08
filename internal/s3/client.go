// Package s3 initialises an AWS S3-compatible client for replic2.
//
// Configuration is read entirely from environment variables so that no
// credentials are baked into the binary or Kubernetes manifests:
//
//	S3_ENDPOINT        — optional; set to the MinIO (or other) endpoint URL
//	                     (e.g. "http://minio:9000").  Omit for real AWS S3.
//	S3_BUCKET          — required; the bucket name where backups are stored.
//	S3_REGION          — required; AWS region (e.g. "us-east-1").
//	S3_ACCESS_KEY_ID   — required; access key ID.
//	S3_SECRET_ACCESS_KEY — required; secret access key.
//	S3_USE_PATH_STYLE  — set to "true" for MinIO / path-style addressing.
//
// The exported Config struct holds both the SDK client and the bucket name so
// callers do not need to remember the bucket separately.
package s3

import (
	"context"  // for loading the AWS config
	"fmt"      // for error wrapping
	"net/http" // for http.Client, http.Request, http.Response (custom timeout wrapper)
	"os"       // for reading env vars
	"strings"  // for TrimSpace / ToLower on S3_USE_PATH_STYLE
	"time"     // for a reasonable HTTP timeout on the SDK transport

	"github.com/aws/aws-sdk-go-v2/aws"              // aws.String helper
	awsconfig "github.com/aws/aws-sdk-go-v2/config" // LoadDefaultConfig
	"github.com/aws/aws-sdk-go-v2/credentials"      // NewStaticCredentialsProvider
	"github.com/aws/aws-sdk-go-v2/service/s3"       // s3.NewFromConfig, s3.Options
)

// Config is the single value passed to every function that needs S3 access.
// It bundles the low-level SDK client with the bucket name so call-sites stay
// concise — callers never need to pass the bucket string separately.
type Config struct {
	// Client is the configured AWS SDK v2 S3 client.
	Client *s3.Client

	// Bucket is the S3 bucket where all replic2 backups are stored.
	Bucket string
}

// New reads S3 configuration from environment variables, constructs an SDK
// client, and returns a Config ready for use.
//
// Returns an error if any required variable is missing.
func New(ctx context.Context) (*Config, error) {
	// --- required variables -------------------------------------------------

	bucket := os.Getenv("S3_BUCKET") // the bucket where backups are stored
	if bucket == "" {
		return nil, fmt.Errorf("S3_BUCKET env var is required")
	}

	region := os.Getenv("S3_REGION") // AWS region (e.g. "us-east-1")
	if region == "" {
		return nil, fmt.Errorf("S3_REGION env var is required")
	}

	accessKeyID := os.Getenv("S3_ACCESS_KEY_ID") // access key ID
	if accessKeyID == "" {
		return nil, fmt.Errorf("S3_ACCESS_KEY_ID env var is required")
	}

	secretKey := os.Getenv("S3_SECRET_ACCESS_KEY") // secret access key
	if secretKey == "" {
		return nil, fmt.Errorf("S3_SECRET_ACCESS_KEY env var is required")
	}

	// --- optional variables -------------------------------------------------

	endpoint := os.Getenv("S3_ENDPOINT") // leave empty for real AWS S3

	// S3_USE_PATH_STYLE=true is required for MinIO and most non-AWS providers.
	pathStyleStr := strings.ToLower(strings.TrimSpace(os.Getenv("S3_USE_PATH_STYLE")))
	usePathStyle := pathStyleStr == "true" || pathStyleStr == "1" // parse as bool

	// --- build a static credentials provider --------------------------------

	// Static credentials: bypass the default credential chain (IAM role,
	// ~/.aws/credentials, etc.) and use the explicit env vars instead.
	creds := credentials.NewStaticCredentialsProvider(
		accessKeyID, // access key ID
		secretKey,   // secret access key
		"",          // session token — empty for long-lived keys
	)

	// --- load the AWS SDK configuration -------------------------------------

	// LoadDefaultConfig sets up the HTTP client, retry logic, and other
	// defaults.  We override region and credentials explicitly.
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),             // set the AWS region
		awsconfig.WithCredentialsProvider(creds), // use our static creds
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// --- build the S3 client ------------------------------------------------

	// s3.Options lets us override endpoint and path-style after the shared
	// config is already set, so we have a single code path for both AWS and
	// MinIO.
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			// Custom endpoint: used for MinIO or any S3-compatible service.
			o.BaseEndpoint = aws.String(endpoint)
		}
		// Path-style addressing: required by MinIO and most non-AWS providers.
		o.UsePathStyle = usePathStyle
		// Reduce the SDK's default HTTP timeout so a bad endpoint fails fast
		// rather than blocking a backup indefinitely.
		o.HTTPClient = &httpClientWithTimeout{timeout: 30 * time.Second}
	})

	return &Config{
		Client: client, // the configured S3 client
		Bucket: bucket, // the target bucket name
	}, nil
}

// httpClientWithTimeout is a minimal net/http.Client wrapper that sets a
// deadline on every request.  We embed it here so the s3 package does not
// need an additional import of net/http at the package level.
type httpClientWithTimeout struct {
	timeout time.Duration // maximum time for a single HTTP request
}

// Do implements the aws.HTTPClient interface required by the SDK.
func (h *httpClientWithTimeout) Do(req *http.Request) (*http.Response, error) {
	client := &http.Client{Timeout: h.timeout} // fresh client with our timeout
	return client.Do(req)                      // delegate to the standard library
}
