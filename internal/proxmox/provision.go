// internal/proxmox/provision.go
//
// Provisioning-oriented Proxmox VE API methods (aceteam#5963): clone from a
// cloud-init template, size the VM, manage lifecycle, and group instances into
// per-org resource pools. These extend the read-mostly Client in client.go with
// the mutating calls the fabric INSTANCE_* job family needs.
//
// All methods speak the standard PVE REST API (/api2/json) with token auth.
// Long-running operations (clone, delete) return a UPID task id; WaitForTask
// polls the task endpoint until it finishes.
package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// put performs an authenticated PUT request with form data.
func (c *Client) put(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	var body = strings.NewReader(form.Encode())
	return c.doRequest(ctx, http.MethodPut, path, body)
}

// delete performs an authenticated DELETE request.
func (c *Client) delete(ctx context.Context, path string) (json.RawMessage, error) {
	return c.doRequest(ctx, http.MethodDelete, path, nil)
}

// NextID returns the next free VMID in the cluster.
func (c *Client) NextID(ctx context.Context) (int, error) {
	data, err := c.get(ctx, "/cluster/nextid")
	if err != nil {
		return 0, err
	}
	// PVE returns the id as a JSON string (e.g. "105").
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		// Some versions return a bare number.
		var n int
		if err2 := json.Unmarshal(data, &n); err2 == nil {
			return n, nil
		}
		return 0, fmt.Errorf("parsing nextid: %w", err)
	}
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parsing nextid %q: %w", s, err)
	}
	return id, nil
}

// CloneOptions holds optional parameters for CloneVM.
type CloneOptions struct {
	// Name is the new VM's name (DNS-safe).
	Name string
	// Full requests a full (non-linked) clone.
	Full bool
	// Storage is the target storage for a full clone (optional).
	Storage string
	// Pool assigns the new VM to a resource pool at creation (optional).
	Pool string
}

// CloneVM clones srcVMID on the given node into newVMID. Returns the UPID of
// the asynchronous clone task.
func (c *Client) CloneVM(ctx context.Context, node string, srcVMID, newVMID int, opts CloneOptions) (string, error) {
	form := url.Values{"newid": {strconv.Itoa(newVMID)}}
	if opts.Name != "" {
		form.Set("name", opts.Name)
	}
	if opts.Full {
		form.Set("full", "1")
	}
	if opts.Storage != "" {
		form.Set("storage", opts.Storage)
	}
	if opts.Pool != "" {
		form.Set("pool", opts.Pool)
	}
	data, err := c.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, srcVMID), form)
	if err != nil {
		return "", err
	}
	return parseUPID(data)
}

// ConfigureVM applies configuration key/values to a QEMU VM (cores, memory,
// cloud-init fields such as cicustom/ipconfig0, tags, ...).
func (c *Client) ConfigureVM(ctx context.Context, node string, vmid int, params map[string]string) error {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	_, err := c.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), form)
	return err
}

// ResizeDisk grows a VM disk to the given absolute size (e.g. "40G").
// Proxmox only grows disks; shrinking is rejected by the API.
func (c *Client) ResizeDisk(ctx context.Context, node string, vmid int, disk, size string) error {
	form := url.Values{"disk": {disk}, "size": {size}}
	_, err := c.put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), form)
	return err
}

// DeleteVM destroys a VM and (with purge) all its references (backups, replication
// jobs) and unreferenced disks. Returns the UPID of the asynchronous delete task.
func (c *Client) DeleteVM(ctx context.Context, node string, vmid int, purge bool) (string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid)
	if purge {
		path += "?purge=1&destroy-unreferenced-disks=1"
	}
	data, err := c.delete(ctx, path)
	if err != nil {
		return "", err
	}
	return parseUPID(data)
}

// TaskStatus describes the state of an asynchronous PVE task.
type TaskStatus struct {
	Status     string `json:"status"`     // "running" | "stopped"
	ExitStatus string `json:"exitstatus"` // "OK" on success (set once stopped)
	UPID       string `json:"upid"`
	Type       string `json:"type"`
}

// GetTaskStatus returns the current status of a task by UPID.
func (c *Client) GetTaskStatus(ctx context.Context, node, upid string) (*TaskStatus, error) {
	data, err := c.get(ctx, fmt.Sprintf("/nodes/%s/tasks/%s/status", node, url.PathEscape(upid)))
	if err != nil {
		return nil, err
	}
	var ts TaskStatus
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("parsing task status: %w", err)
	}
	return &ts, nil
}

// WaitForTask polls a task until it stops or the context/timeout expires.
// A task that stops with an exit status other than "OK" is an error.
func (c *Client) WaitForTask(ctx context.Context, node, upid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ts, err := c.GetTaskStatus(ctx, node, upid)
		if err != nil {
			return fmt.Errorf("polling task %s: %w", upid, err)
		}
		if ts.Status == "stopped" {
			if ts.ExitStatus != "OK" {
				return fmt.Errorf("task %s failed: %s", upid, ts.ExitStatus)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s timed out after %s", upid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(taskPollInterval):
		}
	}
}

// taskPollInterval is how often WaitForTask polls. Overridable in tests.
var taskPollInterval = 2 * time.Second

// Pool represents a PVE resource pool.
type Pool struct {
	PoolID  string `json:"poolid"`
	Comment string `json:"comment,omitempty"`
}

// PoolMember is a guest or storage assigned to a pool.
type PoolMember struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "qemu" | "lxc" | "storage"
	VMID int    `json:"vmid,omitempty"`
	Node string `json:"node,omitempty"`
}

// poolDetail is the response shape of GET /pools/{poolid}.
type poolDetail struct {
	Comment string       `json:"comment,omitempty"`
	Members []PoolMember `json:"members"`
}

// ListPools returns all resource pools in the cluster.
func (c *Client) ListPools(ctx context.Context) ([]Pool, error) {
	data, err := c.get(ctx, "/pools")
	if err != nil {
		return nil, err
	}
	var pools []Pool
	if err := json.Unmarshal(data, &pools); err != nil {
		return nil, fmt.Errorf("parsing pools: %w", err)
	}
	return pools, nil
}

// CreatePool creates a resource pool.
func (c *Client) CreatePool(ctx context.Context, poolID, comment string) error {
	form := url.Values{"poolid": {poolID}}
	if comment != "" {
		form.Set("comment", comment)
	}
	_, err := c.post(ctx, "/pools", form)
	return err
}

// GetPoolMembers returns the members of a pool.
func (c *Client) GetPoolMembers(ctx context.Context, poolID string) ([]PoolMember, error) {
	data, err := c.get(ctx, fmt.Sprintf("/pools/%s", url.PathEscape(poolID)))
	if err != nil {
		return nil, err
	}
	var detail poolDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return nil, fmt.Errorf("parsing pool detail: %w", err)
	}
	return detail.Members, nil
}

// EnsurePool creates the pool if it does not already exist and returns whether
// it was created.
func (c *Client) EnsurePool(ctx context.Context, poolID, comment string) (bool, error) {
	pools, err := c.ListPools(ctx)
	if err != nil {
		return false, fmt.Errorf("listing pools: %w", err)
	}
	for _, p := range pools {
		if p.PoolID == poolID {
			return false, nil
		}
	}
	if err := c.CreatePool(ctx, poolID, comment); err != nil {
		return false, fmt.Errorf("creating pool %s: %w", poolID, err)
	}
	return true, nil
}

// parseUPID extracts a task UPID from a raw API response.
func parseUPID(data json.RawMessage) (string, error) {
	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return "", fmt.Errorf("parsing task UPID: %w", err)
	}
	if !strings.HasPrefix(upid, "UPID:") {
		return "", fmt.Errorf("unexpected task response %q", upid)
	}
	return upid, nil
}
