package worker

import (
	"context"
	"testing"
)

func TestRedisStreamWriterImplementsStreamWriter(t *testing.T) {
	var _ StreamWriter = (*RedisStreamWriter)(nil)
}

func TestCreateRedisStreamWriterFactory(t *testing.T) {
	ctx := context.Background()
	source := NewRedisSource(RedisSourceConfig{
		URL: "redis://localhost:6379",
	})

	factory := CreateRedisStreamWriterFactory(ctx, source)

	if factory == nil {
		t.Fatal("CreateRedisStreamWriterFactory returned nil")
	}

	// Factory should return a StreamWriter
	job := &Job{ID: "test-job-id", RayID: "ray-test-123"}
	writer := factory(job)
	if writer == nil {
		t.Error("Factory returned nil StreamWriter")
	}

	// Verify it's the right type
	rsw, ok := writer.(*RedisStreamWriter)
	if !ok {
		t.Error("Factory should return *RedisStreamWriter")
	}

	if rsw.jobID != "test-job-id" {
		t.Errorf("jobID = %v, want test-job-id", rsw.jobID)
	}
	if rsw.rayID != "ray-test-123" {
		t.Errorf("rayID = %v, want ray-test-123", rsw.rayID)
	}
}

func TestNewRedisStreamWriter(t *testing.T) {
	ctx := context.Background()
	source := NewRedisSource(RedisSourceConfig{
		URL: "redis://localhost:6379",
	})

	writer := NewRedisStreamWriter(ctx, source.Client(), "job-123", "ray-456")

	if writer == nil {
		t.Fatal("NewRedisStreamWriter returned nil")
	}
	if writer.jobID != "job-123" {
		t.Errorf("jobID = %v, want job-123", writer.jobID)
	}
	if writer.rayID != "ray-456" {
		t.Errorf("rayID = %v, want ray-456", writer.rayID)
	}
	if writer.ctx != ctx {
		t.Error("ctx not set correctly")
	}
}
