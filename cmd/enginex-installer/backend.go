package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

func setupBackend(ctx context.Context, o *options) error {
	reader := bufio.NewReader(o.In)
	ask := func(zh, en string) (string, error) {
		o.say(zh, en)
		line, err := reader.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	address, err := ask("已刷好 EngineX 的碟机地址（如 http://192.168.1.20:8080）：", "Running EngineX deck address (for example http://192.168.1.20:8080):")
	if err != nil {
		return err
	}
	if !strings.Contains(address, "://") {
		address = strings.TrimRight(address, "/")
		if _, _, err := net.SplitHostPort(address); err != nil {
			address = net.JoinHostPort(strings.Trim(address, "[]"), "8080")
		}
		address = "http://" + address
	}
	if !stems.ValidEndpoint(address) {
		return errors.New("deck address must be HTTPS or HTTP on the local network")
	}
	u, _ := url.Parse(address)
	if u.Path != "" && u.Path != "/" {
		return errors.New("enter the deck console address without a path")
	}
	code, err := ask("输入碟机屏幕 设置 → 关于 / 更新 中的控制台访问码：", "Console Access Code from Settings → About / Update on the deck:")
	if err != nil {
		return err
	}
	client := *o.HTTP
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	call := func(method string, body io.Reader) (string, error) {
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(address, "/")+"/api/relay", body)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+code)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("deck backend setup: HTTP %d; check the console code and address", resp.StatusCode)
		}
		var status struct {
			RelayURL string `json:"relayUrl"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&status); err != nil {
			return "", err
		}
		return status.RelayURL, nil
	}
	saved, err := call("GET", nil)
	if err != nil {
		return err
	}
	backend, err := ask("后端地址（回车保留 "+saved+"）：", "Backend URL (Enter keeps "+saved+"):")
	if err != nil {
		return err
	}
	if backend == "" {
		backend = saved
	}
	if backend == "" || !stems.ValidEndpoint(backend) {
		return errors.New("backend address must be HTTPS or HTTP on a private network")
	}
	password, err := ask("后端控制台密码（地址未变时回车可保留已保存的密码）：", "Backend console password (Enter keeps the saved password if the address is unchanged):")
	if err != nil {
		return err
	}
	if backend != saved && password == "" {
		return errors.New("enter the password for the new backend")
	}
	payload := map[string]string{"relayUrl": backend}
	if password != "" {
		payload["deemixPassword"] = password
	}
	data, _ := json.Marshal(payload)
	if _, err := call("POST", strings.NewReader(string(data))); err != nil {
		return err
	}
	o.say("后端地址已保存。请用碟机屏幕上的二维码登录流媒体。", "Backend saved. Use the QR codes on the deck screen to sign in to streaming services.")
	return nil
}
