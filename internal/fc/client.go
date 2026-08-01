// Package fc is a minimal client for the Firecracker VMM's HTTP API, which is
// served over a unix domain socket. Only the calls skiff needs are implemented,
// and every one of them is a PUT.
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct{ http *http.Client }

func New(socketPath string) *Client {
	return &Client{http: &http.Client{
		// A wedged VMM that accepted the connection but never answers must not hang
		// `run up` forever. Every call here is a local unix-socket PUT that Firecracker
		// answers in microseconds; 10 s is three orders of magnitude of headroom.
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}}
}

func (c *Client) put(path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, "http://fc"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker PUT %s: %s: %s", path, resp.Status, msg)
	}
	return nil
}

func (c *Client) MachineConfig(vcpus, memMiB int) error {
	return c.put("/machine-config", map[string]any{"vcpu_count": vcpus, "mem_size_mib": memMiB})
}

func (c *Client) BootSource(kernelPath, bootArgs string) error {
	return c.put("/boot-source", map[string]any{"kernel_image_path": kernelPath, "boot_args": bootArgs})
}

func (c *Client) Drive(id, imgPath string, readOnly bool) error {
	return c.put("/drives/"+id, map[string]any{
		"drive_id": id, "path_on_host": imgPath, "is_root_device": id == "rootfs", "is_read_only": readOnly,
	})
}

func (c *Client) NetIface(id, tapName, guestMAC string) error {
	return c.put("/network-interfaces/"+id, map[string]any{
		"iface_id": id, "host_dev_name": tapName, "guest_mac": guestMAC,
	})
}

func (c *Client) Start() error {
	return c.put("/actions", map[string]any{"action_type": "InstanceStart"})
}
