package storetest

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"

	"github.com/ndlib/bendo/store"
)

func getSession() *session.Session {
	// This config is for a local hosted Minio.
	// Is there a better way to do this configuration?
	s3Config := &aws.Config{
		Endpoint:         aws.String("http://localhost:9000"),
		Region:           aws.String("us-east-1"),
		DisableSSL:       aws.Bool(true),
		S3ForcePathStyle: aws.Bool(true),
	}
	return session.New(s3Config)
}

func TestS3Stress(t *testing.T) {
	s := store.NewS3("zoo", "", getSession())
	Stress(t, s, 0)
}
