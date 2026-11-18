package storage

import (
	"context"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestPathStyleEndpointIsTheConfiguredHostAndBucket(t *testing.T) {
	base, err := url.Parse("https://objects.example:9000")
	if err != nil {
		t.Fatal(err)
	}
	resolver := pathStyleEndpoint{base: *base}

	got, err := resolver.ResolveEndpoint(context.Background(), s3.EndpointParameters{
		Bucket: aws.String("cache"),
		Region: aws.String("anywhere"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://objects.example:9000/cache"; got.URI.String() != want {
		t.Fatalf("endpoint = %s want %s", got.URI.String(), want)
	}
	if base.Path != "" {
		t.Fatalf("resolving wrote %q into the shared base URL", base.Path)
	}
	if _, err := resolver.ResolveEndpoint(context.Background(), s3.EndpointParameters{}); err == nil {
		t.Fatal("a request without a bucket resolved to an endpoint")
	}
}
