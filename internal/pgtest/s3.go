package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 test environment: FLOPWIRE_TEST_S3_ENDPOINT (host:port, plain HTTP),
// FLOPWIRE_TEST_S3_ACCESS_KEY, FLOPWIRE_TEST_S3_SECRET_KEY.
const (
	EnvS3Endpoint  = "FLOPWIRE_TEST_S3_ENDPOINT"
	EnvS3AccessKey = "FLOPWIRE_TEST_S3_ACCESS_KEY"
	EnvS3SecretKey = "FLOPWIRE_TEST_S3_SECRET_KEY"
)

// NewBucket creates an empty bucket, empties and removes it when the test
// ends, and returns a client for it. It skips the test when the S3
// environment is unset.
func NewBucket(t testing.TB) (*minio.Client, string) {
	t.Helper()
	endpoint := os.Getenv(EnvS3Endpoint)
	if endpoint == "" {
		t.Skip(EnvS3Endpoint + " is not set")
	}
	mc, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv(EnvS3AccessKey), os.Getenv(EnvS3SecretKey), "")})
	if err != nil {
		t.Fatalf("pgtest: s3 client: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	bucket := "flopwire-test-" + hex.EncodeToString(b[:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("pgtest: make bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if obj.Err == nil {
				_ = mc.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{})
			}
		}
		_ = mc.RemoveBucket(ctx, bucket)
	})
	return mc, bucket
}
