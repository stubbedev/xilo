package storage

import (
	"context"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/stubbedev/xilo/internal/config"
)

// S3 stores blobs as objects in any S3-compatible bucket (AWS, Garage, R2, …)
// via aws-sdk-go-v2, path-style addressing against a custom endpoint.
type S3 struct {
	c      *s3.Client
	bucket string
}

func NewS3(cfg config.S3) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("storage.s3 requires endpoint and bucket")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1" // arbitrary but must be non-empty for the signer
	}
	scheme := "https://"
	if cfg.Insecure {
		scheme = "http://"
	}
	client := s3.New(s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		BaseEndpoint: aws.String(scheme + cfg.Endpoint),
		UsePathStyle: true, // S3-compatible servers rarely do virtual-host style
	})
	return &S3{c: client, bucket: cfg.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
		Body:   r,
	})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (s *S3) Has(ctx context.Context, key string) (bool, error) {
	_, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *S3) Delete(ctx context.Context, key string) error {
	// DeleteObject is idempotent — deleting a missing key is not an error.
	_, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return err
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	return false
}
