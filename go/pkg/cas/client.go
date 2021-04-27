// Package cas implements an efficient client for Content Addressable Storage.
package cas

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/retry"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

// Client is a client for Content Addressable Storage.
// Create one using NewClient.
//
// Goroutine-safe.
//
// All fields are considered immutable, and should not be changed.
type Client struct {
	conn *grpc.ClientConn
	// InstanceName is the full name of the RBE instance.
	InstanceName string

	// ClientConfig is the configuration that the client was created with.
	ClientConfig

	byteStream          bspb.ByteStreamClient
	cas                 repb.ContentAddressableStorageClient
	semBatchUpdateBlobs *semaphore.Weighted

	// Mockable functions.

	testScheduleCheck func(ctx context.Context, item *uploadItem) error
}

// ClientConfig is a config for Client.
// See DefaultClientConfig() for the default values.
type ClientConfig struct {
	// FSConcurrency is the maximum number of concurrent file system operations.
	// TODO(nodir): ensure this does not hurt streaming performance
	FSConcurrency int

	// SmallFileThreshold is a size threshold to categorize a file as small.
	// Such files are buffered entirely (read only once).
	SmallFileThreshold int64

	// LargeFileThreshold is a size threshold to categorize a file as large. For
	// such files, IO concurrency limits are much tighter and locality is
	// prioritized: the file is read for the first and second times with minimal
	// delay between the two.
	LargeFileThreshold int64

	// FileIOSize is the size of file reads.
	FileIOSize int64

	// FindMissingBlobsBatchSize is the maximum number of digests to check in a
	// single FindMissingBlobs RPC.
	FindMissingBlobsBatchSize int

	// BatchUpdateBlobsConcurrency is the maximum number of BatchUpdateBlobs RPCs
	// at the same time.
	BatchUpdateBlobsConcurrency int

	// BatchBlobsBatchSize is the maximum number of blobs to upload/download
	// in a single BatchUpdateBlobs/BatchReadBlobs RPC.
	BatchBlobsBatchSize int

	// RetryPolicy specifies how to retry requests on transient errors.
	RetryPolicy retry.BackoffPolicy

	// PreserveSymlinks specifies whether to preserve symlinks or convert them
	// to regular files.
	PreserveSymlinks bool

	// AllowDanglingSymlinks specifies whether to upload dangling links or halt
	// the upload with an error.
	//
	// This field is ignored if PreserveSymlinks is false, which is the default.
	AllowDanglingSymlinks bool

	// TODO(nodir): add per-RPC timeouts.
}

// DefaultClientConfig returns the default config.
//
// To override a specific value:
//   cfg := DefaultClientConfig()
//   ... mutate cfg ...
//   client, err := NewClientWithConfig(ctx, cfg)
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		// GCE docs recommend at least 32 concurrent IOs.
		// https://cloud.google.com/compute/docs/disks/optimizing-pd-performance#io-queue-depth
		// TODO(nodir): tune this number.
		FSConcurrency: 32,

		SmallFileThreshold: 1024 * 1024,       // 1MiB
		LargeFileThreshold: 256 * 1024 * 1024, // 256MiB

		// GCE docs recommend 4MB IO size for large files.
		// https://cloud.google.com/compute/docs/disks/optimizing-pd-performance#io-size
		FileIOSize: 4 * 1024 * 1024, // 4MiB

		FindMissingBlobsBatchSize: 1000,

		BatchUpdateBlobsConcurrency: 256, // arbitrary

		// This is a suggested approximate limit based on current RBE implementation for writes.
		// Above that BatchUpdateBlobs calls start to exceed a typical minute timeout.
		// This default might not be best for reads though.
		BatchBlobsBatchSize: 4000,

		RetryPolicy: retry.ExponentialBackoff(225*time.Millisecond, 2*time.Second, retry.Attempts(6)),
	}
}

// Validate returns a non-nil error if the config is invalid.
func (c *ClientConfig) Validate() error {
	switch {
	case c.FSConcurrency <= 0:
		return fmt.Errorf("FSConcurrency must be positive")

	case c.SmallFileThreshold < 0:
		return fmt.Errorf("SmallFileThreshold must be non-negative")
	case c.LargeFileThreshold <= 0:
		return fmt.Errorf("LargeFileThreshold must be positive")
	case c.SmallFileThreshold >= c.LargeFileThreshold:
		return fmt.Errorf("SmallFileThreshold must be smaller than LargeFileThreshold")

	case c.FileIOSize <= 0:
		return fmt.Errorf("FileIOSize must be positive")

	// Do not allow more than 100K, otherwise we might run into the request size limits.
	case c.FindMissingBlobsBatchSize <= 0 || c.FindMissingBlobsBatchSize > 10000:
		return fmt.Errorf("FindMissingBlobsBatchSize must be in [1, 10000]")

	default:
		return nil
	}
}

// NewClient creates a new client with the default configuration.
// Use client.Dial to create a connection.
func NewClient(ctx context.Context, conn *grpc.ClientConn, instanceName string) (*Client, error) {
	return NewClientWithConfig(ctx, conn, instanceName, DefaultClientConfig())
}

// NewClientWithConfig creates a new client and accepts a configuration.
func NewClientWithConfig(ctx context.Context, conn *grpc.ClientConn, instanceName string, config ClientConfig) (*Client, error) {
	switch err := config.Validate(); {
	case err != nil:
		return nil, errors.Wrap(err, "invalid config")
	case conn == nil:
		return nil, fmt.Errorf("conn is unspecified")
	case instanceName == "":
		return nil, fmt.Errorf("instance name is unspecified")
	}

	client := &Client{
		InstanceName: instanceName,
		ClientConfig: config,
		conn:         conn,
		byteStream:   bspb.NewByteStreamClient(conn),
		cas:          repb.NewContentAddressableStorageClient(conn),
	}
	client.init()

	// TODO(nodir): Check capabilities, optionally.
	return client, nil
}

// init is a part of NewClientWithConfig that can be done in tests without
// creating a real gRPC connection. This function exists purely to aid testing,
// and is tightly coupled with NewClientWithConfig.
func (c *Client) init() {
	c.semBatchUpdateBlobs = semaphore.NewWeighted(int64(c.BatchUpdateBlobsConcurrency))
}

func (c *Client) withRetries(ctx context.Context, f func() error) error {
	return retry.WithPolicy(ctx, retry.TransientOnly, c.RetryPolicy, f)
}
