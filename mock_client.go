package main

import (
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"os"
)

type MockS3Client struct {
	Requests []*s3.PutObjectInput
	mockList []types.Object
}

func NewMockClient(mockedList []types.Object) *MockS3Client {
	return &MockS3Client{
		Requests: make([]*s3.PutObjectInput, 0),
		mockList: mockedList,
	}
}

func (s *MockS3Client) PutObject(request *s3.PutObjectInput, file *os.File) error {
	s.Requests = append(s.Requests, request)
	return nil
}

func (s *MockS3Client) ListObjects(string) (map[string]types.Object, error) {
	fakeMap := make(map[string]types.Object)
	return fakeMap, nil
}

func (s *MockS3Client) CopyObject(sourceBucket string, destinationBucket string, key string) error {
	return nil
}
func (s *MockS3Client) DeleteObject(bucket string, key string) error                  { return nil }
func (s *MockS3Client) UploadFile(bucketName string, key string, file *os.File) error { return nil }