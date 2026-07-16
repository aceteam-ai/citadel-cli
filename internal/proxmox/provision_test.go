package proxmox

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNextID(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/nextid" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":"105"}`))
	})
	id, err := client.NextID(context.Background())
	if err != nil {
		t.Fatalf("NextID: %v", err)
	}
	if id != 105 {
		t.Errorf("expected 105, got %d", id)
	}
}

func TestCloneVM(t *testing.T) {
	var gotForm url.Values
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/9000/clone" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		r.ParseForm()
		gotForm = r.PostForm
		w.Write([]byte(`{"data":"UPID:pve1:0001:clone:"}`))
	})

	upid, err := client.CloneVM(context.Background(), "pve1", 9000, 105, CloneOptions{
		Name: "test-vm", Full: true, Storage: "local-lvm", Pool: "aceteam-org-abc",
	})
	if err != nil {
		t.Fatalf("CloneVM: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:") {
		t.Errorf("expected UPID, got %q", upid)
	}
	for k, want := range map[string]string{
		"newid": "105", "name": "test-vm", "full": "1",
		"storage": "local-lvm", "pool": "aceteam-org-abc",
	} {
		if got := gotForm.Get(k); got != want {
			t.Errorf("form[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestConfigureVM(t *testing.T) {
	var gotForm url.Values
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/105/config" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		r.ParseForm()
		gotForm = r.PostForm
		w.Write([]byte(`{"data":null}`))
	})

	err := client.ConfigureVM(context.Background(), "pve1", 105, map[string]string{
		"cores": "2", "memory": "4096", "cicustom": "user=local:snippets/aceteam-x.yaml",
	})
	if err != nil {
		t.Fatalf("ConfigureVM: %v", err)
	}
	if gotForm.Get("cores") != "2" || gotForm.Get("memory") != "4096" {
		t.Errorf("sizing params not sent: %v", gotForm)
	}
	if gotForm.Get("cicustom") != "user=local:snippets/aceteam-x.yaml" {
		t.Errorf("cicustom not sent: %v", gotForm)
	}
}

func TestResizeDisk(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/105/resize" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		r.ParseForm()
		if r.PostForm.Get("disk") != "scsi0" || r.PostForm.Get("size") != "40G" {
			t.Errorf("unexpected form: %v", r.PostForm)
		}
		w.Write([]byte(`{"data":null}`))
	})
	if err := client.ResizeDisk(context.Background(), "pve1", 105, "scsi0", "40G"); err != nil {
		t.Fatalf("ResizeDisk: %v", err)
	}
}

func TestDeleteVM(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/105" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("purge") != "1" {
			t.Errorf("expected purge=1, got %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"data":"UPID:pve1:0002:delete:"}`))
	})
	upid, err := client.DeleteVM(context.Background(), "pve1", 105, true)
	if err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:") {
		t.Errorf("expected UPID, got %q", upid)
	}
}

func TestWaitForTask(t *testing.T) {
	orig := taskPollInterval
	taskPollInterval = time.Millisecond
	defer func() { taskPollInterval = orig }()

	calls := 0
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.Write([]byte(`{"data":{"status":"running","upid":"UPID:x"}}`))
			return
		}
		w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK","upid":"UPID:x"}}`))
	})
	if err := client.WaitForTask(context.Background(), "pve1", "UPID:x", time.Second); err != nil {
		t.Fatalf("WaitForTask: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 polls, got %d", calls)
	}
}

func TestWaitForTaskFailure(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"clone failed: no space","upid":"UPID:x"}}`))
	})
	err := client.WaitForTask(context.Background(), "pve1", "UPID:x", time.Second)
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("expected task failure error, got %v", err)
	}
}

func TestEnsurePool(t *testing.T) {
	created := false
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/pools":
			w.Write([]byte(`{"data":[{"poolid":"existing"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/pools":
			r.ParseForm()
			if r.PostForm.Get("poolid") != "aceteam-org-abc" {
				t.Errorf("unexpected poolid: %v", r.PostForm)
			}
			created = true
			w.Write([]byte(`{"data":null}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	didCreate, err := client.EnsurePool(context.Background(), "aceteam-org-abc", "test")
	if err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	if !didCreate || !created {
		t.Error("expected pool to be created")
	}

	// Existing pool: no create call.
	didCreate, err = client.EnsurePool(context.Background(), "existing", "test")
	if err != nil {
		t.Fatalf("EnsurePool existing: %v", err)
	}
	if didCreate {
		t.Error("expected no create for existing pool")
	}
}

func TestGetPoolMembers(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/pools/aceteam-org-abc" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":{"members":[{"id":"qemu/105","type":"qemu","vmid":105,"node":"pve1"}]}}`))
	})
	members, err := client.GetPoolMembers(context.Background(), "aceteam-org-abc")
	if err != nil {
		t.Fatalf("GetPoolMembers: %v", err)
	}
	if len(members) != 1 || members[0].VMID != 105 {
		t.Errorf("unexpected members: %+v", members)
	}
}
