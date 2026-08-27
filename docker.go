package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const socketPath = "/var/run/docker.sock"

type Client struct {
	http *http.Client
	api  string
}

type versionResponse struct {
	APIVersion string `json:"ApiVersion"`
}

type createResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

func Available() bool {
	st, err := os.Stat(socketPath)
	return err == nil && !st.IsDir()
}

func NewClient() (*Client, error) {
	if !Available() {
		return nil, errors.New("Docker Socket 未挂载；请使用新版 Compose 启用 Web 更新")
	}
	tr := &http.Transport{}
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socketPath)
	}
	c := &Client{http: &http.Client{Transport: tr, Timeout: 15 * time.Minute}}
	var v versionResponse
	if err := c.do(context.Background(), http.MethodGet, "/version", nil, &v, false); err != nil {
		return nil, fmt.Errorf("连接 Docker Engine 失败: %w", err)
	}
	if v.APIVersion == "" {
		return nil, errors.New("Docker Engine 未返回 API 版本")
	}
	c.api = "/v" + v.APIVersion
	return c, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, versioned bool) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	if versioned {
		path = c.api + path
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		return fmt.Errorf("Docker API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func splitImage(image string) (string, string) {
	tag := "latest"
	name := image
	if at := strings.Index(name, "@"); at >= 0 {
		return name, ""
	}
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon > lastSlash {
		tag = name[lastColon+1:]
		name = name[:lastColon]
	}
	return name, tag
}

func (c *Client) PullImage(ctx context.Context, image string) error {
	name, tag := splitImage(image)
	q := url.Values{}
	q.Set("fromImage", name)
	if tag != "" {
		q.Set("tag", tag)
	}
	path := "/images/create?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+c.api+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		return fmt.Errorf("拉取镜像失败 %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if e, ok := msg["error"].(string); ok && e != "" {
			return errors.New(e)
		}
	}
	return nil
}

func (c *Client) TriggerUpdate(ctx context.Context, targetName, image string) error {
	if targetName == "" {
		targetName = "bestip-manager"
	}
	if image == "" {
		return errors.New("更新镜像地址为空")
	}
	if err := c.PullImage(ctx, image); err != nil {
		return err
	}

	helperName := fmt.Sprintf("bestip-update-helper-%d", time.Now().Unix())
	body := map[string]any{
		"Image": image,
		"Env": []string{
			"BESTIP_UPDATE_HELPER=1",
			"BESTIP_UPDATE_TARGET=" + targetName,
			"BESTIP_UPDATE_IMAGE=" + image,
		},
		"HostConfig": map[string]any{
			"Binds":       []string{socketPath + ":" + socketPath},
			"AutoRemove":  true,
			"NetworkMode": "bridge",
		},
	}
	var cr createResponse
	if err := c.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(helperName), body, &cr, true); err != nil {
		return fmt.Errorf("创建更新助手失败: %w", err)
	}
	if cr.ID == "" {
		return errors.New("更新助手未返回容器 ID")
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+cr.ID+"/start", nil, nil, true); err != nil {
		return fmt.Errorf("启动更新助手失败: %w", err)
	}
	return nil
}

func RunHelper(ctx context.Context) error {
	target := os.Getenv("BESTIP_UPDATE_TARGET")
	image := os.Getenv("BESTIP_UPDATE_IMAGE")
	if target == "" || image == "" {
		return errors.New("更新助手缺少目标容器或镜像参数")
	}
	c, err := NewClient()
	if err != nil {
		return err
	}
	// Give the initiating HTTP request time to return before the manager is replaced.
	time.Sleep(2 * time.Second)
	return c.replaceContainer(ctx, target, image)
}

func (c *Client) replaceContainer(ctx context.Context, target, image string) error {
	var inspect map[string]any
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(target)+"/json", nil, &inspect, true); err != nil {
		return fmt.Errorf("读取当前容器配置失败: %w", err)
	}
	oldID, _ := inspect["Id"].(string)
	config, _ := inspect["Config"].(map[string]any)
	hostConfig, _ := inspect["HostConfig"].(map[string]any)
	if oldID == "" || config == nil || hostConfig == nil {
		return errors.New("当前容器配置不完整，已取消更新")
	}

	backup := fmt.Sprintf("%s-backup-%d", target, time.Now().Unix())
	if err := c.do(ctx, http.MethodPost, "/containers/"+oldID+"/rename?name="+url.QueryEscape(backup), nil, nil, true); err != nil {
		return fmt.Errorf("创建回退点失败: %w", err)
	}

	rollback := func(cause error) error {
		_ = c.do(context.Background(), http.MethodPost, "/containers/"+oldID+"/rename?name="+url.QueryEscape(target), nil, nil, true)
		_ = c.do(context.Background(), http.MethodPost, "/containers/"+oldID+"/start", nil, nil, true)
		return cause
	}

	_ = c.do(ctx, http.MethodPost, "/containers/"+oldID+"/stop?t=10", nil, nil, true)

	// Recreate from the inspected configuration so bind mounts, ports, restart policy,
	// environment and the Docker socket survive the update.
	config["Image"] = image
	delete(config, "Hostname")
	delete(config, "Domainname")
	config["HostConfig"] = hostConfig
	var cr createResponse
	if err := c.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(target), config, &cr, true); err != nil {
		return rollback(fmt.Errorf("创建新版本容器失败: %w", err))
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+cr.ID+"/start", nil, nil, true); err != nil {
		_ = c.do(context.Background(), http.MethodDelete, "/containers/"+cr.ID+"?force=true", nil, nil, true)
		return rollback(fmt.Errorf("启动新版本容器失败: %w", err))
	}
	// New container is healthy enough to start; the old stopped backup is no longer needed.
	_ = c.do(context.Background(), http.MethodDelete, "/containers/"+oldID+"?force=true", nil, nil, true)
	return nil
}
