package capabilities

import "testing"

// TestEmbeddingTagQueue verifies the routing contract for embedding jobs
// (issue #351): a node carrying the `task:embedding` capability tag must
// subscribe to the `jobs:v1:tag:task:embedding` Redis Streams queue. Queue
// subscription is dynamic (derived from capability tags via ResolveQueues),
// so there is no static queue list to edit — once a node installs the TEI
// service it gains the tag (per #343 PR A serviceTags) and auto-subscribes.
func TestEmbeddingTagQueue(t *testing.T) {
	const embeddingTag = "task:embedding"
	const embeddingQueue = "jobs:v1:tag:task:embedding"

	if got := TagQueueName(embeddingTag); got != embeddingQueue {
		t.Fatalf("TagQueueName(%q) = %q, want %q", embeddingTag, got, embeddingQueue)
	}

	// A TEI node typically carries engine + task + model tags.
	caps := []Capability{
		{Tag: "engine:tei", Category: "engine"},
		{Tag: embeddingTag, Category: "task"},
		{Tag: "model:gte-multilingual-base", Category: "model"},
	}
	queues := ResolveQueues(caps, "jobs:v1:gpu-general")

	found := false
	for _, q := range queues {
		if q == embeddingQueue {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ResolveQueues did not include %q; got %v", embeddingQueue, queues)
	}
}
