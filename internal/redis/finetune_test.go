package redis

import (
	"context"
	"errors"
	"testing"
)

func TestFineTuneCancelRequestAllowsOnlyConfirmedTerminalOutcome(t *testing.T) {
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()
	const org, node = "org-a", "1297"
	for _, id := range []string{"cleanup-ok", "cleanup-failed"} {
		if err := raw.HSet(ctx, "finetune:job:"+id, map[string]any{
			"org_id": org, "node_id": node, "status": "cancelling",
		}).Err(); err != nil {
			t.Fatal(err)
		}
		if err := raw.Set(ctx, "cancel:"+id, "1", 0).Err(); err != nil {
			t.Fatal(err)
		}
		cancelled, err := client.FineTuneCancelled(ctx, id, org, node)
		if err != nil || !cancelled {
			t.Fatalf("pending cancellation not visible: %v %v", cancelled, err)
		}
		if err := raw.Del(ctx, "cancel:"+id).Err(); err != nil {
			t.Fatal(err)
		}
		cancelled, err = client.FineTuneCancelled(ctx, id, org, node)
		if err != nil || !cancelled {
			t.Fatalf("durable cancellation status lost after signal expiry: %v %v", cancelled, err)
		}
		if err := raw.Set(ctx, "cancel:"+id, "1", 0).Err(); err != nil {
			t.Fatal(err)
		}
		if err := client.FineTuneUpdate(ctx, id, org, node, map[string]any{"status": "succeeded"}); !errors.Is(err, ErrFineTuneCancelled) {
			t.Fatalf("success raced cancellation: %v", err)
		}
		if err := client.FineTuneUpdate(ctx, id, "other-org", node, map[string]any{"status": "cancelled"}); err == nil {
			t.Fatal("cross-org terminal write accepted")
		}
		if err := client.FineTuneFailCritical(ctx, id, org, "other-node", map[string]any{"status": "failed", "error": "bad", "finished_at": "now"}); err == nil {
			t.Fatal("wrong-node critical failure accepted")
		}
	}
	confirmed := map[string]any{"status": "cancelled", "finished_at": "2026-09-24T19:30:00Z"}
	if err := client.FineTuneUpdate(ctx, "cleanup-ok", org, node, confirmed); err != nil {
		t.Fatal(err)
	}
	if err := client.FineTuneUpdate(ctx, "cleanup-ok", org, node, confirmed); err != nil {
		t.Fatalf("identical cancelled retry failed: %v", err)
	}
	if got := raw.HGet(ctx, "finetune:job:cleanup-ok", "status").Val(); got != "cancelled" {
		t.Fatalf("confirmed status = %q", got)
	}
	critical := map[string]any{"status": "failed", "error": "container termination unconfirmed", "finished_at": "2026-09-24T19:30:00Z"}
	if err := client.FineTuneUpdate(ctx, "cleanup-failed", org, node, critical); !errors.Is(err, ErrFineTuneCancelled) {
		t.Fatalf("ordinary failure crossed cancel fence: %v", err)
	}
	if err := client.FineTuneFailCritical(ctx, "cleanup-failed", org, node, critical); err != nil {
		t.Fatal(err)
	}
	if err := client.FineTuneFailCritical(ctx, "cleanup-failed", org, node, critical); err != nil {
		t.Fatalf("identical critical retry failed: %v", err)
	}
	if got := raw.HGet(ctx, "finetune:job:cleanup-failed", "status").Val(); got != "failed" {
		t.Fatalf("critical failure status = %q", got)
	}
	if err := client.FineTuneUpdate(ctx, "cleanup-failed", org, node, confirmed); err == nil {
		t.Fatal("terminal failure rewritten to cancelled")
	}
}
